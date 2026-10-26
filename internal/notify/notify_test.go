package notify

import (
	"encoding/json"
	"testing"
	"time"
)

func newConn() *conn { return &conn{out: make(chan []byte, sendQueue)} }

// read one frame, or fail — nothing here should ever need to wait.
func read(t *testing.T, c *conn) Frame {
	t.Helper()
	select {
	case raw := <-c.out:
		var f Frame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		return f
	case <-time.After(time.Second):
		t.Fatal("expected a frame, got nothing")
		return Frame{}
	}
}

func TestSendReachesEveryConnectionAUserHasOpen(t *testing.T) {
	// Two windows, or a desktop and a laptop. Telling only one of them is how a
	// message appears on one machine and not the other.
	h := NewHub()
	a, b := newConn(), newConn()
	h.add("u1", a)
	h.add("u1", b)

	h.Send("u1", KindDM, nil)

	if got := read(t, a).T; got != KindDM {
		t.Fatalf("first connection got %q", got)
	}
	if got := read(t, b).T; got != KindDM {
		t.Fatalf("second connection got %q", got)
	}
}

func TestSendGoesOnlyToTheAddressedUser(t *testing.T) {
	h := NewHub()
	mine, theirs := newConn(), newConn()
	h.add("u1", mine)
	h.add("u2", theirs)

	h.Send("u1", KindDM, nil)

	read(t, mine)
	select {
	case <-theirs.out:
		t.Fatal("a nudge reached somebody it wasn't addressed to")
	default:
	}
}

func TestDataRidesAlong(t *testing.T) {
	h := NewHub()
	c := newConn()
	h.add("u1", c)

	h.Send("u1", KindDM, map[string]string{"from": "u9"})

	f := read(t, c)
	d, ok := f.D.(map[string]any)
	if !ok || d["from"] != "u9" {
		t.Fatalf("payload did not survive: %#v", f.D)
	}
}

// The property everything else depends on: a caller in the middle of storing a
// message must never be slowed down, let alone blocked, by a client that has
// stopped reading. A full queue drops.
func TestAFullQueueDropsInsteadOfBlocking(t *testing.T) {
	h := NewHub()
	stuck := newConn()
	h.add("u1", stuck)

	done := make(chan struct{})
	go func() {
		for range sendQueue * 3 {
			h.Send("u1", KindDM, nil)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked on a client that stopped reading")
	}
	if len(stuck.out) != sendQueue {
		t.Fatalf("queue holds %d, want it capped at %d", len(stuck.out), sendQueue)
	}
}

func TestRemoveStopsDeliveryAndForgetsTheUser(t *testing.T) {
	h := NewHub()
	c := newConn()
	h.add("u1", c)
	h.remove("u1", c)

	h.Send("u1", KindDM, nil)

	select {
	case <-c.out:
		t.Fatal("a removed connection still received a nudge")
	default:
	}
	// The map outlives every connection in it, so an emptied set must not stay
	// behind as a key for every user who has ever signed in.
	if _, still := h.byUser["u1"]; still {
		t.Fatal("the user's entry was left behind after their last connection went")
	}
	if h.Count() != 0 {
		t.Fatalf("Count says %d connections remain", h.Count())
	}
}

func TestSendToSomebodyWhoIsntListeningIsHarmless(t *testing.T) {
	h := NewHub()
	h.Send("nobody", KindProfile, nil) // must not panic
	if h.Listening("nobody") {
		t.Fatal("Listening said yes for a user with no connections")
	}
	// A nil hub is what a domain holds when notifications aren't wired up.
	var nilHub *Hub
	nilHub.Send("u1", KindDM, nil)
}

func TestSendManyReachesEachUserOnce(t *testing.T) {
	h := NewHub()
	a, b := newConn(), newConn()
	h.add("u1", a)
	h.add("u2", b)

	h.SendMany([]string{"u1", "u2"}, KindProfile, nil)

	if read(t, a).T != KindProfile || read(t, b).T != KindProfile {
		t.Fatal("SendMany missed somebody")
	}
	for _, c := range []*conn{a, b} {
		select {
		case <-c.out:
			t.Fatal("SendMany delivered twice")
		default:
		}
	}
}

// ── presence: the socket IS the signal ──────────────────────────────────────

// A first socket arriving and a last one going are the two moments somebody's
// friends need to hear about. The ones in between are other windows of theirs,
// and announcing one would flap a dot that never changed.
func TestOnlyTheFirstAndLastSocketAnnounce(t *testing.T) {
	h := NewHub()
	moved := []string{}
	h.OnPresence(func(userID string) { moved = append(moved, userID) })

	a, b := newConn(), newConn()
	if !h.add("u1", a) {
		t.Fatal("the first socket did not report itself as first")
	}
	if h.add("u1", b) {
		t.Error("a second window reported itself as an arrival")
	}
	if h.remove("u1", a) {
		t.Error("closing one of two windows reported itself as a departure")
	}
	if !h.remove("u1", b) {
		t.Error("the last socket did not report itself as last")
	}
	// Removing it twice must not announce again: a hub that double-counted a
	// departure would tell a user's friends to re-read after they had gone.
	if h.remove("u1", b) {
		t.Error("removing the same socket twice reported a second departure")
	}

	h.announce("u1")
	if len(moved) != 1 || moved[0] != "u1" {
		t.Fatalf("the hook saw %v, want one u1", moved)
	}
}

// Presence reads this for a whole friends list, so it answers in one pass. An id
// with nothing open is ABSENT rather than false, so a caller that forgets to
// check a missing key still reads "not connected".
func TestConnectedAmongAnswersForAList(t *testing.T) {
	h := NewHub()
	h.add("here", newConn())
	h.add("also", newConn())

	got := h.ConnectedAmong([]string{"here", "gone", "also"})
	if !got["here"] || !got["also"] {
		t.Errorf("a connected user read as absent: %v", got)
	}
	if _, present := got["gone"]; present {
		t.Errorf("somebody with no socket is in the map: %v", got)
	}
	if len(h.ConnectedAmong(nil)) != 0 {
		t.Error("asking about nobody answered about somebody")
	}
}

// A hub with no hook is how every test and every unwired build makes one.
func TestAnnounceWithNoHookIsHarmless(t *testing.T) {
	NewHub().announce("u1")
}
