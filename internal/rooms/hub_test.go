package rooms

import (
	"encoding/json"
	"testing"
	"time"
)

// The relay's rules, exercised without a network. The Sender seam is why this is
// possible: a fake collects frames, so presence, host transfer, the rate cap and
// the reaper are all ordinary function calls.

type fakeConn struct {
	frames [][]byte
}

func (f *fakeConn) Send(msg []byte) { f.frames = append(f.frames, append([]byte(nil), msg...)) }

func (f *fakeConn) types() []string {
	out := make([]string, 0, len(f.frames))
	for _, raw := range f.frames {
		var e envelope
		if err := json.Unmarshal(raw, &e); err == nil {
			out = append(out, e.Type)
		}
	}
	return out
}

func (f *fakeConn) count(msgType string) int {
	n := 0
	for _, t := range f.types() {
		if t == msgType {
			n++
		}
	}
	return n
}

func newTestRoom() (*Hub, *Room, time.Time) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	h := NewHub()
	return h, h.Create("room1", "chess", "host", now), now
}

func TestJoinAnnouncesToEveryoneElse(t *testing.T) {
	_, room, _ := newTestRoom()
	a, b := &fakeConn{}, &fakeConn{}

	if _, err := room.Join("host", "hostie", a); err != nil {
		t.Fatalf("host join: %v", err)
	}
	if _, err := room.Join("guest", "guestie", b); err != nil {
		t.Fatalf("guest join: %v", err)
	}

	if a.count(TypeJoin) != 1 {
		t.Errorf("host should have been told about the guest, got %v", a.types())
	}
	if b.count(TypeJoin) != 0 {
		t.Errorf("a joiner shouldn't be told about their own arrival, got %v", b.types())
	}
	if got := room.Members(); len(got) != 2 || got[0].UserID != "host" || got[1].UserID != "guest" {
		t.Errorf("roster should be in arrival order, got %+v", got)
	}
}

func TestRelayGoesToEveryoneButTheSender(t *testing.T) {
	_, room, now := newTestRoom()
	a, b, c := &fakeConn{}, &fakeConn{}, &fakeConn{}
	_, _ = room.Join("host", "h", a)
	_, _ = room.Join("g1", "g1", b)
	_, _ = room.Join("g2", "g2", c)

	frame := []byte(`{"t":"chess:move","d":{"from":"e2"},"from":"host"}`)
	if !room.Relay("host", frame, now) {
		t.Fatal("relay should have been allowed")
	}
	if a.count("chess:move") != 0 {
		t.Error("sender should not receive their own message back")
	}
	if b.count("chess:move") != 1 || c.count("chess:move") != 1 {
		t.Errorf("both others should get it: %v / %v", b.types(), c.types())
	}
}

// A room outlives its creator: the whistle passes to whoever has been there
// longest, so a session doesn't die when the host's laptop closes.
func TestHostTransfersOnLeave(t *testing.T) {
	_, room, now := newTestRoom()
	_, _ = room.Join("host", "h", &fakeConn{})
	_, _ = room.Join("second", "s", &fakeConn{})
	third := &fakeConn{}
	_, _ = room.Join("third", "t", third)

	if room.Host() != "host" {
		t.Fatalf("host should start as host, got %q", room.Host())
	}
	if empty := room.Leave("host", now); empty {
		t.Error("room still has members")
	}
	if room.Host() != "second" {
		t.Errorf("host should pass to the longest-present member, got %q", room.Host())
	}
	if third.count(TypeLeave) != 1 {
		t.Errorf("everyone should hear about the departure: %v", third.types())
	}

	// And again, down to the last member.
	room.Leave("second", now)
	if room.Host() != "third" {
		t.Errorf("host should pass again, got %q", room.Host())
	}
	if empty := room.Leave("third", now); !empty {
		t.Error("room should report empty once the last member goes")
	}
}

// A reconnect must reclaim the same seat rather than leaving a ghost behind.
func TestRejoinReplacesTheConnection(t *testing.T) {
	_, room, now := newTestRoom()
	other := &fakeConn{}
	_, _ = room.Join("host", "h", other)
	old := &fakeConn{}
	_, _ = room.Join("guest", "g", old)

	fresh := &fakeConn{}
	if _, err := room.Join("guest", "g", fresh); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if got := len(room.Members()); got != 2 {
		t.Errorf("rejoin should not add a second seat, got %d members", got)
	}
	if other.count(TypeJoin) != 1 {
		t.Errorf("a rejoin should not re-announce, got %v", other.types())
	}

	room.Relay("host", []byte(`{"t":"ping"}`), now)
	if fresh.count("ping") != 1 {
		t.Error("the new connection should receive traffic")
	}
	if old.count("ping") != 0 {
		t.Error("the replaced connection should be forgotten")
	}
}

