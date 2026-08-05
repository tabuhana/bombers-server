package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

const (
	createBodyLimit = 1 << 12 // 4 KiB - a create is two short strings.

	// How long an empty room survives. Long enough to ride out a reconnect or
	// the gap between creating a room and opening the socket; short enough that
	// abandoned lobbies don't accumulate.
	emptyGrace = 2 * time.Minute
	reapEvery  = 30 * time.Second

	// A member that hasn't been heard from within this window is dropped. The
	// client is expected to ping; coder/websocket's Ping does the round trip.
	pongWait   = 60 * time.Second
	pingPeriod = 25 * time.Second

	// Per-connection outbound queue. A member too slow to drain this is dropped
	// rather than allowed to stall the room - in a realtime game, stale frames
	// are worthless anyway.
	sendQueue = 64

	errRoomNotFound = "room_not_found"
	errNotAllowed   = "room_not_found" // deliberately identical: never leak existence
	errInvalidBody  = "invalid_body"
)

type Handler struct {
	pool   *pgxpool.Pool
	issuer *auth.Issuer
	hub    *Hub
}

func NewHandler(pool *pgxpool.Pool, issuer *auth.Issuer) *Handler {
	return &Handler{pool: pool, issuer: issuer, hub: NewHub()}
}

// memberIdentity is who the socket belongs to, resolved once at join.
type memberIdentity struct {
	UserID   string
	Username string
}

// authFromToken verifies the access token that arrived on the WebSocket
// subprotocol. Same issuer, same rules as the HTTP middleware - the transport
// differs, the gate does not.
func (h *Handler) authFromToken(ctx context.Context, token string) (memberIdentity, error) {
	if token == "" {
		return memberIdentity{}, errors.New("missing token")
	}
	claims, err := h.issuer.ParseAccessToken(token)
	if err != nil {
		return memberIdentity{}, err
	}
	userID := claims.UserID
	if userID == "" {
		return memberIdentity{}, errors.New("token has no user")
	}
	return memberIdentity{UserID: userID, Username: usernameFor(ctx, h.pool, userID)}, nil
}

// StartReaper runs the empty-room sweep until ctx is cancelled. main owns the
// lifetime; the hub itself stays passive.
func (h *Handler) StartReaper(ctx context.Context) {
	go func() {
		t := time.NewTicker(reapEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if n := h.hub.Reap(now, emptyGrace); n > 0 {
					logx.Info("rooms: reaped %d empty room(s)", n)
				}
			}
		}
	}()
}

// LiveRooms reports how many rooms exist (for /health and the console).
func (h *Handler) LiveRooms() int { return h.hub.Count() }

type createRequest struct {
	// Optional. A room names itself if you don't, and the host can rename it
	// afterwards, so asking for one up front would be a prompt in the way of
	// the thing you actually wanted.
	Name string `json:"name"`
}

type createResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	HostID string `json:"host_id"`
}

// Create opens a room. The creator is its host; nobody is in it until a socket
// connects. Rooms are in-memory and ephemeral - restarting the server ends every
// one of them, which is the intended contract.
//
// No activity is named here. A room is a space first; the host points it at a
// game from inside it, over the socket.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// An empty body is the ordinary case ("just make me a room"), so it must not
	// read as a malformed one.
	var req createRequest
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, createBodyLimit)).Decode(&req); err != nil && err != io.EOF {
			httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
			return
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = RandomName()
	}
	if len(name) > MaxRoomNameLen {
		name = name[:MaxRoomNameLen]
	}

	id := ulid.Make().String()
	room := h.hub.Create(id, name, authedID, time.Now())
	httpx.WriteJSON(w, http.StatusCreated, createResponse{
		ID:     room.ID,
		Name:   room.Name(),
		HostID: room.HostID,
	})
}

