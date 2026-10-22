package rooms

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tabuhana/bombers-server/internal/auth"
)

// An invite is held in memory and its message lives in the database, so they are
// not one transaction. These tests are about the rules that stand in for one:
// every NEW invite gets exactly one message, an invite whose message cannot be
// written is voided, and a message may outlive its invite.

func (g *inviteRig) get(as, id string) *httptest.ResponseRecorder {
	return g.do(g.h.GetInvite, http.MethodGet, "/invites/"+id, as, map[string]string{"inviteID": id}, "")
}

func (g *inviteRig) mintedMessages() []mintedMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]mintedMessage(nil), g.minted...)
}

// Making an invite writes exactly one message, from the inviter to the invitee,
// naming the invite and the room, with the room's name copied in at that moment.
func TestCreatingAnInviteMintsExactlyOneMessage(t *testing.T) {
	g, _ := standardRoom(t)

	rec := g.invite("room1", "bob", "alice")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	inv := decodeInvite(t, rec)

	got := g.mintedMessages()
	want := mintedMessage{inviteID: inv.ID, roomID: "room1", roomName: "amber-lantern", from: "bob", to: "alice"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("minted = %+v, want exactly %+v", got, want)
	}
}

// The message is written BEFORE the invitee is told, so the nudge they get is
// never about a card that is not in their thread yet.
func TestTheInviteeIsToldOnlyAfterTheMessageIsWritten(t *testing.T) {
	g, _ := standardRoom(t)
	g.h.mintInvite = func(ctx context.Context, inviteID, roomID, roomName, from, to string) error {
		if n := g.nudges("alice"); len(n) != 0 {
			t.Errorf("alice was told %d time(s) before her message existed", len(n))
		}
		return nil
	}
	if rec := g.invite("room1", "bob", "alice"); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := g.nudges("alice"); len(n) != 1 || n[0].inv.State != InvitePending {
		t.Errorf("alice's nudges = %+v, want the one pending invite", n)
	}
}

// If the message cannot be written the invite is VOIDED: the inviter gets a
// failure, the invitee is told nothing and has nothing waiting, and nobody can
// read, answer or find the invite. It is not left as a pending invite with no
// card, and it is not left as a state of its own.
func TestAMessageThatCannotBeWrittenVoidsTheInvite(t *testing.T) {
	g, room := standardRoom(t)
	g.mintErr = errors.New("database is down")

	rec := g.invite("room1", "bob", "alice")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s, want 500", rec.Code, rec.Body)
	}
	if len(g.mintCtxs) != 1 {
		t.Fatalf("the message was attempted %d times", len(g.mintCtxs))
	}

	if n := g.nudges("alice"); len(n) != 0 {
		t.Errorf("alice was told of an invite that was voided: %+v", n)
	}
	if got := g.list("alice"); len(got) != 0 {
		t.Errorf("alice has an invite waiting: %+v", got)
	}
	room.mu.Lock()
	held := len(room.invites)
	room.mu.Unlock()
	if held != 0 {
		t.Errorf("the room still holds %d invite(s)", held)
	}
	// Neither index may still point at it.
	g.h.hub.invites.mu.Lock()
	byUser, byID := len(g.h.hub.invites.byUser), len(g.h.hub.invites.byID)
	g.h.hub.invites.mu.Unlock()
	if byUser != 0 || byID != 0 {
		t.Errorf("index still holds %d user(s) and %d id(s)", byUser, byID)
	}

	// A voided invite is not a decline: once the database is back she can be asked,
	// and gets a fresh invite and a message.
	g.mintErr = nil
	rec = g.invite("room1", "bob", "alice")
	if rec.Code != http.StatusCreated {
		t.Fatalf("asking again: status = %d, body %s, want a fresh invite", rec.Code, rec.Body)
	}
	if got := g.mintedMessages(); len(got) != 1 || got[0].inviteID != decodeInvite(t, rec).ID {
		t.Errorf("minted = %+v", got)
	}
}

