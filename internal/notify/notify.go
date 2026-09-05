// Package notify is the nudge channel: one socket per signed-in user, carrying
// "something you care about changed, go and look".
//
// It carries NO CONTENT, and that is the whole design. A nudge is `{"t":"dm"}`,
// not the message; the client hears it and re-reads through the ordinary HTTP
// API, which already knows who is allowed to see what. So this package needs no
// permission model of its own, cannot leak anything by getting a broadcast list
// slightly wrong, and stays true to the product rule that the server is a
// published copy plus a nudge — not a live mirror.
//
// It is separate from `rooms`, which is also WebSockets, because the two have
// nothing in common but the transport. A room socket is scoped to a room, dies
// with it, and relays opaque frames between members at speed. This one is
// scoped to a PERSON, lives as long as they're signed in, and only ever says
// that something is stale. Fusing them would have meant a room's lifecycle
// deciding whether your messages arrive.
//
// Delivery is best-effort on purpose. A nudge that doesn't land costs a few
// seconds of staleness — every listener also refreshes on its own schedule and
// when its screen is opened — so nothing here blocks, queues for later, or
// retries. A slow client is dropped rather than allowed to hold up a database
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
	// KindDM — a direct message arrived. `d` carries {"from": "<user id>"} so a
	// client showing one conversation can ignore the rest.
	KindDM = "dm"
	// KindProfile — someone whose card you may hold has changed their details.
	KindProfile = "profile"
	// KindFriend — a friend request arrived, or one you sent was accepted.
	KindFriend = "friend"
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
