package rooms

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// The host's "members can invite" switch and the join gate that widens with it.
//
// The cast: host, bob (a member, the host's friend) and sam, who is bob's friend
// and a stranger to the host. With the switch off bob may not invite sam; with it
// on he may, and sam then gets in on the strength of the invite alone.

// memberInvitesRoom is a room hosted by "host" with bob inside; sam is bob's
// friend and nobody else's. Each person has a recorded connection.
func memberInvitesRoom(t *testing.T) (*inviteRig, *Room, map[string]*fakeConn) {
	g := newInviteRig(t)
	room := g.h.hub.Create("room1", "amber-lantern", "host", g.clock)
	conns := map[string]*fakeConn{"host": {}, "bob": {}}
	for _, id := range []string{"host", "bob"} {
		if _, err := room.Join(id, "name-"+id, conns[id]); err != nil {
			t.Fatal(err)
		}
	}
	g.befriend("host", "bob")
	g.befriend("bob", "sam")
	return g, room, conns
}

// setMemberInvites sends the real control frame through the real dispatcher, so the
// wire name and body are part of what is tested.
func setMemberInvites(g *inviteRig, room *Room, as string, body string, via Sender) {
	g.h.control(room, as, &envelope{Type: CtlInvites, Data: json.RawMessage(body)}, via)
}

// samAccepts has bob invite sam and sam accept; the switch must already be on.
func samAccepts(g *inviteRig, room *Room) Invite {
	g.t.Helper()
	rec := g.invite(room.ID, "bob", "sam")
	if rec.Code != http.StatusCreated {
		g.t.Fatalf("invite status = %d (%s)", rec.Code, rec.Body.String())
	}
	inv := decodeInvite(g.t, rec)
	if acc := g.accept("sam", inv.ID); acc.Code != http.StatusOK {
		g.t.Fatalf("accept status = %d", acc.Code)
	}
	return inv
}

func TestMemberInvitesDefaultsOffAndRefusesAStrangerToTheHost(t *testing.T) {
	g, room, _ := memberInvitesRoom(t)
	if room.MemberInvites() {
		t.Fatal("a new room must have member invites off")
	}
	rec := g.invite("room1", "bob", "sam")
	if rec.Code != http.StatusNotFound || errorCode(rec) != errRoomNotFound {
		t.Fatalf("status = %d, error = %q: want the opaque 404", rec.Code, errorCode(rec))
	}
	if got := g.list("sam"); len(got) != 0 {
		t.Fatalf("a refused invite must leave nothing behind: %+v", got)
	}
	// Today's behaviour, untouched: the host's own friend is still fine.
	g.befriend("bob", "alice")
	g.befriend("host", "alice")
	if rec := g.invite("room1", "bob", "alice"); rec.Code != http.StatusCreated {
		t.Fatalf("a friend of the host with the switch off: status = %d", rec.Code)
	}
}

func TestMemberInvitesOnAllowsAStrangerToTheHost(t *testing.T) {
	g, room, conns := memberInvitesRoom(t)
	setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
	if !room.MemberInvites() {
		t.Fatal("the host's control should have switched it on")
	}
	rec := g.invite("room1", "bob", "sam")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	// Still bounded by friendship: a stranger to BOTH the host and the inviter is
	// refused with the same opaque 404.
	if rec := g.invite("room1", "bob", "zed"); rec.Code != http.StatusNotFound || errorCode(rec) != errRoomNotFound {
		t.Fatalf("a non-friend with the switch on: status = %d, error = %q", rec.Code, errorCode(rec))
	}
}

func TestOnlyTheHostMaySetMemberInvites(t *testing.T) {
	g, room, conns := memberInvitesRoom(t)

	setMemberInvites(g, room, "bob", `{"allow":true}`, conns["bob"])
	if room.MemberInvites() {
		t.Fatal("a member must not be able to switch member invites on")
	}
	if got := conns["bob"].last(TypeError); got == nil || got["code"] != "not_host" {
		t.Fatalf("bob should be told not_host, got %v", got)
	}

	// The host may; and a member then cannot switch it back off.
	setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
	setMemberInvites(g, room, "bob", `{"allow":false}`, conns["bob"])
	if !room.MemberInvites() {
		t.Fatal("a member must not be able to switch member invites off")
	}

	// A body that leaves `allow` out is refused, not read as "off".
	hostConn := &fakeConn{}
	setMemberInvites(g, room, "host", `{}`, hostConn)
	if !room.MemberInvites() {
		t.Fatal("a malformed request must not change the switch")
	}
	if got := hostConn.last(TypeError); got == nil || got["code"] != errInvalidBody {
		t.Fatalf("a missing allow should be %q, got %v", errInvalidBody, got)
	}
}