// The voided invite's id answers nothing for anyone.
func TestAVoidedInviteCannotBeReadOrAnswered(t *testing.T) {
	g, room := standardRoom(t)
	var made string
	g.h.mintInvite = func(ctx context.Context, inviteID, roomID, roomName, from, to string) error {
		made = inviteID
		return errors.New("no")
	}
	g.invite("room1", "bob", "alice")
	if made == "" {
		t.Fatal("setup: the message was never attempted")
	}
	for _, who := range []string{"alice", "bob"} {
		if rec := g.get(who, made); rec.Code != http.StatusNotFound {
			t.Errorf("%s reads a voided invite: %d", who, rec.Code)
		}
	}
	if rec := g.accept("alice", made); rec.Code != http.StatusNotFound {
		t.Errorf("accept a voided invite: %d", rec.Code)
	}
	if _, err := room.AnswerInvite(made, "alice", true, g.clock); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("the room still answers it: %v", err)
	}
}

// News queued about an invite before it was voided is dropped with it: nobody is
// told an invite they never heard of has expired.
func TestVoidingAnInviteDropsNewsQueuedAboutIt(t *testing.T) {
	g, room := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	room.mu.Lock()
	room.expired = append(room.expired, Invite{ID: inv.ID}, Invite{ID: "other"})
	room.voided = append(room.voided, Invite{ID: inv.ID})
	room.mu.Unlock()

	room.VoidInvite(inv.ID)

	room.mu.Lock()
	defer room.mu.Unlock()
	if len(room.expired) != 1 || room.expired[0].ID != "other" || len(room.voided) != 0 {
		t.Errorf("queues = %+v / %+v, want only the other invite's news", room.expired, room.voided)
	}
}

// Asking someone who already has a pending invite is one invitation: it returns the
// SAME invite and writes no second message.
func TestReinviteReturnsTheSameInviteAndWritesNoSecondMessage(t *testing.T) {
	g, _ := standardRoom(t)
	first := decodeInvite(t, g.invite("room1", "bob", "alice"))

	rec := g.invite("room1", "bob", "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if again := decodeInvite(t, rec); again.ID != first.ID {
		t.Errorf("a different invite: %s vs %s", again.ID, first.ID)
	}
	// By another member too: it is the person's invite, not the asker's.
	if rec := g.invite("room1", "host", "alice"); rec.Code != http.StatusOK {
		t.Errorf("host re-asking: status = %d", rec.Code)
	}
	if got := g.mintedMessages(); len(got) != 1 {
		t.Errorf("minted %d messages for one invitation: %+v", len(got), got)
	}
}

// A declined invite is final for the room's life and is returned, not remade: it
// has its message already.
func TestAskingAfterADeclineWritesNoMessage(t *testing.T) {
	g, _ := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	if rec := g.decline("alice", inv.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("decline: %d", rec.Code)
	}

	rec := g.invite("room1", "bob", "alice")
	if rec.Code != http.StatusOK || decodeInvite(t, rec).State != InviteDeclined {
		t.Fatalf("status = %d, body %s, want the declined invite back", rec.Code, rec.Body)
	}
	if got := g.mintedMessages(); len(got) != 1 {
		t.Errorf("minted %d messages: %+v", len(got), got)
	}
}

