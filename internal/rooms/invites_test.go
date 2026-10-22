package rooms

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tabuhana/bombers-server/internal/auth"
)

// A recorded nudge: who was told, under what kind, and what the invite said.
type nudged struct {
	to   string
	kind string
	inv  Invite
}

// inviteRig is a handler with no database and no real clock: friendships are a
// set, usernames a map, the clock a variable, and every nudge is written down.
type inviteRig struct {
	t       *testing.T
	h       *Handler
	mu      sync.Mutex
	friends map[[2]string]bool
	clock   time.Time
	sent    []nudged
}

func newInviteRig(t *testing.T) *inviteRig {
	g := &inviteRig{t: t, friends: map[[2]string]bool{}, clock: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)}
	g.h = NewHandler(nil, nil, func(userIDs []string, kind string, data any) {
		inv, _ := data.(Invite)
		g.mu.Lock()
		defer g.mu.Unlock()
		for _, id := range userIDs {
			g.sent = append(g.sent, nudged{to: id, kind: kind, inv: inv})
		}
	})
	g.h.friends = func(_ context.Context, a, b string) (bool, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.friends[pair(a, b)], nil
	}
	g.h.username = func(_ context.Context, id string) string { return "name-" + id }
	g.h.now = func() time.Time {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.clock
	}
	return g
}

