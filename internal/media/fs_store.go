package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FSStore is a filesystem-backed media Store: raw bytes on local disk, no S3,
// no daemon, no network — the local self-host default (LOCAL_MODE.md §6). Media
// is just blobs and the filesystem is blob storage, so this is ~a hundred lines
// with nothing to download. Objects live at <dir>/users/<userID>/<kind>, the
// same key scheme the S3 backend uses (objectKey), so the two are drop-in
// interchangeable behind Store. Metadata (content type, size, updated_at) lives
// in the user_media table, so this layer is dumb bytes: it never reads or writes
// a content type.
type FSStore struct {
	dir string
}

// NewFSStore roots a filesystem store at dir, creating it (and any parents) if
// absent. A non-empty dir is required — an empty one would scatter media at the
// process's working directory. Mirrors NewStorage: called once at startup,
// failure is fatal there.
func NewFSStore(dir string) (*FSStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("media: filesystem store requires a non-empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create media dir %q: %w", dir, err)
	}
	return &FSStore{dir: dir}, nil
}

// path maps a (user, kind) to the on-disk object path, reusing objectKey so the
// filesystem layout matches the S3 key scheme exactly (users/<id>/<kind>).
// filepath.Join cleans the forward-slash key into OS-native separators.
func (s *FSStore) path(userID, kind string) string {
	return filepath.Join(s.dir, objectKey(userID, kind))
}

// Put writes (or replaces) a user's media object atomically: it writes to a
// temp file in the destination directory, then renames it over the target, so a
// crash mid-write can never leave a half-written object where the next Get would
// read it. The temp lives in the SAME directory as the target so the rename is a
// cheap in-place move, not a cross-filesystem copy. contentType is intentionally
// ignored — the user_media table owns metadata; the store persists raw bytes.
func (s *FSStore) Put(ctx context.Context, userID, kind string, data []byte, contentType string) error {
	target := s.path(userID, kind)
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create media dir: %w", err)
	}

	tmp, err := os.CreateTemp(parent, "."+kind+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp media file: %w", err)
	}
	tmpName := tmp.Name()
	// If anything below fails (or after a successful rename, when tmpName no
	// longer exists), don't leave the temp file behind — a stale temp is
	// harmless (Get only opens the exact <kind> name) but shouldn't accumulate.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp media file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp media file: %w", err)
	}
	// CreateTemp makes the file 0600; media files are world-readable 0644.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp media file: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("finalize media file: %w", err)
	}
	return nil
}

// Get opens a user's media object for streaming. A missing file maps to
// ErrObjectNotFound — the same sentinel the S3 backend returns — so the handler
// sees an identical not-found signal regardless of backend. The caller must
// Close the returned reader (*os.File satisfies io.ReadCloser).
func (s *FSStore) Get(ctx context.Context, userID, kind string) (io.ReadCloser, error) {
	f, err := os.Open(s.path(userID, kind))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("open media file: %w", err)
	}
	return f, nil
}

// Remove deletes a user's media object. A missing file is a successful no-op,
// matching S3's idempotent delete (and the DELETE endpoint's contract).
func (s *FSStore) Remove(ctx context.Context, userID, kind string) error {
	if err := os.Remove(s.path(userID, kind)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove media file: %w", err)
	}
	return nil
}

// Ping reports whether the store is usable — the /health "media" signal, the
// filesystem analog of the S3 backend's BucketExists. The constructor already
// created dir; re-stat it so a later deletion or a bad mount surfaces as
// media: down. Cheap enough to run on every health poll (no write), and the
// media field is informational — the DB still governs overall status.
func (s *FSStore) Ping(ctx context.Context) error {
	info, err := os.Stat(s.dir)
	if err != nil {
		return fmt.Errorf("stat media dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("media dir %q is not a directory", s.dir)
	}
	return nil
}

// ── the generic object surface (activity assets) ─────────────────────────────
//
// Same key scheme as S3, so the two backends stay drop-in interchangeable: the
// key is a forward-slash path, cleaned into OS separators under the media dir.
// Keys are validated by the caller (see the activities domain) — this layer
// still refuses anything that would escape the root, because a path traversal
// through a published bundle would be the worst kind of bug.

func (s *FSStore) objectPath(key string) (string, error) {
	clean := filepath.Clean("/" + strings.ReplaceAll(key, "\\", "/"))
	if clean == "/" {
		return "", fmt.Errorf("media: empty object key")
	}
	return filepath.Join(s.dir, filepath.FromSlash(clean)), nil
}

// PutObject writes (or replaces) an object at an explicit key.
func (s *FSStore) PutObject(_ context.Context, key string, data []byte, _ string) error {
	target, err := s.objectPath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create object dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".obj.tmp-*")
	if err != nil {
		return fmt.Errorf("create temp object: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close object: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace object: %w", err)
	}
	return nil
}

// GetObject opens an object by key; a missing one is ErrObjectNotFound.
func (s *FSStore) GetObject(_ context.Context, key string) (io.ReadCloser, error) {
	target, err := s.objectPath(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("open object: %w", err)
	}
	return f, nil
}

// RemovePrefix deletes everything under a key prefix (an activity's assets).
// ListObjects walks the media directory and returns every key under a prefix,
// in the same slash-separated form the S3 store uses — a caller must not be
// able to tell which backend it got. Backup only; nothing in the serving path
// lists.
func (s *FSStore) ListObjects(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(s.dir, path)
		if rerr != nil {
			return rerr
		}
		key := filepath.ToSlash(rel)
		if prefix == "" || strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}
	return keys, nil
}

func (s *FSStore) RemovePrefix(_ context.Context, prefix string) error {
	target, err := s.objectPath(prefix)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove objects: %w", err)
	}
	return nil
}