// A refused invite is refused before anything is made, so no message is written
// for it, whatever the reason.
func TestARefusedInviteWritesNoMessage(t *testing.T) {
	cases := map[string]func(g *inviteRig) *httptest.ResponseRecorder{
		"not a friend": func(g *inviteRig) *httptest.ResponseRecorder { return g.invite("room1", "bob", "stranger") },
		"not a member": func(g *inviteRig) *httptest.ResponseRecorder { return g.invite("room1", "alice", "bob") },
		"yourself":     func(g *inviteRig) *httptest.ResponseRecorder { return g.invite("room1", "bob", "bob") },
		"no such room": func(g *inviteRig) *httptest.ResponseRecorder { return g.invite("nowhere", "bob", "alice") },
		"room ended": func(g *inviteRig) *httptest.ResponseRecorder {
			g.h.hub.Get("room1")
			room, _ := g.h.hub.Get("room1")
			room.Close()
			return g.invite("room1", "bob", "alice")
		},
		"no user id": func(g *inviteRig) *httptest.ResponseRecorder {
			return g.do(g.h.CreateInvite, http.MethodPost, "/rooms/room1/invites", "bob", map[string]string{"roomID": "room1"}, `{}`)
		},
		"a friend the room would turn away": func(g *inviteRig) *httptest.ResponseRecorder {
			g.befriend("bob", "carol") // not the host's friend, and the switch is off
			return g.invite("room1", "bob", "carol")
		},
	}
	for name, do := range cases {
		t.Run(name, func(t *testing.T) {
			g, _ := standardRoom(t)
			rec := do(g)
			if rec.Code < 400 {
				t.Fatalf("status = %d, want a refusal", rec.Code)
			}
			if got := g.mintedMessages(); len(got) != 0 {
				t.Errorf("a refused invite minted %+v", got)
			}
			if len(g.mintCtxs) != 0 {
				t.Error("the message was even attempted")
			}
		})
	}
}

// Too many invites in one room is refused after the room is asked but before an
// invite exists: no message for it either.
func TestAnInviteOverTheCapWritesNoMessage(t *testing.T) {
	g, _ := standardRoom(t)
	for i := 0; i < maxInvitesPerRoom; i++ {
		id := "friend-" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		g.befriend("host", id)
		if rec := g.invite("room1", "host", id); rec.Code != http.StatusCreated {
			t.Fatalf("invite %d: %d %s", i, rec.Code, rec.Body)
		}
		// Decline so they are never forgotten to make room.
		list := g.list(id)
		g.decline(id, list[0].ID)
	}
	before := len(g.mintedMessages())
	g.befriend("host", "one-too-many")
	if rec := g.invite("room1", "host", "one-too-many"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 too_many_invites", rec.Code)
	}
	if after := len(g.mintedMessages()); after != before {
		t.Errorf("minted %d -> %d for a refused invite", before, after)
	}
}

