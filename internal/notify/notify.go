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
// The one exception is `dm`, which carries the message. The rule is right for
// `profile`, `friend` and `record`, which fan out to many people and could leak
// by getting a list wrong. It is wrong for a direct message, which is addressed
// to exactly the two people in the conversation — the ids come from the message
// row itself, so there is no broadcast list to get wrong — and a chat that makes
// you fetch after every doorbell is not a chat. messaging builds the frame and
// decides who gets it; this package still only delivers what it is handed.
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
	// KindTransfer — a friend has a file for you and is waiting in a room to send
	// it (POST /transfers/offer). `d` carries {"from": "<sender id>", "room":
	// "<room id>", "name": "<filename>", "size": <bytes>}. Sent to that one friend
	// only, stores nothing, and is meant to be allowed to go missing: both ends have
	// to be live for the transfer to happen at all, so an offer nobody was there to
	// hear is not something to queue. The sender learns that from its own room timing
	// out, never from this package.
	KindTransfer = "transfer"
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
}

func NewHub() *Hub {
	return &Hub{byUser: make(map[string]map[*conn]struct{})}
}

func (h *Hub) add(userID string, c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.byUser[userID]
	if set == nil {
		set = make(map[*conn]struct{})
		h.byUser[userID] = set
	}
	set[c] = struct{}{}
}

func (h *Hub) remove(userID string, c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.byUser[userID]
	if set == nil {
		return
	}
	delete(set, c)
	// Drop the empty set rather than leaving a key behind for every user who has
	// ever signed in — this map outlives every connection in it.
	if len(set) == 0 {
		delete(h.byUser, userID)
	}
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

// Listening reports whether a user has any socket open. Only for tests and the
// console's status line — no behaviour should depend on it, because the answer
// can change between asking and acting on it.
func (h *Hub) Listening(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byUser[userID]) > 0
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
