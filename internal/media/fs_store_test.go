package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// TestFSStore exercises the filesystem backend through the Store interface: the
// put -> get (bytes match) -> delete -> get (not-found) round-trip, a get on a
// never-written key, and idempotent delete. Both missing-object reads must
// surface ErrObjectNotFound — the same not-found signal the S3 backend returns,
// which is the contract the handler relies on.
func TestFSStore(t *testing.T) {
	// Compile-time proof the FS backend satisfies the seam the handlers depend on.
	var _ Store = (*FSStore)(nil)

	// An empty dir is rejected up front.
	if _, err := NewFSStore(""); err == nil {
		t.Fatal("NewFSStore(\"\"): want error for empty dir, got nil")
	}

	ctx := context.Background()
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}

	if err := store.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	const (
		userID = "user-123"
		kind   = "avatar"
	)
	want := []byte("the-raw-image-bytes")

	// A read before any write is a not-found.
	if _, err := store.Get(ctx, userID, kind); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("Get before Put: want ErrObjectNotFound, got %v", err)
	}

	// Put then Get returns the exact bytes.
	if err := store.Put(ctx, userID, kind, want, "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := store.Get(ctx, userID, kind)
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	got, readErr := io.ReadAll(rc)
	closeErr := rc.Close()
	if readErr != nil {
		t.Fatalf("read object: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close object: %v", closeErr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("bytes mismatch: got %q, want %q", got, want)
	}

	// Delete, then Get is a not-found again.
	if err := store.Remove(ctx, userID, kind); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := store.Get(ctx, userID, kind); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("Get after Remove: want ErrObjectNotFound, got %v", err)
	}

	// Delete of an already-absent key is a successful no-op (idempotent).
	if err := store.Remove(ctx, userID, kind); err != nil {
		t.Fatalf("Remove idempotent: %v", err)
	}
}
