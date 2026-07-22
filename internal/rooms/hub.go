// Package rooms is the realtime relay behind Bombers activities (games, lobbies).
//
// THE SERVER NEVER LEARNS A GAME'S RULES. It forwards opaque messages between
// the members of a room and tracks who is present — nothing else. Chess move
// validation, scores, turn order and win conditions all live in the clients,
// with one member designated HOST as the referee (see ACTIVITIES.md in the
// client repo). That is what lets a 3D lobby streaming position updates 30x a
// second and a turn-based board game share one implementation.
//
// Rooms are ephemeral and in-memory by design: nothing here is persisted, and a
// room that stays empty past the grace period is reaped. DMs persist forever;
// rooms persist nothing (PRODUCT.md).
package rooms

import (
	"errors"
	"sync"
	"time"
)

// Room control messages the SERVER generates. Namespaced so an activity's own
// message types can never collide with them.
const (
	TypeWelcome = "room:welcome" // sent to the joiner: who's here, who hosts
	TypeJoin    = "room:join"    // broadcast: someone arrived
	TypeLeave   = "room:leave"   // broadcast: someone left (may carry a new host)
	TypeError   = "room:error"   // sent to one member: something was refused
)

var (
	ErrRoomNotFound = errors.New("room not found")
	ErrRoomFull     = errors.New("room full")
)

// maxMembers bounds a single room. Friends-scale by design — this is a game
// lobby, not a broadcast channel.
const maxMembers = 16

// Rate limiting: a token bucket per member, sized for continuous position sync
// (a 3D activity at ~30Hz) with headroom, so only a broken or hostile client
// ever trips it. Tripping drops the message and tells the sender; it never
// closes the socket, because a brief burst shouldn't end a game.
const (
	rateBurst      = 120 // tokens a member may bank
	ratePerSecond  = 60  // refill rate
	rateWindowUnit = float64(time.Second)
)

// Sender is one member's outbound side. The WebSocket connection implements it;
// tests use a fake, which is what keeps every rule in this file testable without
// a network.
type Sender interface {
	// Send delivers one frame. It must not block indefinitely — the
	// implementation drops the frame if the peer is too slow (a stalled member
	// must never stall the room).
	Send(msg []byte)
}

// Member is one participant's presence in a room.
type Member struct {
	UserID   string
	Username string
	conn     Sender

	tokens   float64
	lastFill time.Time
}

// allow reports whether this member may send another message now, consuming a
// token if so.
func (m *Member) allow(now time.Time) bool {
	if m.lastFill.IsZero() {
		m.lastFill = now
		m.tokens = rateBurst
	}
	elapsed := float64(now.Sub(m.lastFill)) / rateWindowUnit
	if elapsed > 0 {
		m.tokens += elapsed * ratePerSecond
		if m.tokens > rateBurst {
			m.tokens = rateBurst
		}
		m.lastFill = now
	}
	if m.tokens < 1 {
		return false
	}
	m.tokens--
	return true
}

// Room is one live session: an activity, its members, and whoever is refereeing.
type Room struct {
	ID         string
	ActivityID string
	// HostID is the referee: the client that validates moves and answers a late
	// joiner's state request. It transfers to the longest-present remaining
	// member when the host leaves, so a session survives its creator dropping.
	HostID    string
	CreatedAt time.Time

	mu      sync.Mutex
	members map[string]*Member
	// order preserves arrival order, so host transfer is deterministic rather
	// than whatever the map iteration happens to yield.
	order []string
	// emptySince marks when the last member left; the reaper uses it. Zero while
	// anyone is present.
	emptySince time.Time
}

// MemberInfo is the wire-safe view of a member (presence lists, join/leave).
type MemberInfo struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
}

// Members returns the current roster, in arrival order.
func (r *Room) Members() []MemberInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.membersLocked()
}

func (r *Room) membersLocked() []MemberInfo {
	out := make([]MemberInfo, 0, len(r.members))
	for _, id := range r.order {
		if m, ok := r.members[id]; ok {
			out = append(out, MemberInfo{UserID: m.UserID, Username: m.Username})
		}
	}
	return out
}

// Host returns the current referee's user id.
func (r *Room) Host() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.HostID
}

