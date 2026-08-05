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
	"strings"
	"sync"
	"time"
)

// Room control messages the SERVER generates. Namespaced so an activity's own
// message types can never collide with them.
const (
	TypeWelcome = "room:welcome" // sent to the joiner: who's here, who hosts, what this room is
	TypeJoin    = "room:join"    // broadcast: someone arrived
	TypeLeave   = "room:leave"   // broadcast: someone left
	TypeUpdate  = "room:update"  // broadcast: the name or the chosen game changed
	TypeKicked  = "room:kicked"  // sent to one member: the host removed you
	TypeClosed  = "room:closed"  // broadcast: the room has ended, for everyone
	TypeError   = "room:error"   // sent to one member: something was refused
)

// Requests a CLIENT may make of the room itself, as opposed to messages it wants
// relayed. A second reserved namespace, and the mirror of `room:`: the server
// answers these and never forwards them.
//
// They ride the socket rather than sitting on HTTP endpoints because they are
// ordered against the traffic around them — a rename and the chat line
// mentioning it must not overtake each other — and because the socket is already
// the room's identity. Every one of them is HOST-ONLY; anyone else gets
// `not_host` back.
const (
	CtlRename = "host:rename" // {name}        — retitle the room
	CtlKick   = "host:kick"   // {user_id}     — remove a member
	CtlGame   = "host:game"   // {activity_id} — point the room at a game
	CtlEnd    = "host:end"    // {}            — close the room for everyone
)

// CtlPrefix marks a client→server control request. Frames carrying it are
// handled, never relayed.
const CtlPrefix = "host:"

var (
	ErrRoomNotFound = errors.New("room not found")
	ErrRoomFull     = errors.New("room full")
	ErrRoomClosed   = errors.New("room closed")
	ErrNotHost      = errors.New("not the host")
)

// Bounds on what a host may set. A room name is a label in a sidebar, and an
// activity id is a slug — both are short by nature, and neither is worth a
// validation framework.
const (
	MaxRoomNameLen   = 48
	MaxActivityIDLen = 64
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
	// Close ends this member's connection. The room calls it when the member no
	// longer belongs — kicked, or the room itself ended — so a dead room doesn't
	// leave sockets open waiting for clients to take the hint. Must be safe to
	// call more than once.
	Close()
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

// Room is one live space: some people, a name, a chat, and the game they've
// pointed themselves at.
//
// A room is NOT a game. It is created empty and the host chooses a game inside
// it — which is why ActivityID is mutable and may be blank. That separation is
// the whole point: you sit in a room with friends, and a game is something you
// then start together.
type Room struct {
	ID string
	// HostID is both the room's owner and the game's referee. It never
	// transfers: the host leaving ENDS the room (see Leave). A room is the
	// host's space, and a temporary space outliving the person who made it was
	// a worse answer than telling everyone it's over.
	HostID    string
	CreatedAt time.Time

	mu sync.Mutex
	// name is a throwaway label, generated at creation and changeable by the
	// host at any time. Held here rather than by the clients because a joiner
	// has to learn it from somewhere.
	name string
	// activityID is the game this room will start. Empty until the host picks
	// one. The server holds the ID and nothing else — it still has no idea what
	// any game DOES.
	activityID string
	closed     bool
	members    map[string]*Member
	// order preserves arrival order, so the roster reads the way the room
	// filled rather than however the map iterates.
	order []string
	// emptySince marks when the last member left; the reaper uses it. Zero while
	// anyone is present.
	emptySince time.Time
	// hostAway marks when the host's connection went away. Zero while they're
	// here. A blip must not end the room, so this starts a countdown the reaper
	// finishes rather than closing on the spot.
	hostAway time.Time
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

// Host returns the referee's user id.
func (r *Room) Host() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.HostID
}

// Name is the room's current label.
func (r *Room) Name() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.name
}

// Activity is the game this room is pointed at, or "" if the host hasn't
// chosen one.
func (r *Room) Activity() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activityID
}

// Closed reports whether the room has ended.
func (r *Room) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// SetName retitles the room and tells everyone. Host only.
//
// An empty or over-long name is silently trimmed to something sane rather than
// refused: a rename is a label, and failing one is more disruptive than
// clamping it.
func (r *Room) SetName(byUserID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("empty name")
	}
	if len(name) > MaxRoomNameLen {
		name = name[:MaxRoomNameLen]
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRoomClosed
	}
	if byUserID != r.HostID {
		return ErrNotHost
	}
	r.name = name
	r.broadcastLocked(TypeUpdate, map[string]any{"name": r.name, "activity_id": r.activityID}, "")
	return nil
}

// SetActivity points the room at a game (or clears it with ""). Host only.
//
// The server records the id and learns nothing else. Whether anyone actually
// HAS that game is a question the clients answer among themselves — this is
// what they answer it about.
func (r *Room) SetActivity(byUserID, activityID string) error {
	activityID = strings.TrimSpace(activityID)
	if len(activityID) > MaxActivityIDLen {
		return errors.New("activity id too long")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRoomClosed
	}
	if byUserID != r.HostID {
		return ErrNotHost
	}
	r.activityID = activityID
	r.broadcastLocked(TypeUpdate, map[string]any{"name": r.name, "activity_id": r.activityID}, "")
	return nil
}