// The cap exists so one broken client can't flood a room; it must NOT be so
// tight that legitimate 30Hz position sync trips it.
func TestRateLimit(t *testing.T) {
	_, room, now := newTestRoom()
	_, _ = room.Join("host", "h", &fakeConn{})
	_, _ = room.Join("guest", "g", &fakeConn{})

	sent, blocked := 0, 0
	for i := 0; i < rateBurst+50; i++ {
		if room.Relay("host", []byte(`{"t":"x"}`), now) {
			sent++
		} else {
			blocked++
		}
	}
	if sent != rateBurst {
		t.Errorf("should allow exactly the burst, allowed %d", sent)
	}
	if blocked == 0 {
		t.Error("a flood past the burst should be refused")
	}

	// Tokens refill over time: a second later, a full second's worth is back.
	if !room.Relay("host", []byte(`{"t":"x"}`), now.Add(time.Second)) {
		t.Error("the bucket should refill")
	}

	// A realistic 30Hz stream for 5 seconds must never be refused.
	_, room2, base := newTestRoom()
	_, _ = room2.Join("a", "a", &fakeConn{})
	_, _ = room2.Join("b", "b", &fakeConn{})
	for i := 0; i < 150; i++ {
		at := base.Add(time.Duration(i) * (time.Second / 30))
		if !room2.Relay("a", []byte(`{"t":"pos"}`), at) {
			t.Fatalf("30Hz sync was rate-limited at frame %d", i)
		}
	}
}

func TestReaperOnlyDropsRoomsEmptyPastTheGrace(t *testing.T) {
	hub, room, now := newTestRoom()
	_, _ = room.Join("host", "h", &fakeConn{})

	// Occupied rooms are never reaped, however long they sit.
	if n := hub.Reap(now.Add(time.Hour), time.Minute); n != 0 {
		t.Errorf("an occupied room was reaped (%d)", n)
	}

	room.Leave("host", now)
	// Inside the grace window a reconnect is still possible - keep it.
	if n := hub.Reap(now.Add(30*time.Second), time.Minute); n != 0 {
		t.Errorf("room reaped inside the grace window (%d)", n)
	}
	if _, err := hub.Get("room1"); err != nil {
		t.Error("room should still exist inside the grace window")
	}
	// Past it, drop it.
	if n := hub.Reap(now.Add(2*time.Minute), time.Minute); n != 1 {
		t.Errorf("empty room past grace should be reaped, got %d", n)
	}
	if _, err := hub.Get("room1"); err == nil {
		t.Error("room should be gone")
	}
}

func TestRoomFull(t *testing.T) {
	_, room, _ := newTestRoom()
	for i := 0; i < maxMembers; i++ {
		if _, err := room.Join(string(rune('a'+i)), "x", &fakeConn{}); err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
	}
	if _, err := room.Join("one-too-many", "x", &fakeConn{}); err != ErrRoomFull {
		t.Errorf("expected ErrRoomFull, got %v", err)
	}
}

// The server stamps `from` itself; a client claiming to be someone else must not
// survive the round trip.
func TestStampFromOverwritesClientClaim(t *testing.T) {
	e, ok := decodeIncoming([]byte(`{"t":"chess:move","d":{"x":1},"from":"somebody-else"}`))
	if !ok {
		t.Fatal("should decode")
	}
	out, err := stampFrom(e, "real-user")
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	var got envelope
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.From != "real-user" {
		t.Errorf("from should be the authenticated sender, got %q", got.From)
	}
	if string(got.Data) != `{"x":1}` {
		t.Errorf("payload must pass through byte-for-byte, got %s", got.Data)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, ok := decodeIncoming([]byte(`not json`)); ok {
		t.Error("garbage should be refused")
	}
	if _, ok := decodeIncoming([]byte(`{"d":{"x":1}}`)); ok {
		t.Error("a frame with no type should be refused")
	}
}

func TestBearerFromProtocols(t *testing.T) {
	if got := bearerFromProtocols("bearer, abc.def.ghi"); got != "abc.def.ghi" {
		t.Errorf("got %q", got)
	}
	if got := bearerFromProtocols("bearer,abc"); got != "abc" {
		t.Errorf("got %q", got)
	}
	for _, bad := range []string{"", "abc", "notbearer, abc"} {
		if got := bearerFromProtocols(bad); got != "" {
			t.Errorf("%q should yield no token, got %q", bad, got)
		}
	}
}
