// Package media owns profile media (avatar + banner): the object-storage
// layer, the upload/serve/delete endpoints, and the metadata table. Bytes live
// in S3-compatible object storage (MinIO locally; MinIO-on-VPS or Cloudflare
// R2 in prod — plain S3 API only, nothing AWS-proprietary); metadata lives in
// Postgres (user_media). Serving is a pass-through: the client always fetches
// through the server, which enforces friendship + profile visibility — the
// bucket stays private and is never linked directly.
package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Store is the media byte-I/O seam. The handlers and /health depend on this
// interface, not a concrete backend, so media bytes can live in S3-compatible
// object storage (Storage, below) or on the local filesystem (FSStore) with no
// handler change. The key scheme is server-constructed and identical across
// backends: users/<userID>/<kind>.
//
// The store is dumb bytes: metadata (content type, size, updated_at) lives in
// the user_media table, so the store never persists or returns a content type.
// Put still takes one (the S3 backend records it as object metadata); backends
// that don't need it ignore it. Get's missing-object signal is ErrObjectNotFound.
type Store interface {
	Put(ctx context.Context, userID, kind string, data []byte, contentType string) error
	Get(ctx context.Context, userID, kind string) (io.ReadCloser, error)
	Remove(ctx context.Context, userID, kind string) error
	Ping(ctx context.Context) error
}

// Storage is the thin S3 wrapper the handlers use. It speaks the plain S3 API
// via minio-go, so the same code points at local MinIO, MinIO-on-VPS, or R2 —
// only the endpoint/credentials in config change. It satisfies Store.
type Storage struct {
	client *minio.Client
	bucket string
}

// ErrObjectNotFound is returned by Get when no object exists at the key.
var ErrObjectNotFound = errors.New("object not found")

// NewStorage connects to the object store and ensures the bucket exists
// (creating it on first run, so local dev needs no manual MinIO setup).
// Mirrors store.NewPool: called once at startup, failure is fatal there.
func NewStorage(ctx context.Context, endpoint, accessKey, secretKey, bucket string, useSSL bool) (*Storage, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}

	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket %q: %w", bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("create bucket %q: %w", bucket, err)
		}
	}
	return &Storage{client: client, bucket: bucket}, nil
}

// objectKey derives where a user's media lives in the bucket. One fixed key
// per (user, kind) — a reupload overwrites in place, which is exactly the
// replace semantics the API promises.
func objectKey(userID, kind string) string {
	return fmt.Sprintf("users/%s/%s", userID, kind)
}

// Put writes (or overwrites) the object for a user's media kind.
func (s *Storage) Put(ctx context.Context, userID, kind string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, objectKey(userID, kind),
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

// Get opens the object for streaming. minio-go opens lazily, so a missing
// object only surfaces on Stat — normalized to ErrObjectNotFound here. The
// caller must Close the reader.
func (s *Storage) Get(ctx context.Context, userID, kind string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, objectKey(userID, kind), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("stat object: %w", err)
	}
	return obj, nil
}

// Remove deletes the object. Removing a nonexistent key is a no-op in S3, so
// this is naturally idempotent.
func (s *Storage) Remove(ctx context.Context, userID, kind string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, objectKey(userID, kind), minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("remove object: %w", err)
	}
	return nil
}

// Ping reports whether the object store is reachable (the /health "media"
// signal). BucketExists is the cheapest authenticated round-trip.
func (s *Storage) Ping(ctx context.Context) error {
	if _, err := s.client.BucketExists(ctx, s.bucket); err != nil {
		return fmt.Errorf("ping object store: %w", err)
	}
	return nil
}
