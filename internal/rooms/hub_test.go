package rooms

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The relay's rules, exercised without a network. The Sender seam is why this is
// possible: a fake collects frames, so presence, the host rules, the rate cap and
// the reaper are all ordinary function calls.

type fakeConn struct {
	frames [][]byte
	closed bool
}

func (f *fakeConn) Send(msg []byte) { f.frames = append(f.frames, append([]byte(nil), msg...)) }

func (f *fakeConn) Close() { f.closed = true }

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

// last returns the payload of the most recent frame of this type.
func (f *fakeConn) last(msgType string) map[string]any {
	for i := len(f.frames) - 1; i >= 0; i-- {
		var e envelope
		if err := json.Unmarshal(f.frames[i], &e); err != nil || e.Type != msgType {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return nil
		}
		return data
	}
	return nil
}

func newTestRoom() (*Hub, *Room, time.Time) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	h := NewHub()
	return h, h.Create("room1", "amber-lantern", "host", now), now
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

// ── the room is a space, not a game ──────────────────────────────────────────

// A room starts empty-handed and the host points it at a game later. Nothing
// about creating one mentions an activity.
func TestRoomStartsWithNoGame(t *testing.T) {
	_, room, _ := newTestRoom()
	if room.Activity() != "" {
		t.Errorf("a new room should have no game, got %q", room.Activity())
	}
	if room.Name() != "amber-lantern" {
		t.Errorf("room should carry its name, got %q", room.Name())
	}
}

func TestHostSetsTheGameAndEveryoneHears(t *testing.T) {
	_, room, _ := newTestRoom()
	a, b := &fakeConn{}, &fakeConn{}
	_, _ = room.Join("host", "h", a)
	_, _ = room.Join("guest", "g", b)

	if err := room.SetActivity("host", "connect-four"); err != nil {
		t.Fatalf("host should be able to set the game: %v", err)
	}
	if room.Activity() != "connect-four" {
		t.Errorf("game should be recorded, got %q", room.Activity())
	}
	if got := b.last(TypeUpdate); got == nil || got["activity_id"] != "connect-four" {
		t.Errorf("members should be told which game, got %v", got)
	}
	// The update carries the name too, so one frame describes the whole room.
	if got := b.last(TypeUpdate); got["name"] != "amber-lantern" {
		t.Errorf("update should carry the name as well, got %v", got)
	}
}

func TestOnlyTheHostMaySetTheGameOrRename(t *testing.T) {
	_, room, _ := newTestRoom()
	_, _ = room.Join("host", "h", &fakeConn{})
	guest := &fakeConn{}
	_, _ = room.Join("guest", "g", guest)

	if err := room.SetActivity("guest", "chess"); !errors.Is(err, ErrNotHost) {
		t.Errorf("a guest must not set the game, got %v", err)
	}
	if err := room.SetName("guest", "mine now"); !errors.Is(err, ErrNotHost) {
		t.Errorf("a guest must not rename the room, got %v", err)
	}
	if room.Activity() != "" || room.Name() != "amber-lantern" {
		t.Error("a refused control must not have changed anything")
	}
}

func TestRenameAnnouncesAndClamps(t *testing.T) {
	_, room, _ := newTestRoom()
	_, _ = room.Join("host", "h", &fakeConn{})
	guest := &fakeConn{}
	_, _ = room.Join("guest", "g", guest)

	if err := room.SetName("host", "  friday night  "); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if room.Name() != "friday night" {
		t.Errorf("name should be trimmed, got %q", room.Name())
	}
	if got := guest.last(TypeUpdate); got == nil || got["name"] != "friday night" {
		t.Errorf("members should hear the rename, got %v", got)
	}

	// An empty name is refused; an over-long one is clamped rather than refused,
	// because a label is not worth failing over.
	if err := room.SetName("host", "   "); err == nil {
		t.Error("an empty name should be refused")
	}
	long := make([]byte, MaxRoomNameLen+20)
	for i := range long {
		long[i] = 'x'
	}
	if err := room.SetName("host", string(long)); err != nil {
		t.Fatalf("a long name should be clamped, not refused: %v", err)
	}
	if len(room.Name()) != MaxRoomNameLen {
		t.Errorf("name should clamp to %d, got %d", MaxRoomNameLen, len(room.Name()))
	}
}

// ── kicking ──────────────────────────────────────────────────────────────────

func TestKickRemovesTellsAndDisconnects(t *testing.T) {
	_, room, now := newTestRoom()
	host, guest, other := &fakeConn{}, &fakeConn{}, &fakeConn{}
	_, _ = room.Join("host", "h", host)
	_, _ = room.Join("guest", "g", guest)
	_, _ = room.Join("other", "o", other)

	if err := room.Kick("host", "guest"); err != nil {
		t.Fatalf("kick: %v", err)
	}
	if guest.count(TypeKicked) != 1 {
		t.Errorf("the kicked member should be told why, got %v", guest.types())
	}
	if !guest.closed {
		t.Error("the kicked member's connection should be closed")
	}
	if other.count(TypeLeave) != 1 {
		t.Errorf("everyone else sees an ordinary departure, got %v", other.types())
	}
	if len(room.Members()) != 2 {
		t.Errorf("roster should be down to two, got %+v", room.Members())
	}
	// And they stop receiving traffic.
	before := guest.count("ping")
	room.Relay("host", []byte(`{"t":"ping"}`), now)
	if guest.count("ping") != before {
		t.Error("a kicked member should not still receive relayed messages")
	}
}

