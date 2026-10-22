package rooms

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Room invites: "come to my room", held by the server.
//
// NOT the room's `invitation` in the client (core/realtime/rooms.ts) - that is
// the question a guest who is ALREADY in the room is asked when the host points
// it at a game. This is about someone who is not in the room yet. Two questions,
// two things; never fuse them.
//
// AN INVITE IS OWNED BY ITS ROOM, IN MEMORY, AND DIES WITH IT. There is no table
// and no migration on purpose: an invite points at a room, rooms are ephemeral,
// and a database row would outlive the one thing it points at and be garbage the
// moment the process restarts. What made the old invite (a `bombers://room/<id>`
// string regex'd out of DM text) a shortcut was never where it lived - it was
// that the server held no state at all: no record, no expiry, no author, and
// anyone who typed the string got in. Holding it here fixes every one of those.
//
// The recipient being offline is the requirement, and it is met by LISTING
// (GET /invites), never by the socket. A notify frame is a nudge to a live
// connection; it may land on no one. Nothing in this file may depend on whether
// the invitee was listening.

// InviteState is where an invite stands.
type InviteState string

const (
	InvitePending  InviteState = "pending"
	InviteAccepted InviteState = "accepted"
	InviteDeclined InviteState = "declined"
	// InviteExpired is "pending, but out of time or out of room". It is reached
	// only by the lazy check on a read, by the room ending, and by the reaper's
	// sweep - and only ever from pending, so an answered invite never becomes one.
	InviteExpired InviteState = "expired"
)

// InviteTTL is how long an invite stays answerable: long enough to come back from
// dinner, short enough that a Join never shows up for something nobody is still
// doing. The room ending cuts it short.
const InviteTTL = time.Hour

// maxInvitesPerRoom bounds what a room remembers. Declined invites cannot be
// forgotten (a decline is final for the room's life), so without a bound a
// member could grow a room's map for as long as it stays open.
const maxInvitesPerRoom = 64

var (
	ErrNotMember      = errors.New("not in this room")
	ErrInviteNotFound = errors.New("invite not found")
	ErrInviteExpired  = errors.New("invite expired")
	ErrInviteAnswered = errors.New("invite already answered")
	ErrTooManyInvites = errors.New("too many invites")
)

// Invite is an invitation as the wire describes it, and also what a room stores.
// `RoomName` and the effective `State` are filled in when a copy is taken (see
// viewLocked), so what is stored never goes stale against the room's name.
//
// `To`/`ToName` are beyond the shape the plan first drew: the inviter is told
// when an invite is answered, and a frame that names the room and the inviter
// but not WHO answered says nothing the inviter can use.
type Invite struct {
	ID        string      `json:"id"`
	Room      string      `json:"room"`
	RoomName  string      `json:"room_name"`
	From      string      `json:"from"`
	FromName  string      `json:"from_name"`
	To        string      `json:"to"`
	ToName    string      `json:"to_name"`
	State     InviteState `json:"state"`
	ExpiresAt time.Time   `json:"expires_at"`
}

// settleLocked turns a pending invite that has run out of time, or whose room has
// ended, into an expired one - LAZILY, on whatever read comes along. There is no
// timer per invite; the clock is only consulted when someone asks. EVERY read of
// an invite goes through this (via viewLocked or directly) - a read that skips it
// would show "pending" for something nobody can accept. Caller holds r.mu.
func (r *Room) settleLocked(inv *Invite, now time.Time) {
	if inv.State == InvitePending && (r.closed || !now.Before(inv.ExpiresAt)) {
		r.expireLocked(inv)
	}
}

// copyLocked is the one place an invite is copied for the outside, with the room's
// name as it is now. Every invite that leaves this package - a response body or a
// pushed frame - is built here, so the two cannot grow different shapes. Caller
// holds r.mu.
func (r *Room) copyLocked(inv *Invite) Invite {
	out := *inv
	out.RoomName = r.name
	return out
}

// viewLocked settles the invite and returns a copy for the outside. Caller holds
// r.mu.
func (r *Room) viewLocked(inv *Invite, now time.Time) Invite {
	r.settleLocked(inv, now)
	return r.copyLocked(inv)
}

// expireLocked is the ONLY place a pending invite becomes expired, and it is
// where the invitee is owed a frame. The transition happens once per invite (it
// is only ever made from pending, and expired is final), so recording the news
// here is what makes "told exactly once" true however the invite got there: a
// read that found it lapsed, the room closing, or the reaper's sweep. Accepted
// and declined invites never come through here, so a settled answer is never
// contradicted.
//
// It only QUEUES the news (r.expired); pushing is flushExpired's job, with no
// lock held. Caller holds r.mu.
func (r *Room) expireLocked(inv *Invite) {
	inv.State = InviteExpired
	r.expired = append(r.expired, r.copyLocked(inv))
}