func TestMemberInvitesReachesMembersOverUpdateAndTheWelcome(t *testing.T) {
	g, room, conns := memberInvitesRoom(t)

	welcome := func(you string) map[string]any {
		frame, err := welcomeFrame(room, you)
		if err != nil {
			t.Fatal(err)
		}
		c := &fakeConn{}
		c.Send(frame)
		return c.last(TypeWelcome)
	}
	if got := welcome("sam"); got["member_invites"] != false {
		t.Fatalf("welcome should say member_invites=false by default, got %v", got["member_invites"])
	}

	setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
	if got := conns["bob"].last(TypeUpdate); got == nil || got["member_invites"] != true {
		t.Fatalf("a member should hear member_invites=true over room:update, got %v", got)
	}
	if got := welcome("sam"); got["member_invites"] != true {
		t.Fatalf("a joiner should see member_invites=true in the welcome, got %v", got["member_invites"])
	}

	// One update frame describes the whole room: a rename afterwards still says
	// where the switch stands, and the switch's own update still names the room.
	if err := room.SetName("host", "game night"); err != nil {
		t.Fatal(err)
	}
	got := conns["bob"].last(TypeUpdate)
	if got["member_invites"] != true || got["name"] != "game night" {
		t.Fatalf("every room:update carries the whole state, got %v", got)
	}
	updates := conns["bob"].count(TypeUpdate)
	setMemberInvites(g, room, "host", `{"allow":false}`, conns["host"])
	if conns["bob"].count(TypeUpdate) != updates+1 {
		t.Fatal("the switch should ride exactly one room:update, not a second broadcast")
	}
	if conns["bob"].last(TypeUpdate)["member_invites"] != false {
		t.Fatal("switching off should be broadcast too")
	}
}

func TestAcceptedInviteAdmitsAStrangerToTheHost(t *testing.T) {
	g, room, conns := memberInvitesRoom(t)
	ctx := t.Context()
	if g.h.allowed(ctx, room, "sam") {
		t.Fatal("a stranger with no invite must not be admitted")
	}

	setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
	inv := decodeInvite(t, g.invite("room1", "bob", "sam"))
	if g.h.allowed(ctx, room, "sam") {
		t.Fatal("a PENDING invite is an offer, not a way in")
	}
	g.accept("sam", inv.ID)
	if !g.h.allowed(ctx, room, "sam") {
		t.Fatal("an accepted invite should admit its holder")
	}
	if rec := getRoom(g.h, "room1", "sam"); rec.Code != http.StatusOK {
		t.Fatalf("GET /rooms/{id} shares the gate: status = %d", rec.Code)
	}
	// Narrow: it admits that one person and no one else.
	if g.h.allowed(ctx, room, "zed") {
		t.Fatal("an invite admits only its holder")
	}
}

func TestADeclinedAnExpiredAndAnotherRoomsInviteAdmitNobody(t *testing.T) {
	ctx := t.Context()

	t.Run("declined", func(t *testing.T) {
		g, room, conns := memberInvitesRoom(t)
		setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
		inv := decodeInvite(t, g.invite("room1", "bob", "sam"))
		g.decline("sam", inv.ID)
		if g.h.allowed(ctx, room, "sam") {
			t.Fatal("a declined invite admitted someone")
		}
		if rec := getRoom(g.h, "room1", "sam"); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want the opaque 404", rec.Code)
		}
	})

	t.Run("lapsed before it was answered", func(t *testing.T) {
		g, room, conns := memberInvitesRoom(t)
		setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
		g.invite("room1", "bob", "sam")
		g.advance(InviteTTL + time.Minute)
		if g.h.allowed(ctx, room, "sam") {
			t.Fatal("an expired invite admitted someone")
		}
	})

	t.Run("accepted, and then its hour ran out unread", func(t *testing.T) {
		g, room, conns := memberInvitesRoom(t)
		setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
		samAccepts(g, room)
		if !g.h.allowed(ctx, room, "sam") {
			t.Fatal("setup: the accepted invite should admit")
		}
		// Nothing reads the invite in between: the gate itself must notice.
		g.advance(InviteTTL)
		if g.h.allowed(ctx, room, "sam") {
			t.Fatal("an accepted invite admitted someone after its hour")
		}
		if rec := getRoom(g.h, "room1", "sam"); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want the opaque 404", rec.Code)
		}
	})

	t.Run("accepted, but to a different room", func(t *testing.T) {
		g, room, conns := memberInvitesRoom(t)
		other := g.room("room2", "otherhost", "otherguest")
		setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
		samAccepts(g, room)
		if g.h.allowed(ctx, other, "sam") {
			t.Fatal("an invite to one room admitted someone to another")
		}
		if rec := getRoom(g.h, "room2", "sam"); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want the opaque 404", rec.Code)
		}
	})
}

