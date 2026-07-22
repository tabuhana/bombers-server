package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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
	ActivityID string `json:"activity_id"`
}

type createResponse struct {
	ID         string `json:"id"`
	ActivityID string `json:"activity_id"`
	HostID     string `json:"host_id"`
}

// Create opens a room. The creator is its host (the referee); nobody is in it
// until a socket connects. Rooms are in-memory and ephemeral - restarting the
// server ends every session, which is the intended contract.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req createRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, createBodyLimit)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}
	activityID := strings.TrimSpace(req.ActivityID)
	if activityID == "" || len(activityID) > 64 {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}

	id := ulid.Make().String()
	room := h.hub.Create(id, activityID, authedID, time.Now())
	httpx.WriteJSON(w, http.StatusCreated, createResponse{
		ID:         room.ID,
		ActivityID: room.ActivityID,
		HostID:     room.HostID,
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

	out := make(chan []byte, sendQueue)
	sender := &wsSender{out: out}

	if _, err := room.Join(claims.UserID, claims.Username, sender); err != nil {
		frame, _ := encodeControl(TypeError, map[string]any{"code": "room_full"})
		_ = conn.Write(ctx, websocket.MessageText, frame)
		_ = conn.Close(websocket.StatusPolicyViolation, "room full")
		return
	}
	defer func() {
		if room.Leave(claims.UserID, time.Now()) {
			logx.Info("rooms: %s is now empty", room.ID)
		}
	}()

	// Welcome: everything a joiner needs to render the session immediately -
	// who's here and who referees. A late joiner asks the host for game state
	// itself; the server has none to give.
	welcome, err := encodeControl(TypeWelcome, map[string]any{
		"room":        room.ID,
		"activity_id": room.ActivityID,
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
		frame, err := stampFrom(e, claims.UserID)
		if err != nil {
			continue
		}
		if !room.Relay(claims.UserID, frame, time.Now()) {
			sender.Send(mustControl(TypeError, map[string]any{"code": "rate_limited"}))
		}
	}
}

// wsSender adapts the outbound channel to the hub's Sender. A full queue means
// the peer isn't keeping up: the frame is DROPPED rather than blocking the
// broadcast - one slow member must never freeze the room.
type wsSender struct {
	out chan []byte
}

func (s *wsSender) Send(msg []byte) {
	select {
	case s.out <- msg:
	default:
	}
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