// disposeInvitesLocked is the room ending: every invite still pending becomes
// expired, so a holder of one can never read "pending" for a room that is gone.
// Called from Close with r.mu held.
func (r *Room) disposeInvitesLocked() {
	for _, inv := range r.invites {
		if inv.State == InvitePending {
			r.expireLocked(inv)
		}
	}
}

// flushExpired hands the invites that expired since the last flush to the room's
// sink, which tells each invitee. It takes r.mu only to swap the queue out and
// calls the sink with NO lock held: the sink reaches the notify hub, and a push
// made under r.mu would hold the room across code this package does not own.
//
// THE RULE: every method that can settle an invite must run this AFTER it has
// released r.mu - `defer r.flushExpired()` BEFORE the Lock, so the deferred
// calls unwind Unlock first - or the news sits in the queue until some other
// call happens along. A method that settles and does not flush is a silent
// expiry. Nothing here asks whether the invitee is listening; the frame is a
// nudge and GET /invites is the truth.
func (r *Room) flushExpired() {
	r.mu.Lock()
	queued := r.expired
	r.expired = nil
	sink := r.onExpired
	r.mu.Unlock()

	if sink == nil {
		return
	}
	for _, inv := range queued {
		sink(inv)
	}
}

// lapse settles one invite by id on behalf of the reaper, so an invite whose hour
// ran out with nobody reading it is still announced before the index forgets it.
func (r *Room) lapse(inviteID string, now time.Time) {
	defer r.flushExpired()
	r.mu.Lock()
	defer r.mu.Unlock()
	if inv, ok := r.invites[inviteID]; ok {
		r.settleLocked(inv, now)
	}
}

// Invite asks `inviteeID` to this room on behalf of `byUserID`, who must be IN the
// room right now. It returns the invite and whether it is new.
//
//   - A live pending invite to the same person is returned as it is, `created`
//     false: asking twice is one invitation.
//   - A DECLINED one is returned too, still declined, for the life of the room:
//     the inviter sees the answer, and a no is not something to ask past.
//   - An accepted or lapsed one is history, and a new invite is made.
//
// The friendship rules are the caller's (they need the database); this owns who
// may ask and what an invite is.
func (r *Room) Invite(byUserID, inviteeID, inviteeName string, now time.Time) (inv Invite, created bool, err error) {
	defer r.flushExpired()
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return Invite{}, false, ErrRoomClosed
	}
	by, ok := r.members[byUserID]
	if !ok {
		return Invite{}, false, ErrNotMember
	}

	var live *Invite
	for _, existing := range r.invites {
		if existing.To != inviteeID {
			continue
		}
		r.settleLocked(existing, now)
		switch existing.State {
		case InviteDeclined:
			return r.viewLocked(existing, now), false, nil
		case InvitePending:
			live = existing
		}
	}
	if live != nil {
		return r.viewLocked(live, now), false, nil
	}

	if len(r.invites) >= maxInvitesPerRoom {
		r.forgetSpentInvitesLocked(now)
		if len(r.invites) >= maxInvitesPerRoom {
			return Invite{}, false, ErrTooManyInvites
		}
	}

	made := &Invite{
		ID:        ulid.Make().String(),
		Room:      r.ID,
		From:      byUserID,
		FromName:  by.Username,
		To:        inviteeID,
		ToName:    inviteeName,
		State:     InvitePending,
		ExpiresAt: now.UTC().Add(InviteTTL).Truncate(time.Second),
	}
	if r.invites == nil {
		r.invites = map[string]*Invite{}
	}
	r.invites[made.ID] = made
	// r.mu -> index lock is the only order these two are ever taken in.
	r.idx.add(inviteeID, made.ID, r, made.ExpiresAt)
	return r.viewLocked(made, now), true, nil
}

// forgetSpentInvitesLocked drops what no longer says anything: accepted and
// lapsed invites. Declined ones stay - that is the finality. Caller holds r.mu.
func (r *Room) forgetSpentInvitesLocked(now time.Time) {
	for id, inv := range r.invites {
		r.settleLocked(inv, now)
		if inv.State == InviteAccepted || inv.State == InviteExpired {
			delete(r.invites, id)
		}
	}
}

// AnswerInvite records the invitee's answer. It refuses anything that is not a
// live pending invite addressed to `userID`:
//
//   - ErrInviteNotFound - no such invite, or not for you (indistinguishable).
//   - ErrInviteExpired  - out of time, or the room ended. The invite is returned
//     so the caller can say so.
//   - ErrInviteAnswered - already accepted or declined; the answer stands.
//
// Accepting joins nothing. The caller hands back the room id and the client opens
// the room's socket as it always has, through the same gate.
func (r *Room) AnswerInvite(inviteID, userID string, accept bool, now time.Time) (Invite, error) {
	defer r.flushExpired()
	r.mu.Lock()
	defer r.mu.Unlock()

	inv, ok := r.invites[inviteID]
	if !ok || inv.To != userID {
		return Invite{}, ErrInviteNotFound
	}
	r.settleLocked(inv, now)
	switch inv.State {
	case InviteExpired:
		return r.viewLocked(inv, now), ErrInviteExpired
	case InvitePending:
	default:
		return r.viewLocked(inv, now), ErrInviteAnswered
	}
	if accept {
		inv.State = InviteAccepted
	} else {
		inv.State = InviteDeclined
	}
	return r.viewLocked(inv, now), nil
}