// The message is not tied to the request that asked for it: a client that hangs up
// the instant it has sent must not leave an invite without its card.
func TestTheMessageIsWrittenEvenIfTheRequestIsGone(t *testing.T) {
	g, _ := standardRoom(t)
	var ctxErr error
	calls := 0
	g.h.mintInvite = func(ctx context.Context, inviteID, roomID, roomName, from, to string) error {
		calls++
		ctxErr = ctx.Err()
		return nil
	}
	req := httptest.NewRequest(http.MethodPost, "/rooms/room1/invites", strings.NewReader(`{"user_id":"alice"}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("roomID", "room1")
	ctx, cancel := context.WithCancel(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	cancel() // the client is already gone
	req = req.WithContext(auth.WithUserID(ctx, "bob"))

	rec := httptest.NewRecorder()
	g.h.CreateInvite(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if calls != 1 || ctxErr != nil {
		t.Errorf("the message was written %d time(s) under context error %v", calls, ctxErr)
	}
}

// ---- GET /invites/{id} ------------------------------------------------------

// The inviter AND the invitee can read the invite, whatever state it is in. That
// is the read a card in the thread makes, where GET /invites (pending, addressed
// to you) cannot answer for the person who sent it.
func TestGetInviteAnswersForTheSenderAndTheInvitee(t *testing.T) {
	g, _ := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))

	for _, who := range []string{"bob", "alice"} {
		rec := g.get(who, inv.ID)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body %s", who, rec.Code, rec.Body)
		}
		got := decodeInvite(t, rec)
		if got.ID != inv.ID || got.State != InvitePending || got.From != "bob" || got.To != "alice" || got.RoomName != "amber-lantern" {
			t.Errorf("%s reads %+v", who, got)
		}
	}
}

// Nobody else can, and an id nobody has is the same answer, so an id cannot be
// probed for. The host who did not send it is a third party like any other.
func TestGetInviteIs404ForAThirdPartyAndForAnUnknownId(t *testing.T) {
	g, _ := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))

	for _, who := range []string{"host", "stranger"} {
		rec := g.get(who, inv.ID)
		if rec.Code != http.StatusNotFound || errorCode(rec) != errInviteNotFound {
			t.Errorf("%s: status = %d, body %s", who, rec.Code, rec.Body)
		}
	}
	other := g.get("alice", "01HZZZZZZZZZZZZZZZZZZZZZZZ")
	if other.Code != http.StatusNotFound || errorCode(other) != errInviteNotFound {
		t.Errorf("unknown id: %d %s", other.Code, other.Body)
	}
	// The third party's answer is byte for byte the unknown id's.
	if g.get("host", inv.ID).Body.String() != other.Body.String() {
		t.Error("a third party can tell a real invite from a made-up one")
	}
}

// What the card reads follows the invite through every answer, for both people.
func TestGetInviteFollowsTheInviteThroughItsAnswers(t *testing.T) {
	g, _ := standardRoom(t)
	accepted := decodeInvite(t, g.invite("room1", "bob", "alice"))
	g.befriend("host", "dave")
	declined := decodeInvite(t, g.invite("room1", "host", "dave"))

	if rec := g.accept("alice", accepted.ID); rec.Code != http.StatusOK {
		t.Fatalf("accept: %d", rec.Code)
	}
	if rec := g.decline("dave", declined.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("decline: %d", rec.Code)
	}

	for who, want := range map[string]InviteState{"bob": InviteAccepted, "alice": InviteAccepted} {
		if got := decodeInvite(t, g.get(who, accepted.ID)).State; got != want {
			t.Errorf("%s reads the accepted invite as %s, want %s", who, got, want)
		}
	}
	for who, want := range map[string]InviteState{"host": InviteDeclined, "dave": InviteDeclined} {
		if got := decodeInvite(t, g.get(who, declined.ID)).State; got != want {
			t.Errorf("%s reads the declined invite as %s, want %s", who, got, want)
		}
	}
	// And the one read GET /invites cannot make: an answered invite is no longer
	// listed, but it is still readable.
	if len(g.list("alice")) != 0 {
		t.Error("setup: an answered invite should not be listed")
	}
}

// A pending invite that runs out of time reads as expired, to both, on the way.
func TestGetInviteReadsALapsedInviteAsExpired(t *testing.T) {
	g, _ := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	g.advance(InviteTTL)
	for _, who := range []string{"bob", "alice"} {
		if got := decodeInvite(t, g.get(who, inv.ID)).State; got != InviteExpired {
			t.Errorf("%s reads %s, want expired", who, got)
		}
	}
}

// Once the reaper has pruned it the id is a 404, and a 404 is what a card reads as
// EXPIRED. The message that carried it is untouched - rooms has no way to reach it
// - and still says what was said.
func TestTheMessageOutlivesTheInviteAndTheInviteIs404(t *testing.T) {
	g, room := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	minted := g.mintedMessages()
	if len(minted) != 1 {
		t.Fatal("setup: no message")
	}

	// The room ends and the reaper's sweep has been past the invite's hour.
	room.Close()
	g.h.hub.Remove("room1")
	g.advance(InviteTTL + time.Second)
	g.h.hub.Reap(g.clock, emptyGrace)

	for _, who := range []string{"bob", "alice"} {
		rec := g.get(who, inv.ID)
		if rec.Code != http.StatusNotFound || errorCode(rec) != errInviteNotFound {
			t.Errorf("%s: status = %d, body %s, want 404 invite_not_found", who, rec.Code, rec.Body)
		}
	}
	// Nothing here took the message back.
	if after := g.mintedMessages(); len(after) != 1 || after[0] != minted[0] {
		t.Errorf("minted changed: %+v -> %+v", minted, after)
	}
}

// An ended room's invite reads as expired until it is pruned, not as a 404.
func TestGetInviteOfAnEndedRoomReadsAsExpiredUntilPruned(t *testing.T) {
	g, room := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	room.Close()
	g.h.hub.Remove("room1")
	for _, who := range []string{"bob", "alice"} {
		if got := decodeInvite(t, g.get(who, inv.ID)).State; got != InviteExpired {
			t.Errorf("%s reads %s, want expired", who, got)
		}
	}
}