// Join adds (or REPLACES) a member's connection and announces them. Replacing is
// deliberate: a reconnect from the same account takes over its slot instead of
// creating a ghost, which is what makes a flaky connection survivable.
func (r *Room) Join(userID, username string, conn Sender) (*Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, rejoining := r.members[userID]
	if !rejoining && len(r.members) >= maxMembers {
		return nil, ErrRoomFull
	}

	m := &Member{UserID: userID, Username: username, conn: conn}
	if rejoining {
		// Keep their place in the order; just swap the connection.
		m.tokens = existing.tokens
		m.lastFill = existing.lastFill
	} else {
		r.order = append(r.order, userID)
	}
	r.members[userID] = m
	r.emptySince = time.Time{}

	// An empty room's first arrival takes the host seat (covers the creator
	// joining, and a room whose members all dropped and someone returns).
	if r.HostID == "" || r.members[r.HostID] == nil {
		r.HostID = userID
	}

	if !rejoining {
		r.broadcastLocked(TypeJoin, map[string]any{
			"member": MemberInfo{UserID: userID, Username: username},
			"host":   r.HostID,
		}, userID)
	}
	return m, nil
}

// Leave removes a member, transfers the host seat if they held it, and announces
// the departure. Returns true if the room is now empty.
func (r *Room) Leave(userID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.members[userID]; !ok {
		return len(r.members) == 0
	}
	delete(r.members, userID)
	for i, id := range r.order {
		if id == userID {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}

	// Host transfer: the longest-present remaining member picks up the whistle.
	if r.HostID == userID {
		r.HostID = ""
		for _, id := range r.order {
			if _, ok := r.members[id]; ok {
				r.HostID = id
				break
			}
		}
	}

	r.broadcastLocked(TypeLeave, map[string]any{
		"member": MemberInfo{UserID: userID},
		"host":   r.HostID,
	}, "")

	if len(r.members) == 0 {
		r.emptySince = now
		return true
	}
	return false
}

// Relay forwards one member's opaque message to everyone else. The payload is
// never inspected — `raw` is the frame the sender wrote, re-emitted with a
// `from` stamp by the caller. Returns false when the sender is over its rate
// budget (the caller tells them; the message is dropped, not queued).
func (r *Room) Relay(fromUserID string, frame []byte, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	m, ok := r.members[fromUserID]
	if !ok {
		return false
	}
	if !m.allow(now) {
		return false
	}
	for id, other := range r.members {
		if id == fromUserID {
			continue
		}
		other.conn.Send(frame)
	}
	return true
}

// SendTo delivers a frame to one member (control messages, errors).
func (r *Room) SendTo(userID string, frame []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.members[userID]; ok {
		m.conn.Send(frame)
	}
}

// broadcastLocked emits a server control message to every member except
// `exceptUserID` (pass "" for everyone). Caller holds the lock.
func (r *Room) broadcastLocked(msgType string, data map[string]any, exceptUserID string) {
	frame, err := encodeControl(msgType, data)
	if err != nil {
		return // a control message we can't encode is a bug, never a room failure
	}
	for id, m := range r.members {
		if id == exceptUserID {
			continue
		}
		m.conn.Send(frame)
	}
}

// Hub owns every live room on this server.
type Hub struct {
	mu    sync.Mutex
	rooms map[string]*Room
}

func NewHub() *Hub {
	return &Hub{rooms: map[string]*Room{}}
}

// Create registers a new room hosted by `hostID`. The host doesn't join until
// they open a socket — creating is an HTTP call, joining is the WebSocket.
func (h *Hub) Create(id, activityID, hostID string, now time.Time) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := &Room{
		ID:         id,
		ActivityID: activityID,
		HostID:     hostID,
		CreatedAt:  now,
		members:    map[string]*Member{},
		emptySince: now, // nobody has connected yet; the reaper's grace covers it
	}
	h.rooms[id] = r
	return r
}

// Get returns a live room by id.
func (h *Hub) Get(id string) (*Room, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[id]
	if !ok {
		return nil, ErrRoomNotFound
	}
	return r, nil
}

// Reap drops rooms that have been empty longer than `grace`. The grace period is
// the whole point: a member whose connection blips must be able to come back to
// the same room, and a created-but-not-yet-joined room must survive the moment
// between the HTTP create and the socket opening.
func (h *Hub) Reap(now time.Time, grace time.Duration) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	dropped := 0
	for id, r := range h.rooms {
		r.mu.Lock()
		empty := len(r.members) == 0
		since := r.emptySince
		r.mu.Unlock()
		if empty && !since.IsZero() && now.Sub(since) > grace {
			delete(h.rooms, id)
			dropped++
		}
	}
	return dropped
}

// Count reports how many rooms are live (status/console surface).
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms)
}