// pendingInviteFor is one invite as `userID` should see it in their listing, or
// false when it is not theirs or is no longer pending.
func (r *Room) pendingInviteFor(inviteID, userID string, now time.Time) (Invite, bool) {
	defer r.flushExpired()
	r.mu.Lock()
	defer r.mu.Unlock()
	inv, ok := r.invites[inviteID]
	if !ok || inv.To != userID {
		return Invite{}, false
	}
	v := r.viewLocked(inv, now)
	return v, v.State == InvitePending
}

// HasMember reports whether `userID` is in the room at this moment.
func (r *Room) HasMember(userID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.members[userID]
	return ok
}

// inviteIndex answers "what is waiting for this user" without anyone walking
// every room. It is a LOCATOR and nothing more - the invite itself lives in its
// room - so there is one copy of the truth, and the index can be wrong only by
// pointing at a room that has since ended, which the room itself then reports as
// expired.
//
// Lock order: a room's mu may be held while taking this lock, never the other way
// round. Nothing here calls into a room while holding it.
type inviteIndex struct {
	mu     sync.Mutex
	byUser map[string]map[string]indexEntry // invitee -> invite id -> where
}

type indexEntry struct {
	room      *Room
	expiresAt time.Time
}

func newInviteIndex() *inviteIndex {
	return &inviteIndex{byUser: map[string]map[string]indexEntry{}}
}

func (x *inviteIndex) add(userID, inviteID string, r *Room, expiresAt time.Time) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	set := x.byUser[userID]
	if set == nil {
		set = map[string]indexEntry{}
		x.byUser[userID] = set
	}
	set[inviteID] = indexEntry{room: r, expiresAt: expiresAt}
}

// lookup finds the room that holds `inviteID` among those addressed to `userID`.
// Scoping it to the user is what makes someone else's invite id a plain miss.
func (x *inviteIndex) lookup(userID, inviteID string) *Room {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.byUser[userID][inviteID].room
}

type indexRef struct {
	user string
	id   string
	room *Room
}

func (x *inviteIndex) forUser(userID string) []indexRef {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]indexRef, 0, len(x.byUser[userID]))
	for id, e := range x.byUser[userID] {
		out = append(out, indexRef{user: userID, id: id, room: e.room})
	}
	return out
}

// lapsed lists the entries whose invite has run out its hour, WITHOUT removing
// them. The reaper settles each one's room (which announces any that were still
// pending) and only then drops exactly this list: dropping first would leave the
// id reading as `404 invite_not_found` to someone who was never told it expired.
// It returns a list rather than calling the rooms because the index never calls
// into a room while holding its lock.
func (x *inviteIndex) lapsed(now time.Time) []indexRef {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []indexRef
	for uid, set := range x.byUser {
		for id, e := range set {
			if !now.Before(e.expiresAt) {
				out = append(out, indexRef{user: uid, id: id, room: e.room})
			}
		}
	}
	return out
}

// drop forgets the given entries, and only those. It takes the list `lapsed`
// returned rather than asking the clock again, so nothing is forgotten that was
// not first settled and announced. An ended room's invites stay readable as
// `expired` until their hour and not a moment longer, which is also what lets
// the ended room itself be collected.
func (x *inviteIndex) drop(refs []indexRef) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, ref := range refs {
		set := x.byUser[ref.user]
		delete(set, ref.id)
		if len(set) == 0 {
			delete(x.byUser, ref.user)
		}
	}
}

// InvitesFor lists the invites waiting on `userID` across every room, newest
// first. Only PENDING ones: an answered, lapsed or ended invite is not waiting on
// anyone. This is the truth that a missed notify frame is recovered from.
//
// It asks the per-user index rather than walking the hub, so its cost is the
// user's own invites and it never takes the hub lock.
func (h *Hub) InvitesFor(userID string, now time.Time) []Invite {
	refs := h.invites.forUser(userID)
	out := make([]Invite, 0, len(refs))
	for _, ref := range refs {
		if inv, ok := ref.room.pendingInviteFor(ref.id, userID, now); ok {
			out = append(out, inv)
		}
	}
	// ULIDs sort by creation time, so the id is the order.
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// AnswerInvite finds the invite among those addressed to `userID` and records
// their answer.
func (h *Hub) AnswerInvite(userID, inviteID string, accept bool, now time.Time) (Invite, error) {
	room := h.invites.lookup(userID, inviteID)
	if room == nil {
		return Invite{}, ErrInviteNotFound
	}
	return room.AnswerInvite(inviteID, userID, accept, now)
}