func TestKickRefusals(t *testing.T) {
	_, room, _ := newTestRoom()
	_, _ = room.Join("host", "h", &fakeConn{})
	_, _ = room.Join("guest", "g", &fakeConn{})

	if err := room.Kick("guest", "host"); !errors.Is(err, ErrNotHost) {
		t.Errorf("a guest must not kick, got %v", err)
	}
	if err := room.Kick("host", "host"); err == nil {
		t.Error("the host must not be able to kick themselves - that's ending the room")
	}
	if err := room.Kick("host", "nobody"); err == nil {
		t.Error("kicking someone who isn't here should fail")
	}
}

// ── the host owns the room ───────────────────────────────────────────────────

// The whistle never changes hands. A room is the host's space.
func TestHostSeatNeverTransfers(t *testing.T) {
	_, room, now := newTestRoom()
	_, _ = room.Join("host", "h", &fakeConn{})
	second := &fakeConn{}
	_, _ = room.Join("second", "s", second)

	outcome := room.Leave("host", now)
	if !outcome.HostGone {
		t.Error("leaving as host should report it")
	}
	if outcome.Empty {
		t.Error("the room still has someone in it")
	}
	if room.Host() != "host" {
		t.Errorf("the host seat must not move, got %q", room.Host())
	}
	if second.count(TypeLeave) != 1 {
		t.Errorf("everyone should hear about the departure: %v", second.types())
	}
	// Crucially NOT closed: this may have been a dropped connection.
	if room.Closed() {
		t.Error("a host disconnect must not close the room on the spot")
	}
}

// A host whose connection blipped comes back to the same room, and the
// countdown to closing it is cancelled.
func TestHostCanReturnWithinTheGrace(t *testing.T) {
	hub, room, now := newTestRoom()
	_, _ = room.Join("host", "h", &fakeConn{})
	_, _ = room.Join("guest", "g", &fakeConn{})

	room.Leave("host", now)
	if n := hub.Reap(now.Add(30*time.Second), time.Minute); n != 0 {
		t.Fatalf("room closed inside the grace window (%d)", n)
	}

	if _, err := room.Join("host", "h", &fakeConn{}); err != nil {
		t.Fatalf("the host should be able to come back: %v", err)
	}
	if n := hub.Reap(now.Add(10*time.Minute), time.Minute); n != 0 {
		t.Errorf("a returned host should have cancelled the countdown, reaped %d", n)
	}
	if room.Closed() {
		t.Error("room should still be alive")
	}
}

// A host who doesn't come back ends the room, and the people still sitting in it
// are told rather than left staring at a dead lobby.
func TestHostGoneTooLongClosesTheRoom(t *testing.T) {
	hub, room, now := newTestRoom()
	_, _ = room.Join("host", "h", &fakeConn{})
	guest := &fakeConn{}
	_, _ = room.Join("guest", "g", guest)

	room.Leave("host", now)
	if n := hub.Reap(now.Add(2*time.Minute), time.Minute); n != 1 {
		t.Fatalf("an absent host past the grace should end the room, reaped %d", n)
	}
	if !room.Closed() {
		t.Error("room should be closed")
	}
	if guest.count(TypeClosed) != 1 {
		t.Errorf("the people still in it should be told, got %v", guest.types())
	}
	if !guest.closed {
		t.Error("their connection should be closed too")
	}
	if _, err := hub.Get("room1"); err == nil {
		t.Error("a closed room should no longer be reachable")
	}
}

// Ending on purpose is immediate - that's the difference between a decision and
// a dropped socket.
func TestCloseEndsItForEveryone(t *testing.T) {
	hub, room, _ := newTestRoom()
	host, guest := &fakeConn{}, &fakeConn{}
	_, _ = room.Join("host", "h", host)
	_, _ = room.Join("guest", "g", guest)

	room.Close()
	hub.Remove(room.ID)

	for name, c := range map[string]*fakeConn{"host": host, "guest": guest} {
		if c.count(TypeClosed) != 1 {
			t.Errorf("%s should be told the room ended, got %v", name, c.types())
		}
		if !c.closed {
			t.Errorf("%s's connection should be closed", name)
		}
	}
	if len(room.Members()) != 0 {
		t.Errorf("a closed room has no members, got %+v", room.Members())
	}
	// Twice is a no-op, not a second round of frames.
	room.Close()
	if host.count(TypeClosed) != 1 {
		t.Error("closing twice should not re-announce")
	}
}

func TestClosedRoomRefusesJoinsAndControls(t *testing.T) {
	_, room, _ := newTestRoom()
	_, _ = room.Join("host", "h", &fakeConn{})
	room.Close()

	if _, err := room.Join("host", "h", &fakeConn{}); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("a closed room should refuse a join, got %v", err)
	}
	if err := room.SetName("host", "later"); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("a closed room should refuse a rename, got %v", err)
	}
	if err := room.SetActivity("host", "chess"); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("a closed room should refuse a game change, got %v", err)
	}
	if err := room.Kick("host", "guest"); !errors.Is(err, ErrRoomClosed) {
		t.Errorf("a closed room should refuse a kick, got %v", err)
	}
}

// ── everything below is unchanged behaviour, still guarded ───────────────────

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

// A room name should be readable and shaped like a name - the point is being
// able to say it out loud, not uniqueness.
func TestRandomName(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		name := RandomName()
		if len(name) == 0 || len(name) > MaxRoomNameLen {
			t.Fatalf("implausible name %q", name)
		}
		parts := 0
		for _, c := range name {
			if c == '-' {
				parts++
			}
		}
		if parts != 1 {
			t.Fatalf("expected two hyphenated words, got %q", name)
		}
		seen[name] = true
	}
	// Not a uniqueness guarantee - just proof it isn't returning one constant.
	if len(seen) < 5 {
		t.Errorf("names look stuck, only %d distinct in 50", len(seen))
	}
}
