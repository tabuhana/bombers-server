package console

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// recordingStore records the prefixes it was asked to remove; fail makes chosen ones
// refuse. It satisfies objectStore and nothing else is called on it.
type recordingStore struct {
	removed []string
	fail    map[string]bool
}

func (s *recordingStore) Ping(context.Context) error { return nil }
func (s *recordingStore) PutObject(context.Context, string, []byte, string) error {
	return errors.New("not used")
}
func (s *recordingStore) RemovePrefix(_ context.Context, prefix string) error {
	s.removed = append(s.removed, prefix)
	if s.fail[prefix] {
		return errors.New("bucket unreachable")
	}
	return nil
}

// Deleting an account cascades its messages away in the database, where no handler
// sees it, so this is what keeps their pictures from being left in the store for ever.
func TestDeletingAnAccountRemovesEachOfItsPicturesAndCountsTheFailures(t *testing.T) {
	s := &recordingStore{fail: map[string]bool{"messages/m2/": true}}
	failed := removeMessageImages(context.Background(), s, []string{"m1", "m2", "m3"})

	if strings.Join(s.removed, ",") != "messages/m1/,messages/m2/,messages/m3/" {
		t.Errorf("removed = %v: one failure must not stop the rest", s.removed)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
}

func TestRemovingPicturesWithNoStoreOrNoPicturesIsANoOp(t *testing.T) {
	if failed := removeMessageImages(context.Background(), nil, []string{"m1"}); failed != 0 {
		t.Errorf("no store: failed = %d", failed)
	}
	s := &recordingStore{}
	if failed := removeMessageImages(context.Background(), s, nil); failed != 0 || len(s.removed) != 0 {
		t.Errorf("no pictures: failed %d removed %v", failed, s.removed)
	}
}

// Both sides of a conversation lose the message when either account goes, so the
// pictures to remove are the ones the user SENT and the ones they RECEIVED. A tombstone
// is not filtered out (its bytes may be what a failed delete left behind).
func TestSQLTheAccountsPicturesAreThoseItSentAndThoseItReceived(t *testing.T) {
	norm := strings.Join(strings.Fields(userImageIDsSQL), " ")
	for _, want := range []string{"FROM messages", "kind = 'image'", "sender_id = $1", "recipient_id = $1"} {
		if !strings.Contains(norm, want) {
			t.Errorf("userImageIDsSQL is missing %q: %s", want, norm)
		}
	}
	if strings.Contains(norm, "deleted_at") {
		t.Errorf("userImageIDsSQL must not skip tombstones: %s", norm)
	}
}

// The order inside deluser cannot be run without a database, so it is pinned as text:
// the names of the pictures are read BEFORE the delete (afterwards the rows that carry
// them are gone), and the bytes are removed only AFTER it (before it, a delete that
// failed would have destroyed pictures of messages that still exist).
func TestDeluserReadsThePicturesBeforeTheDeleteAndRemovesThemAfter(t *testing.T) {
	raw, err := os.ReadFile("admin.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	read := strings.Index(src, "userImageMessageIDs(ctx, c, id)")
	del := strings.Index(src, "DELETE FROM users WHERE id = $1")
	remove := strings.Index(src, "removeMessageImages(rmCtx, c.media, pictures)")
	// One call to it besides its definition: a second, earlier one is exactly the mistake.
	if n := strings.Count(src, "removeMessageImages("); n != 2 {
		t.Errorf("removeMessageImages appears %d times, want its definition and one call", n)
	}
	if read < 0 || del < 0 || remove < 0 || !(read < del && del < remove) {
		t.Errorf("read at %d, delete at %d, remove at %d: want read < delete < remove", read, del, remove)
	}
}