// Join upgrades to a WebSocket and runs the member's session.
//
// AUTH over WebSocket: a browser/webview cannot set an Authorization header on
// a socket, and putting a token in the query string writes it into every access
// log. So the access token rides the subprotocol - `["bearer", "<token>"]` -
// which the WebSocket API does let a client set. The server picks "bearer" as
// the negotiated protocol and reads the token from the same header.
func (h *Handler) Join(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "roomID")
	if roomID == "" {
		httpx.WriteError(w, http.StatusNotFound, errRoomNotFound)
		return
	}

	token := bearerFromProtocols(r.Header.Get("Sec-WebSocket-Protocol"))
	claims, err := h.authFromToken(r.Context(), token)
	if err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	room, err := h.hub.Get(roomID)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, errRoomNotFound)
		return
	}

	// Friend-gating, same rule as every other cross-user surface: you may join a
	// room whose host is you or an accepted friend. Anything else collapses to
	// the same opaque 404 so a room id can't be probed.
	if room.HostID != claims.UserID {
		friends, err := areFriends(r.Context(), h.pool, room.HostID, claims.UserID)
		if err != nil {
			logx.Error("rooms: friendship check: %v", err)
			httpx.WriteError(w, http.StatusNotFound, errNotAllowed)
			return
		}
		if !friends {
			httpx.WriteError(w, http.StatusNotFound, errNotAllowed)
			return
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{"bearer"},
		// Same-origin checking is off because the desktop client's origin is a
		// Tauri/webview scheme, not this host. The bearer token above is the
		// actual gate, exactly as it is for every HTTP route.
		InsecureSkipVerify: true,
	})
	if err != nil {
		logx.Error("rooms: accept: %v", err)
		return
	}

	h.runMember(r.Context(), conn, room, claims)
}

// runMember owns one connection's lifetime: join, pump frames until it ends,
// then leave. Everything it does to the room goes through the hub, so the rules
// (rate limit, host transfer, presence) live in one testable place.
func (h *Handler) runMember(ctx context.Context, conn *websocket.Conn, room *Room, claims memberIdentity) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The socket must die whatever ends this function, and one of those endings
	// is now the SERVER's decision rather than the peer's: a kick, or the room
	// closing, unwinds from the inside while the client is still perfectly
	// happy. Without this it would sit there holding a connection to a room that
	// no longer exists.
	defer conn.CloseNow()

	out := make(chan []byte, sendQueue)
	sender := &wsSender{out: out, cancel: cancel}

	if _, err := room.Join(claims.UserID, claims.Username, sender); err != nil {
		code := "room_full"
		if errors.Is(err, ErrRoomClosed) {
			code = "room_closed"
		}
		frame, _ := encodeControl(TypeError, map[string]any{"code": code})
		_ = conn.Write(ctx, websocket.MessageText, frame)
		_ = conn.Close(websocket.StatusPolicyViolation, code)
		return
	}
	defer func() {
		outcome := room.Leave(claims.UserID, time.Now())
		if outcome.Empty {
			logx.Info("rooms: %s is now empty", room.ID)
		}
		if outcome.HostGone {
			// Not closed here on purpose: this fires for a dropped socket as
			// well as a deliberate exit, and a blip must not end everyone's
			// session. The reaper closes the room if they don't come back.
			logx.Info("rooms: %s lost its host", room.ID)
		}
	}()

	// Welcome: everything a joiner needs to render the room immediately - what
	// it's called, who's here, who hosts, and which game it's pointed at. A late
	// joiner asks the host for game STATE itself; the server has none to give.
	welcome, err := encodeControl(TypeWelcome, map[string]any{
		"room":        room.ID,
		"name":        room.Name(),
		"activity_id": room.Activity(),
		"you":         claims.UserID,
		"host":        room.Host(),
		"members":     room.Members(),
	})
	if err == nil {
		sender.Send(welcome)
	}

	// Writer: drains the member's queue and keeps the connection warm.
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case frame, ok := <-out:
				if !ok {
					return
				}
				writeCtx, cancelWrite := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(writeCtx, websocket.MessageText, frame)
				cancelWrite()
				if err != nil {
					cancel()
					return
				}
			case <-ticker.C:
				pingCtx, cancelPing := context.WithTimeout(ctx, pongWait)
				err := conn.Ping(pingCtx)
				cancelPing()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Reader: every inbound frame is relayed verbatim to the rest of the room.
	conn.SetReadLimit(maxFrameBytes)
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			var closeErr websocket.CloseError
			if !errors.As(err, &closeErr) && ctx.Err() == nil {
				logx.Info("rooms: read ended for %s: %v", claims.UserID, err)
			}
			cancel()
			return
		}

		e, ok := decodeIncoming(raw)
		if !ok {
			sender.Send(mustControl(TypeError, map[string]any{"code": "invalid_frame"}))
			continue
		}
		// `room:` is the server's namespace - a client claiming to be one would
		// let it forge presence.
		if strings.HasPrefix(e.Type, "room:") {
			sender.Send(mustControl(TypeError, map[string]any{"code": "reserved_type"}))
			continue
		}
		// `host:` is a request OF the room rather than a message THROUGH it.
		// Answered here and never relayed.
		if strings.HasPrefix(e.Type, CtlPrefix) {
			if h.control(room, claims.UserID, e, sender) {
				return // the room ended under us
			}
			continue
		}
		frame, err := stampFrom(e, claims.UserID)
		if err != nil {
			continue
		}
		if !room.Relay(claims.UserID, frame, time.Now()) {
			sender.Send(mustControl(TypeError, map[string]any{"code": "rate_limited"}))
		}
	}
}