func pair(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

func (g *inviteRig) befriend(a, b string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.friends[pair(a, b)] = true
}

func (g *inviteRig) advance(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.clock = g.clock.Add(d)
}

// room makes a room hosted by `host` with the listed people sitting in it.
func (g *inviteRig) room(id, host string, members ...string) *Room {
	room := g.h.hub.Create(id, "amber-lantern", host, g.clock)
	for _, m := range append([]string{host}, members...) {
		if _, err := room.Join(m, "name-"+m, &fakeConn{}); err != nil {
			g.t.Fatal(err)
		}
	}
	return room
}

func (g *inviteRig) nudges(to string) []nudged {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []nudged
	for _, n := range g.sent {
		if n.to == to {
			out = append(out, n)
		}
	}
	return out
}

func (g *inviteRig) do(fn http.HandlerFunc, method, path, userID string, params map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	req = req.WithContext(auth.WithUserID(ctx, userID))
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

func (g *inviteRig) invite(roomID, as, target string) *httptest.ResponseRecorder {
	return g.do(g.h.CreateInvite, http.MethodPost, "/rooms/"+roomID+"/invites", as,
		map[string]string{"roomID": roomID}, `{"user_id":"`+target+`"}`)
}

func (g *inviteRig) list(as string) []Invite {
	rec := g.do(g.h.ListInvites, http.MethodGet, "/invites", as, nil, "")
	if rec.Code != http.StatusOK {
		g.t.Fatalf("list status = %d", rec.Code)
	}
	var out struct {
		Invites []Invite `json:"invites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		g.t.Fatal(err)
	}
	return out.Invites
}

func (g *inviteRig) accept(as, id string) *httptest.ResponseRecorder {
	return g.do(g.h.AcceptInvite, http.MethodPost, "/invites/"+id+"/accept", as, map[string]string{"inviteID": id}, "")
}

func (g *inviteRig) decline(as, id string) *httptest.ResponseRecorder {
	return g.do(g.h.DeclineInvite, http.MethodPost, "/invites/"+id+"/decline", as, map[string]string{"inviteID": id}, "")
}

func decodeInvite(t *testing.T, rec *httptest.ResponseRecorder) Invite {
	t.Helper()
	var inv Invite
	if err := json.Unmarshal(rec.Body.Bytes(), &inv); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return inv
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var b struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return b.Error
}

// The standard cast: host and bob are in the room, alice is the host's friend
// and bob's friend, and so can be asked.
func standardRoom(t *testing.T) (*inviteRig, *Room) {
	g := newInviteRig(t)
	room := g.room("room1", "host", "bob")
	g.befriend("host", "bob")
	g.befriend("host", "alice")
	g.befriend("bob", "alice")
	return g, room
}

func TestInviteShape(t *testing.T) {
	g, _ := standardRoom(t)
	rec := g.invite("room1", "bob", "alice")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	want := map[string]any{
		"room": "room1", "room_name": "amber-lantern", "from": "bob", "from_name": "name-bob",
		"to": "alice", "to_name": "name-alice", "state": "pending", "expires_at": "2026-10-22T11:00:00Z",
	}
	for k, v := range want {
		if raw[k] != v {
			t.Errorf("%s = %v, want %v", k, raw[k], v)
		}
	}
	if id, _ := raw["id"].(string); len(id) != 26 {
		t.Errorf("id = %v, want a ULID", raw["id"])
	}
}

// Any member may ask, not only the host: the Invite button is on every toolbar.
func TestAnyMemberMayInvite(t *testing.T) {
	g, _ := standardRoom(t)
	if rec := g.invite("room1", "bob", "alice"); rec.Code != http.StatusCreated {
		t.Fatalf("a non-host member: status = %d, body %s", rec.Code, rec.Body)
	}
}

// The refusal is the SAME 404 the join gate gives for a room that is not there, to
// the byte, so an id (room or user) cannot be probed through invites.
func TestNonFriendIsRefusedWithTheOpaque404(t *testing.T) {
	g, _ := standardRoom(t)
	missing := g.do(g.h.Get, http.MethodGet, "/rooms/nope", "bob", map[string]string{"roomID": "nope"}, "")

	// dave is nobody's friend; erin is not bob's friend even though she is the host's.
	g.befriend("host", "erin")
	for _, target := range []string{"dave", "erin"} {
		rec := g.invite("room1", "bob", target)
		if rec.Code != http.StatusNotFound || errorCode(rec) != errRoomNotFound {
			t.Fatalf("%s: status = %d, body %s", target, rec.Code, rec.Body)
		}
		if !bytes.Equal(rec.Body.Bytes(), missing.Body.Bytes()) {
			t.Errorf("%s: body %q differs from the join gate's %q", target, rec.Body, missing.Body)
		}
	}
	if len(g.list("dave")) != 0 || len(g.list("erin")) != 0 {
		t.Error("a refused invite left something behind")
	}
}

// bob's friend carol is a stranger to the host. The join gate would turn her away,
// so inviting her would hand out an invitation that can only fail.
func TestInviteeTheRoomWouldTurnAwayIsRefused(t *testing.T) {
	g, _ := standardRoom(t)
	g.befriend("bob", "carol")
	rec := g.invite("room1", "bob", "carol")
	if rec.Code != http.StatusNotFound || errorCode(rec) != errRoomNotFound {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestNonMemberCannotInvite(t *testing.T) {
	g, _ := standardRoom(t)
	g.befriend("alice", "zed")
	g.befriend("host", "zed")
	// alice is the host's friend and could JOIN, but is not in the room; zed is the
	// one she would ask.
	rec := g.invite("room1", "alice", "zed")
	if rec.Code != http.StatusNotFound || errorCode(rec) != errRoomNotFound {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if len(g.nudges("zed")) != 0 {
		t.Error("a refused invite was pushed")
	}
}

func TestInviteRefusals(t *testing.T) {
	g, room := standardRoom(t)
	if rec := g.invite("room1", "bob", "bob"); rec.Code != http.StatusNotFound {
		t.Errorf("inviting yourself: status = %d", rec.Code)
	}
	if rec := g.invite("nope", "bob", "alice"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown room: status = %d", rec.Code)
	}
	for _, body := range []string{``, `{}`, `{"user_id":""}`, `nonsense`} {
		rec := g.do(g.h.CreateInvite, http.MethodPost, "/rooms/room1/invites", "bob", map[string]string{"roomID": "room1"}, body)
		if rec.Code != http.StatusBadRequest || errorCode(rec) != errInvalidBody {
			t.Errorf("body %q: status = %d, body %s", body, rec.Code, rec.Body)
		}
	}
	room.Close()
	if rec := g.invite("room1", "bob", "alice"); rec.Code != http.StatusNotFound {
		t.Errorf("ended room: status = %d", rec.Code)
	}
}

func TestReinviteReturnsTheSameInvite(t *testing.T) {
	g, _ := standardRoom(t)
	first := g.invite("room1", "bob", "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("first: %d", first.Code)
	}
	// Again, by the same member and by a different one: one pending invite per
	// person per room.
	second := g.invite("room1", "bob", "alice")
	third := g.invite("room1", "host", "alice")
	for name, rec := range map[string]*httptest.ResponseRecorder{"second": second, "third": third} {
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 for the existing invite", name, rec.Code)
		}
		if decodeInvite(t, rec).ID != decodeInvite(t, first).ID {
			t.Errorf("%s: a second invite was made", name)
		}
	}
	if got := len(g.list("alice")); got != 1 {
		t.Errorf("alice has %d invites, want 1", got)
	}
	if got := len(g.nudges("alice")); got != 1 {
		t.Errorf("alice was nudged %d times, want once - a repeat ask is not a new event", got)
	}
}

func TestAcceptHandsBackTheRoomAndJoinsNothing(t *testing.T) {
	g, room := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	rec := g.accept("alice", inv.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["room"] != "room1" || len(got) != 1 {
		t.Fatalf("body = %v, want {room: room1}", got)
	}
	if room.HasMember("alice") {
		t.Error("accepting put alice in the room; the socket is the only way in")
	}
	if len(g.list("alice")) != 0 {
		t.Error("an accepted invite is still listed as pending")
	}
}

func TestAcceptThenDeclineIsRefused(t *testing.T) {
	g, _ := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	if rec := g.accept("alice", inv.ID); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	rec := g.decline("alice", inv.ID)
	if rec.Code != http.StatusConflict || errorCode(rec) != errInviteAnswered {
		t.Fatalf("decline after accept: status = %d, body %s", rec.Code, rec.Body)
	}
	// And the other way round, and twice.
	inv2 := decodeInvite(t, g.invite("room1", "host", "bob"))
	if rec := g.decline("bob", inv2.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("decline: %d", rec.Code)
	}
	for _, rec := range []*httptest.ResponseRecorder{g.accept("bob", inv2.ID), g.decline("bob", inv2.ID)} {
		if rec.Code != http.StatusConflict {
			t.Errorf("answering a declined invite: status = %d", rec.Code)
		}
	}
}

// A decline is final for the room's life: the inviter sees the answer, and asking
// again returns the same declined invite instead of a fresh one to pester with.
func TestDeclineIsFinalForTheRoomsLife(t *testing.T) {
	g, _ := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	g.decline("alice", inv.ID)

	for _, by := range []string{"bob", "host"} {
		rec := g.invite("room1", by, "alice")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s re-invite: status = %d", by, rec.Code)
		}
		again := decodeInvite(t, rec)
		if again.ID != inv.ID || again.State != InviteDeclined {
			t.Errorf("%s re-invite = %+v, want the same declined invite", by, again)
		}
	}
	if len(g.list("alice")) != 0 {
		t.Error("alice has a pending invite after declining")
	}
	// Even after the hour: the room is still alive, so the no still stands.
	g.advance(2 * time.Hour)
	if again := decodeInvite(t, g.invite("room1", "bob", "alice")); again.State != InviteDeclined {
		t.Errorf("after the hour: %+v", again)
	}
}

// An accepted invite is spent, not final: someone who came and went can be asked
// again.
func TestAcceptedInviteDoesNotBlockAFreshOne(t *testing.T) {
	g, _ := standardRoom(t)
	first := decodeInvite(t, g.invite("room1", "bob", "alice"))
	g.accept("alice", first.ID)
	rec := g.invite("room1", "bob", "alice")
	if rec.Code != http.StatusCreated || decodeInvite(t, rec).ID == first.ID {
		t.Fatalf("status = %d: expected a new invite", rec.Code)
	}
}

func TestInviteToAClosedRoomReadsAsExpired(t *testing.T) {
	g, room := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	if len(g.list("alice")) != 1 {
		t.Fatal("setup: invite should be pending")
	}

	room.Close()
	g.h.hub.Remove("room1") // an ended room is unreachable by id; the invite must still say so

	if got := g.list("alice"); len(got) != 0 {
		t.Fatalf("an ended room's invite is still listed: %+v", got)
	}
	rec := g.accept("alice", inv.ID)
	if rec.Code != http.StatusGone || errorCode(rec) != errInviteExpired {
		t.Fatalf("accept: status = %d, body %s", rec.Code, rec.Body)
	}
	if rec := g.decline("alice", inv.ID); rec.Code != http.StatusGone {
		t.Fatalf("decline: status = %d", rec.Code)
	}
	// And at the room level, where the state itself is readable.
	view, err := room.AnswerInvite(inv.ID, "alice", true, g.clock)
	if err != ErrInviteExpired || view.State != InviteExpired {
		t.Fatalf("view = %+v, err = %v", view, err)
	}
}

// The reaper closes rooms through the same path; its invites must follow.
func TestReapedRoomsInvitesExpire(t *testing.T) {
	g := newInviteRig(t)
	room := g.room("room1", "host", "bob")
	g.befriend("host", "bob")
	g.befriend("host", "alice")
	g.befriend("bob", "alice")
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))

	room.Leave("host", g.clock)
	room.Leave("bob", g.clock)
	g.advance(emptyGrace + time.Second)
	if n := g.h.hub.Reap(g.clock, emptyGrace); n != 1 {
		t.Fatalf("reaped %d", n)
	}
	if got := g.list("alice"); len(got) != 0 {
		t.Fatalf("listed: %+v", got)
	}
	if rec := g.accept("alice", inv.ID); rec.Code != http.StatusGone {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestInviteExpiresAfterAnHour(t *testing.T) {
	g, _ := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	if want := g.clock.Add(time.Hour); !inv.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", inv.ExpiresAt, want)
	}

	g.advance(time.Hour - time.Second)
	if got := g.list("alice"); len(got) != 1 || got[0].State != InvitePending {
		t.Fatalf("a second before the hour: %+v", got)
	}

	g.advance(time.Second) // exactly the hour
	if got := g.list("alice"); len(got) != 0 {
		t.Fatalf("at the hour the invite is still listed: %+v", got)
	}
	rec := g.accept("alice", inv.ID)
	if rec.Code != http.StatusGone || errorCode(rec) != errInviteExpired {
		t.Fatalf("accept after the hour: status = %d, body %s", rec.Code, rec.Body)
	}
	// The room is alive, so a lapsed invite is not a no: the person can be asked again.
	if rec := g.invite("room1", "bob", "alice"); rec.Code != http.StatusCreated {
		t.Fatalf("re-invite after lapse: status = %d", rec.Code)
	}
}

// The hub sweep forgets invites that have run out their hour, so the index does
// not grow for as long as the process lives.
func TestReaperForgetsLapsedInvites(t *testing.T) {
	g, _ := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	g.advance(InviteTTL + time.Second)
	g.h.hub.Reap(g.clock, emptyGrace)

	g.h.hub.invites.mu.Lock()
	left := len(g.h.hub.invites.byUser)
	g.h.hub.invites.mu.Unlock()
	if left != 0 {
		t.Errorf("index still holds %d user(s)", left)
	}
	if rec := g.accept("alice", inv.ID); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestInviterIsToldOfTheAnswer(t *testing.T) {
	g, _ := standardRoom(t)

	acc := decodeInvite(t, g.invite("room1", "bob", "alice"))
	g.accept("alice", acc.ID)
	g.befriend("host", "dan")
	g.befriend("bob", "dan")
	dec := decodeInvite(t, g.invite("room1", "bob", "dan"))
	g.decline("dan", dec.ID)

	got := g.nudges("bob")
	if len(got) != 2 {
		t.Fatalf("bob heard %d frames, want 2: %+v", len(got), got)
	}
	for i, want := range []struct {
		id, from string
		state    InviteState
	}{{acc.ID, "alice", InviteAccepted}, {dec.ID, "dan", InviteDeclined}} {
		n := got[i]
		if n.kind != KindInvite || n.inv.ID != want.id || n.inv.State != want.state || n.inv.To != want.from || n.inv.ToName != "name-"+want.from {
			t.Errorf("frame %d = %+v, want %s %s by %s", i, n, want.id, want.state, want.from)
		}
	}
	// The invitee was told it arrived, pending, then (see
	// TestAnswererIsToldTooWithTheSameInvite) her own answer.
	first := g.nudges("alice")
	if len(first) != 2 || first[0].kind != KindInvite || first[0].inv.State != InvitePending || first[0].inv.ID != acc.ID {
		t.Errorf("alice heard %+v", first)
	}
}

// The recipient being away is the requirement, and nothing here may depend on
// whether they were listening: with no one to nudge at all, the invite is still
// there to be listed and answered.
func TestInviteSurvivesNobodyListening(t *testing.T) {
	g, _ := standardRoom(t)
	g.h.nudge = nil
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	if got := g.list("alice"); len(got) != 1 || got[0].ID != inv.ID {
		t.Fatalf("list = %+v", got)
	}
	if rec := g.accept("alice", inv.ID); rec.Code != http.StatusOK {
		t.Fatalf("accept: %d", rec.Code)
	}
}

func TestOnlyTheInviteeCanSeeOrAnswer(t *testing.T) {
	g, _ := standardRoom(t)
	inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
	for _, who := range []string{"bob", "host", "stranger"} {
		for name, rec := range map[string]*httptest.ResponseRecorder{"accept": g.accept(who, inv.ID), "decline": g.decline(who, inv.ID)} {
			if rec.Code != http.StatusNotFound || errorCode(rec) != errInviteNotFound {
				t.Errorf("%s %s: status = %d, body %s", who, name, rec.Code, rec.Body)
			}
		}
		if len(g.list(who)) != 0 {
			t.Errorf("%s can list alice's invite", who)
		}
	}
	// Still pending: none of that touched it.
	if got := g.list("alice"); len(got) != 1 || got[0].State != InvitePending {
		t.Fatalf("list = %+v", got)
	}
}

func TestListSpansRoomsNewestFirst(t *testing.T) {
	g, _ := standardRoom(t)
	g.room("room2", "alice-friend", "bob2")
	g.befriend("alice-friend", "bob2")
	g.befriend("alice-friend", "alice")
	g.befriend("bob2", "alice")

	a := decodeInvite(t, g.invite("room1", "bob", "alice"))
	b := decodeInvite(t, g.invite("room2", "bob2", "alice"))
	got := g.list("alice")
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
		t.Fatalf("list = %+v, want [%s %s]", got, b.ID, a.ID)
	}
	// An empty list is [], never null.
	rec := g.do(g.h.ListInvites, http.MethodGet, "/invites", "nobody", nil, "")
	if strings.TrimSpace(rec.Body.String()) != `{"invites":[]}` {
		t.Errorf("empty list = %s", rec.Body)
	}
}

// The listing reports the room's name as it is now, not as it was at invite time.
func TestInviteShowsTheCurrentRoomName(t *testing.T) {
	g, room := standardRoom(t)
	g.invite("room1", "bob", "alice")
	if err := room.SetName("host", "game night"); err != nil {
		t.Fatal(err)
	}
	if got := g.list("alice"); len(got) != 1 || got[0].RoomName != "game night" {
		t.Fatalf("list = %+v", got)
	}
}

func TestRoomRemembersAtMostSoManyInvites(t *testing.T) {
	g := newInviteRig(t)
	room := g.room("room1", "host")
	for i := 0; i < maxInvitesPerRoom; i++ {
		if _, _, err := room.Invite("host", "u"+string(rune('A'+i/26))+string(rune('a'+i%26)), "", g.clock); err != nil {
			t.Fatalf("invite %d: %v", i, err)
		}
	}
	if _, _, err := room.Invite("host", "one-too-many", "", g.clock); err != ErrTooManyInvites {
		t.Fatalf("err = %v", err)
	}
	// Once they lapse there is room again.
	if _, _, err := room.Invite("host", "one-too-many", "", g.clock.Add(2*InviteTTL)); err != nil {
		t.Fatalf("after the hour: %v", err)
	}
}

// Invites are reached from HTTP handlers, the socket's close path and the reaper at
// once. Run with -race where the toolchain supports it; without it this still
// catches a deadlock or a panic.
func TestInvitesUnderConcurrentUse(t *testing.T) {
	g, room := standardRoom(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rec := g.invite("room1", "bob", "alice")
				if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
					id := decodeInvite(t, rec).ID
					g.list("alice")
					g.accept("alice", id)
					g.decline("alice", id)
				}
				g.h.hub.Reap(g.clock, emptyGrace)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		room.Close()
	}()
	wg.Wait()
	if got := g.list("alice"); len(got) != 0 {
		t.Fatalf("pending invites after the room closed: %+v", got)
	}
}

// The invitee's OTHER windows must settle on the answer too, or they keep showing
// a pending invite that would now 409. The frame is the same invite the inviter
// got, carrying the state that was chosen, and a re-list agrees with it.
func TestAnswererIsToldTooWithTheSameInvite(t *testing.T) {
	for _, accept := range []bool{true, false} {
		g, _ := standardRoom(t)
		inv := decodeInvite(t, g.invite("room1", "bob", "alice"))
		want := InviteDeclined
		if accept {
			g.accept("alice", inv.ID)
			want = InviteAccepted
		} else {
			g.decline("alice", inv.ID)
		}

		mine := g.nudges("alice")
		if len(mine) != 2 {
			t.Fatalf("accept=%v: alice heard %d frames, want 2 (the invite, then her answer): %+v", accept, len(mine), mine)
		}
		got, theirs := mine[1], g.nudges("bob")
		if got.kind != KindInvite || got.inv.ID != inv.ID || got.inv.State != want {
			t.Errorf("accept=%v: answerer's frame = %+v, want %s", accept, got, want)
		}
		if len(theirs) != 1 || theirs[0].inv != got.inv {
			t.Errorf("accept=%v: inviter's frame %+v differs from the answerer's %+v", accept, theirs, got)
		}
		if len(g.list("alice")) != 0 {
			t.Errorf("accept=%v: a re-list still shows the invite the frame said was %s", accept, want)
		}
	}
}
