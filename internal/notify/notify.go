// Package notify is the nudge channel: one socket per signed-in user, carrying
// "something you care about changed, go and look".
//
// It carries NO CONTENT, and that is the whole design. A nudge is
// `{"t":"profile","d":{"from":"<user id>"}}` — what changed and whose it is —
// never the thing itself; the client hears it and re-reads through the ordinary
// HTTP API, which already knows who is allowed to see what. So this package needs
// no permission model of its own, cannot leak anything by getting a broadcast
// list slightly wrong, and stays true to the product rule that the server is a
// published copy plus a nudge — not a live mirror.
//
// The exceptions are `dm`, which carries the message, and `invite`, which carries the
// invite. The rule is right for `profile`, `friend` and `record`, which fan out to
// many people and could leak by getting a list wrong. It is wrong for a direct
// message, which is addressed to exactly the two people in the conversation — the
// ids come from the message row itself, so there is no broadcast list to get wrong —
// and a chat that makes you fetch after every doorbell is not a chat. messaging
// builds the frame and decides who gets it; this package still only delivers what it
// is handed. An invite is the same shape of exception: it goes to the one person
// asked, or to the one who asked, and rooms builds it from the invite itself.
//
// It is separate from `rooms`, which is also WebSockets, because the two have
// nothing in common but the transport. A room socket is scoped to a room, dies
// with it, and relays opaque frames between members at speed. This one is
// scoped to a PERSON, lives as long as they're signed in, and only ever says
// that something is stale. Fusing them would have meant a room's lifecycle
// deciding whether your messages arrive.
//
// Delivery is best-effort on purpose. A nudge that doesn't land only leaves
// something stale — every screen also re-reads when it's opened and after the
// socket reconnects, and some on a timer too — so nothing here blocks, queues
// for later, or retries. A slow client is dropped rather than allowed to hold up a database
// write somewhere else in the process.
package notify

import (
	"encoding/json"
	"sync"

	"github.com/tabuhana/bombers-server/internal/logx"
)

// The kinds of nudge. Small and closed on purpose: each one exists because a
// screen in the client re-reads something when it hears it, and a kind nobody
// listens for is just noise on a wire.
const (
	// KindDM — something happened to a direct message: it arrived, was edited, was
	// deleted, was pinned or unpinned, or had a reaction added or removed. `d` carries
	// {"op": "new"|"edit"|"delete"|"pin"|"react", "from": "<sender id>", "message":
	// {...}} and, on "new" only, the sender's "nonce" if they gave one. `from` is
	// always the message's author, whoever acted. A pin and an unpin are both "pin",
	// and a reaction added and one removed both "react": the message says which, by
	// its pinned_at or its reactions. The message is as the RECEIVING person sees it
	// (their `me` on each reaction), so each of the two gets a frame of their own.
	// Sent to both people in the conversation, so the sender's other windows and
	// devices stay in step. The only kind that carries content (see above).
	KindDM = "dm"
	// KindProfile — someone whose card you may hold changed it: their details
	// (PUT /me/profile) or their avatar or banner. Sent to every accepted friend.
	// `d` carries {"from": "<owner id>"} so a client re-reads that one card rather
	// than every card it holds.
	KindProfile = "profile"
	// KindFriend — a friend request arrived, one you sent was accepted or
	// rejected, or a friend removed you.
	KindFriend = "friend"
	// KindRecord — a node put something in front of you, changed it, withdrew
	// it, or somebody answered one of yours (internal/noderecords). `d` carries
	// {"node": "<node id>", "from": "<writer id>"}, so the client wakes the one
	// node concerned instead of every node that has ever sent anything.
	KindRecord = "record"
	// KindTyping — a friend is composing a message to you right now
	// (POST /messages/{userID}/typing). `d` carries {"from": "<typist id>"}.
	// Sent to that one friend only, stores nothing, and is meant to be allowed to
	// go missing: the client shows the indicator for a few seconds and lets it
	// lapse.
	KindTyping = "typing"
	// KindRead — a friend read your side of the conversation
	// (POST /messages/{userID}/read). `d` carries {"from": "<reader id>", "at":
	// "<RFC 3339>", "id": "<message id>"}, where `at` and `id` together are their
	// new read position, so the sender can mark everything up to that position as
	// seen. Sent to that one friend, and only when the marker actually
	// moved.
	KindRead = "read"
	// KindInvite — someone asked you to a room, or answered an invite you sent
	// (internal/rooms). `d` carries the whole invite, {"id", "room", "room_name",
	// "from", "from_name", "to", "to_name", "state", "expires_at"}, and its `state`
	// says which change this is: "pending" to the invitee when it is made,
	// "accepted" or "declined" to the inviter AND the answerer when answered (the
	// answerer's other windows settle on the same answer), and "expired" to the
	// invitee ALONE when a pending invite lapses - its room ended, or its hour ran
	// out. The inviter is not told of an expiry: a room they were in ending is
	// something they can see, and nobody asked them a question. One kind for every
	// change, like KindDM. It carries content because it is addressed to exactly the
	// two people it concerns, whose ids come from the invite itself — and it is
	// STILL only a nudge: the server holds the invite and GET /invites lists it, so
	// a frame that lands on no socket loses nothing.
	KindInvite = "invite"
	// KindPresence — a friend arrived or left: their app connected, their last
	// window closed, or they chose a status. `d` carries {"from": "<their id>"} and
	// NOT the status, although the hub is the one place that knows it — presence
	// fans out to a whole friends list, which is the case the no-content rule is
	// for, and "appear offline" is exactly the thing a frame addressed one friend
	// too wide would give away. The client re-reads GET /presence, which applies
	// the friend gate and the invisible rule.
	//
	// Sent to every accepted friend. Nothing is sent for an INVISIBLE user's
	// socket arriving or going, because `{"from": X}` would say X is at their
	// computer, which is the one thing they asked not to say.
	KindPresence = "presence"
)