// Kick removes a member at the host's request: the target is told why and
// disconnected, everyone else sees an ordinary departure.
//
// The host cannot kick themselves — that's `end`, and quietly treating one as
// the other would close a room somebody meant to stay in.
func (r *Room) Kick(byUserID, targetID string) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrRoomClosed
	}
	if byUserID != r.HostID {
		r.mu.Unlock()
		return ErrNotHost
	}
	if targetID == r.HostID {
		r.mu.Unlock()
		return errors.New("the host cannot be kicked")
	}
	target, ok := r.members[targetID]
	if !ok {
		r.mu.Unlock()
		return errors.New("not in this room")
	}

	// Tell them first — after removal they're no longer a broadcast target.
	if frame, err := encodeControl(TypeKicked, map[string]any{"room": r.ID}); err == nil {
		target.conn.Send(frame)
	}
	r.removeLocked(targetID)
	r.broadcastLocked(TypeLeave, map[string]any{"member": MemberInfo{UserID: targetID}}, "")
	r.mu.Unlock()

	target.conn.Close()
	return nil
}

// Close ends the room for everyone: each member is told and disconnected. Safe
// to call twice — the second time does nothing.
func (r *Room) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.broadcastLocked(TypeClosed, map[string]any{"room": r.ID}, "")
	conns := make([]Sender, 0, len(r.members))
	for _, m := range r.members {
		conns = append(conns, m.conn)
	}
	r.members = map[string]*Member{}
	r.order = nil
	r.mu.Unlock()

	// Outside the lock: closing a connection can run arbitrary teardown, and
	// holding the room's lock through it would let one slow socket freeze
	// everything else.
	for _, c := range conns {
		c.Close()
	}
}

// Join adds (or REPLACES) a member's connection and announces them. Replacing is
// deliberate: a reconnect from the same account takes over its slot instead of
// creating a ghost, which is what makes a flaky connection survivable.
func (r *Room) Join(userID, username string, conn Sender) (*Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, ErrRoomClosed
	}

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

	// The host coming back cancels the countdown their disconnect started.
	if userID == r.HostID {
		r.hostAway = time.Time{}
	}

	if !rejoining {
		r.broadcastLocked(TypeJoin, map[string]any{
			"member": MemberInfo{UserID: userID, Username: username},
			"host":   r.HostID,
		}, userID)
	}
	return m, nil
}

// LeaveOutcome is what a departure did to the room.
type LeaveOutcome struct {
	// Empty is true when nobody is left in the room.
	Empty bool
	// HostGone is true when the person who left was the host. The room is NOT
	// closed here: this may be a dropped connection rather than a decision, and
	// a laptop lid must not end everyone's evening. The reaper closes the room
	// if they don't come back — an explicit End() is what closes it at once.
	HostGone bool
}

// Leave removes a member and announces the departure.
//
// The host seat never moves. A room belongs to whoever opened it, and a
// temporary space that outlives its owner — quietly re-homed to whoever
// happened to be standing there longest — was a worse answer than ending it.
func (r *Room) Leave(userID string, now time.Time) LeaveOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.members[userID]; !ok {
		return LeaveOutcome{Empty: len(r.members) == 0}
	}
	r.removeLocked(userID)

	if userID == r.HostID {
		r.hostAway = now
	}

	r.broadcastLocked(TypeLeave, map[string]any{
		"member": MemberInfo{UserID: userID},
		"host":   r.HostID,
	}, "")

	if len(r.members) == 0 {
		r.emptySince = now
	}
	return LeaveOutcome{Empty: len(r.members) == 0, HostGone: userID == r.HostID}
}

// removeLocked drops a member from the roster and the arrival order. Caller
// holds the lock.
func (r *Room) removeLocked(userID string) {
	delete(r.members, userID)
	for i, id := range r.order {
		if id == userID {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
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
//
// No activity: a room starts as a space, and the host points it at a game later
// (or never — a room with nothing but a conversation in it is a legitimate room).
func (h *Hub) Create(id, name, hostID string, now time.Time) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := &Room{
		ID:         id,
		HostID:     hostID,
		CreatedAt:  now,
		name:       name,
		members:    map[string]*Member{},
		emptySince: now, // nobody has connected yet; the reaper's grace covers it
	}
	h.rooms[id] = r
	return r
}

// Remove drops a room from the hub. Used when a room ends on purpose, so a
// closed room stops being reachable immediately instead of lingering until the
// next sweep.
func (h *Hub) Remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.rooms, id)
}

// Get returns a live room by id. A closed room is gone as far as anyone asking
// is concerned — rejoining one would be joining a room that ended.
func (h *Hub) Get(id string) (*Room, error) {
	h.mu.Lock()
	r, ok := h.rooms[id]
	h.mu.Unlock()
	if !ok {
		return nil, ErrRoomNotFound
	}
	if r.Closed() {
		return nil, ErrRoomNotFound
	}
	return r, nil
}

// Reap ends rooms that have run out of reasons to exist, and returns how many.
//
// Two reasons, both on the same grace period, and the grace is the whole point —
// a connection that blips must be able to come back to the same room:
//
//   - EMPTY past the grace. Also covers the gap between the HTTP create and the
//     host's socket opening.
//   - HOST GONE past the grace. The host leaving ends the room, but a dropped
//     socket isn't a decision — this is where a host who never comes back
//     finally becomes one. Everyone still sitting there is told.
func (h *Hub) Reap(now time.Time, grace time.Duration) int {
	h.mu.Lock()
	var ending []*Room
	for id, r := range h.rooms {
		r.mu.Lock()
		empty := len(r.members) == 0 && !r.emptySince.IsZero() && now.Sub(r.emptySince) > grace
		hostGone := !r.hostAway.IsZero() && now.Sub(r.hostAway) > grace
		r.mu.Unlock()
		if empty || hostGone {
			delete(h.rooms, id)
			ending = append(ending, r)
		}
	}
	h.mu.Unlock()

	// Closing broadcasts and shuts sockets, so it happens outside the hub lock —
	// otherwise one unresponsive member would block every other room's sweep.
	for _, r := range ending {
		r.Close()
	}
	return len(ending)
}

// Count reports how many rooms are live (status/console surface).
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms)
}