func TestSwitchingOffEvictsNobodyAndVoidsNoAcceptedInvite(t *testing.T) {
	g, room, conns := memberInvitesRoom(t)
	ctx := t.Context()
	setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
	samAccepts(g, room)
	samConn := &fakeConn{}
	if _, err := room.Join("sam", "name-sam", samConn); err != nil {
		t.Fatal(err)
	}

	setMemberInvites(g, room, "host", `{"allow":false}`, conns["host"])

	if !room.HasMember("sam") || samConn.closed {
		t.Fatal("switching off must not remove or disconnect a member")
	}
	if !g.h.allowed(ctx, room, "sam") {
		t.Fatal("switching off must not void an invite already accepted")
	}
	// What it DOES do is stop new invites.
	g.befriend("bob", "kim")
	if rec := g.invite("room1", "bob", "kim"); rec.Code != http.StatusNotFound {
		t.Fatalf("a new stranger invite after switching off: status = %d, want 404", rec.Code)
	}
}

// With the switch off, "who may be invited" is exactly who needs no invite to get
// in. Counting an accepted invite as a reason to invite would let a member extend a
// stranger's stay indefinitely with the switch off.
func TestOffMeansOffEvenForSomeoneAnEarlierInviteLetIn(t *testing.T) {
	g, room, conns := memberInvitesRoom(t)
	setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
	samAccepts(g, room)
	setMemberInvites(g, room, "host", `{"allow":false}`, conns["host"])

	if rec := g.invite("room1", "bob", "sam"); rec.Code != http.StatusNotFound {
		t.Fatalf("re-inviting an admitted stranger with the switch off: status = %d, want 404", rec.Code)
	}
}

// The handler reads the switch before a database round trip; the room reads it
// again under its lock, so a host who flips it in between is not overtaken.
func TestAnInviteAuthorisedOnTheSwitchIsRefusedIfItIsOffByTheTimeTheRoomLocks(t *testing.T) {
	g, room, _ := memberInvitesRoom(t)
	if _, _, err := room.Invite("bob", "sam", "sam", g.clock, true); err != ErrInvitesOff {
		t.Fatalf("err = %v, want ErrInvitesOff", err)
	}
	if _, _, err := room.Invite("bob", "alice", "alice", g.clock, false); err != nil {
		t.Fatalf("an invite that did not rest on the switch: %v", err)
	}
}

// An accepted invite is a credential for its hour, so making room for new invites
// must not forget it.
func TestAMembersInviteFloodCannotForgetAnAcceptedInvite(t *testing.T) {
	g, room, conns := memberInvitesRoom(t)
	setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
	samAccepts(g, room)
	for i := 0; i < maxInvitesPerRoom-1; i++ {
		id := "u" + string(rune('A'+i/26)) + string(rune('a'+i%26))
		if _, _, err := room.Invite("bob", id, "", g.clock, false); err != nil {
			t.Fatalf("invite %d: %v", i, err)
		}
	}
	// The room is full; this one makes it sweep for spent invites.
	_, _, _ = room.Invite("bob", "one-too-many", "", g.clock, false)
	if !g.h.allowed(t.Context(), room, "sam") {
		t.Fatal("making space forgot a live accepted invite, locking its holder out")
	}
}

func TestAnAcceptedInviteDiesWithTheRoom(t *testing.T) {
	g, room, conns := memberInvitesRoom(t)
	setMemberInvites(g, room, "host", `{"allow":true}`, conns["host"])
	samAccepts(g, room)
	if !g.h.allowed(t.Context(), room, "sam") {
		t.Fatal("setup: the accepted invite should admit")
	}

	room.Close()

	// Asked of the room directly and through the gate with the ended room in hand:
	// hub.Get already hides a closed room, and the invite must not rely on that.
	if room.HoldsInvite("sam", g.clock) {
		t.Fatal("an accepted invite is still good after the room ended")
	}
	if g.h.allowed(t.Context(), room, "sam") {
		t.Fatal("the gate admitted an invite holder to an ended room")
	}
	if rec := getRoom(g.h, "room1", "sam"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the opaque 404", rec.Code)
	}
}
