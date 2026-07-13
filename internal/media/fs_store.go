package media

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
