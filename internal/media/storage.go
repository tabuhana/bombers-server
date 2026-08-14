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

	// The generic object surface, for bytes that aren't a user's media kind —
	// today, an activity's assets (sprites, audio) which belong to a published
	// GAME rather than to a person. Same bucket, same authenticated
	// pass-through rule; only the key namespace differs.
	PutObject(ctx context.Context, key string, data []byte, contentType string) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
	RemovePrefix(ctx context.Context, prefix string) error
	// ListObjects enumerates keys under a prefix. Backup only — nothing in the
	// serving path lists, because every read there is by exact key.
	ListObjects(ctx context.Context, prefix string) ([]string, error)
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

// PutObject writes (or overwrites) an object at an explicit key.
func (s *Storage) PutObject(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key,
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

// GetObject opens an object by key. A missing object is ErrObjectNotFound.
func (s *Storage) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}
	// minio-go defers the request, so a missing object only surfaces on the
	// first read/stat — check now so callers get a clean error.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("stat object: %w", err)
	}
	return obj, nil
}

// RemovePrefix deletes every object under a key prefix (an activity's whole
// asset folder when it's unpublished or republished).
func (s *Storage) RemovePrefix(ctx context.Context, prefix string) error {
	objects := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	for obj := range objects {
		if obj.Err != nil {
			return fmt.Errorf("list objects: %w", obj.Err)
		}
		if err := s.client.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("remove object: %w", err)
		}
	}
	return nil
}

// ListObjects returns every key under a prefix (pass "" for the whole bucket).
//
// Only backup needs this, and it needs it for the obvious reason: you cannot
// copy what you cannot enumerate. Nothing in the serving path lists — every
// read here is by exact key — so this stays out of the hot path entirely.
func (s *Storage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list objects: %w", obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
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