// controlRequest is the payload of every `host:` frame - a small union, because
// four one-field requests do not deserve four types.
type controlRequest struct {
	Name       string `json:"name"`
	UserID     string `json:"user_id"`
	ActivityID string `json:"activity_id"`
}

// control answers one host request. Returns true when the room ended, so the
// caller stops reading from a socket that no longer belongs to anything.
//
// Every refusal names its reason: unlike joining - where an opaque 404 stops a
// room id being probed - the sender is already inside the room, so there is
// nothing left to hide and a silent no-op would just look broken.
func (h *Handler) control(room *Room, userID string, e *envelope, sender Sender) bool {
	var req controlRequest
	if len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, &req); err != nil {
			sender.Send(mustControl(TypeError, map[string]any{"code": errInvalidBody}))
			return false
		}
	}

	var err error
	switch e.Type {
	case CtlRename:
		err = room.SetName(userID, req.Name)
	case CtlGame:
		err = room.SetActivity(userID, req.ActivityID)
	case CtlKick:
		err = room.Kick(userID, req.UserID)
	case CtlEnd:
		// The one control that answers by ending the conversation. Host only,
		// same as the rest.
		if room.Host() != userID {
			sender.Send(mustControl(TypeError, map[string]any{"code": "not_host"}))
			return false
		}
		room.Close()
		h.hub.Remove(room.ID)
		logx.Info("rooms: %s ended by its host", room.ID)
		return true
	default:
		err = errors.New("unknown control")
	}

	if err != nil {
		sender.Send(mustControl(TypeError, map[string]any{"code": controlErrorCode(err)}))
	}
	return false
}

func controlErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrNotHost):
		return "not_host"
	case errors.Is(err, ErrRoomClosed):
		return "room_closed"
	default:
		return "refused"
	}
}

// wsSender adapts the outbound channel to the hub's Sender. A full queue means
// the peer isn't keeping up: the frame is DROPPED rather than blocking the
// broadcast - one slow member must never freeze the room.
type wsSender struct {
	out    chan []byte
	cancel context.CancelFunc
	once   sync.Once
}

func (s *wsSender) Send(msg []byte) {
	select {
	case s.out <- msg:
	default:
	}
}

// Close ends this member's connection - what the room calls when they've been
// kicked or it has itself ended. Cancelling the member's context unwinds both
// pumps and runs the deferred Leave, so there is one teardown path whether the
// socket died on its own or was ended for it.
func (s *wsSender) Close() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}

func mustControl(msgType string, data map[string]any) []byte {
	frame, err := encodeControl(msgType, data)
	if err != nil {
		return []byte(`{"t":"room:error","d":{"code":"internal"}}`)
	}
	return frame
}

// bearerFromProtocols pulls the token out of a `bearer, <token>` subprotocol
// header. Returns "" when the header isn't in that shape.
func bearerFromProtocols(header string) string {
	parts := strings.Split(header, ",")
	if len(parts) < 2 {
		return ""
	}
	if strings.TrimSpace(parts[0]) != "bearer" {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