// sendQueue bounds one connection's backlog. These are tiny and infrequent, so
// a client that can't drain even this is not one worth waiting for.
const sendQueue = 16

// Frame is what goes down the wire. Deliberately the same envelope shape as a
// room frame, so the client's socket plumbing reads the same either side.
type Frame struct {
	T string `json:"t"`
	D any    `json:"d,omitempty"`
}

// conn is one open socket. The channel is the only thing the hub touches, so
// the transport can be swapped without the hub knowing.
type conn struct {
	out chan []byte
}

// Hub tracks who is listening. A user has several connections more often than
// you'd think — two windows, a desktop and a laptop — so this is a set per user
// rather than a single socket, and all of them get told.
type Hub struct {
	mu     sync.RWMutex
	byUser map[string]map[*conn]struct{}
	// Told when a user's FIRST socket arrives and when their LAST one goes — the
	// two moments presence is made of. WHICH of the two is not passed: the hook's
	// job is to say somebody's presence moved, and every reader answers that by
	// asking the hub again, so a flag would be a second copy of an answer that is
	// already one call away. Set once at wiring, read without a lock because
	// nothing writes it after that.
	onPresence func(userID string)
}

func NewHub() *Hub {
	return &Hub{byUser: make(map[string]map[*conn]struct{})}
}

// OnPresence registers the hook. Called ONCE, during wiring, before anything can
// connect: there is no lock around the field, because a hub whose hook changes
// under a live connection is not a thing that should be possible.
func (h *Hub) OnPresence(fn func(userID string)) { h.onPresence = fn }

// announce runs the hook OUTSIDE the lock. It does real work — a friends query
// and a fan-out back through this hub — so calling it under h.mu would deadlock
// the moment it sent anything.
func (h *Hub) announce(userID string) {
	if h.onPresence == nil {
		return
	}
	h.onPresence(userID)
}

// add registers a socket and reports whether it is this user's first: a second
// window is not an arrival, and telling their friends about one would be a flap
// nobody asked to see.
func (h *Hub) add(userID string, c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.byUser[userID]
	if set == nil {
		set = make(map[*conn]struct{})
		h.byUser[userID] = set
	}
	first := len(set) == 0
	set[c] = struct{}{}
	return first
}

// remove deregisters a socket and reports whether it was this user's last — the
// moment they are gone, and the only one worth announcing.
func (h *Hub) remove(userID string, c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	// A set is dropped the moment it empties (below), so a user with nothing open
	// has no entry at all: that is what makes removing the same socket twice
	// report one departure, with no need to check whether this one is held.
	set := h.byUser[userID]
	if set == nil {
		return false
	}
	delete(set, c)
	// Drop the empty set rather than leaving a key behind for every user who has
	// ever signed in — this map outlives every connection in it.
	if len(set) == 0 {
		delete(h.byUser, userID)
		return true
	}
	return false
}

// Send nudges one user on every socket they have open.
//
// Never blocks and never errors. Callers are in the middle of doing the real
// work — storing a message, saving a profile — and a notification is not
// allowed to make any of that slower or more fallible than it was.
func (h *Hub) Send(userID, kind string, data any) {
	if h == nil || userID == "" {
		return
	}
	frame, err := json.Marshal(Frame{T: kind, D: data})
	if err != nil {
		logx.Error("notify: encode %s: %v", kind, err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.byUser[userID] {
		select {
		case c.out <- frame:
		default:
			// Backed up. Dropping is correct: the client will catch this on its
			// next refresh, and blocking here would punish an unrelated request.
		}
	}
}

// SendMany nudges several users with one encode. Used where a single change is
// interesting to a whole friends list.
func (h *Hub) SendMany(userIDs []string, kind string, data any) {
	for _, id := range userIDs {
		h.Send(id, kind, data)
	}
}

// Listening reports whether a user has any socket open — which is to say
// whether an app of theirs is running, now. `presence` reads it (through
// ConnectedAmong) and that IS the behaviour: a dot is a statement about this
// moment, and the answer going stale between asking and drawing is corrected by
// the frame the change itself sends. It replaced a stored heartbeat, which could
// only ever say "was running, within three minutes".
func (h *Hub) Listening(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byUser[userID]) > 0
}

// ConnectedAmong answers for a list in one pass under one lock, because the
// caller is a friends list and asking per id would be a lock acquisition each.
// Ids with nothing open are ABSENT from the map rather than false, so a caller
// that forgets to check reads "not connected" either way.
func (h *Hub) ConnectedAmong(userIDs []string) map[string]bool {
	out := make(map[string]bool, len(userIDs))
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, id := range userIDs {
		if len(h.byUser[id]) > 0 {
			out[id] = true
		}
	}
	return out
}

// Count is how many sockets are open in total, for `status`.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, set := range h.byUser {
		n += len(set)
	}
	return n
}
