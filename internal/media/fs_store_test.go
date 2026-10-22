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

// Removing a user's prefix takes every kind they have and nothing of anyone else's, in
// particular not a user whose id merely begins with the same characters.
func TestRemovingAUserPrefixTakesTheirMediaAndNoOneElses(t *testing.T) {
	ctx := context.Background()
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ user, kind string }{
		{"u1", "avatar"}, {"u1", "banner"}, {"u10", "avatar"}, {"u2", "banner"},
	} {
		if err := store.Put(ctx, c.user, c.kind, []byte(c.user+c.kind), "image/png"); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.RemovePrefix(ctx, UserPrefix("u1")); err != nil {
		t.Fatalf("RemovePrefix: %v", err)
	}
	for _, kind := range []string{"avatar", "banner"} {
		if _, err := store.Get(ctx, "u1", kind); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("u1 %s survived: %v", kind, err)
		}
	}
	for _, c := range []struct{ user, kind string }{{"u10", "avatar"}, {"u2", "banner"}} {
		rc, err := store.Get(ctx, c.user, c.kind)
		if err != nil {
			t.Errorf("%s %s was removed with u1's prefix: %v", c.user, c.kind, err)
			continue
		}
		_ = rc.Close()
	}
	// A user with nothing stored is a no-op.
	if err := store.RemovePrefix(ctx, UserPrefix("nobody")); err != nil {
		t.Errorf("a prefix that holds nothing: %v", err)
	}
}

// Every key a user's media is stored at is under their prefix, so removing the prefix
// cannot miss a kind.
func TestEveryMediaKeyIsUnderTheUsersPrefix(t *testing.T) {
	if got := UserPrefix("u1"); got != "users/u1/" {
		t.Errorf("UserPrefix = %q", got)
	}
	for _, kind := range []string{"avatar", "banner"} {
		if key := objectKey("u1", kind); key != "users/u1/"+kind {
			t.Errorf("objectKey(u1, %s) = %q", kind, key)
		}
	}
}
