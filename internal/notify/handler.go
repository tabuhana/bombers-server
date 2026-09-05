package notify

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

const (
	// A connection we haven't heard from in this long is gone, whatever the OS
	// thinks. Laptops sleep and wifi drops without ever closing a socket, and a
	// dead entry in the hub means nudges written into a void.
	pongWait   = 60 * time.Second
	pingPeriod = 25 * time.Second
)

type Handler struct {
	hub    *Hub
	issuer *auth.Issuer
}

func NewHandler(issuer *auth.Issuer) *Handler {
	return &Handler{hub: NewHub(), issuer: issuer}
}

// Hub is what the rest of the server sends through.
func (h *Handler) Hub() *Hub { return h.hub }

// Listen upgrades to a WebSocket and holds it open for as long as the client
// wants nudges.
//
// AUTH is the same trick the room socket uses, for the same reason: a webview
// cannot set an Authorization header on a WebSocket, and a token in the query
// string ends up in every access log. So it rides the subprotocol as
// `["bearer", "<token>"]`, which the browser API does allow.
func (h *Handler) Listen(w http.ResponseWriter, r *http.Request) {
	token := bearerFromProtocols(r.Header.Get("Sec-WebSocket-Protocol"))
	userID, err := h.userFromToken(token)
	if err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	sock, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{"bearer"},
		// Off for the same reason as rooms: the desktop client's origin is a
		// Tauri scheme, not this host. The bearer token above is the real gate.
		InsecureSkipVerify: true,
	})
	if err != nil {
		logx.Error("notify: accept: %v", err)
		return
	}
	defer sock.CloseNow()

	h.run(r.Context(), sock, userID)
}

// run owns one connection: register, pump until something ends it, deregister.
func (h *Handler) run(ctx context.Context, sock *websocket.Conn, userID string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c := &conn{out: make(chan []byte, sendQueue)}
	h.hub.add(userID, c)
	defer h.hub.remove(userID, c)

	// The client sends nothing but its own keepalives. Reading is still
	// necessary — it's how a closed connection is noticed — so a reader runs
	// purely to cancel the context when the socket ends.
	go func() {
		defer cancel()
		for {
			if _, _, err := sock.Read(ctx); err != nil {
				return
			}
			// Anything a client sends is ignored rather than refused. This
			// channel is one-directional by design, and a future client that
			// says hello should not be disconnected for it.
		}
	}()

	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-c.out:
			writeCtx, done := context.WithTimeout(ctx, 10*time.Second)
			err := sock.Write(writeCtx, websocket.MessageText, frame)
			done()
			if err != nil {
				return
			}
		case <-ping.C:
			pingCtx, done := context.WithTimeout(ctx, pongWait)
			err := sock.Ping(pingCtx)
			done()
			if err != nil {
				return
			}
		}
	}
}

func (h *Handler) userFromToken(token string) (string, error) {
	if token == "" {
		return "", errors.New("missing token")
	}
	claims, err := h.issuer.ParseAccessToken(token)
	if err != nil {
		return "", err
	}
	if claims.UserID == "" {
		return "", errors.New("token has no user")
	}
	return claims.UserID, nil
}

// bearerFromProtocols pulls the token out of `Sec-WebSocket-Protocol:
// bearer, <token>`.
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
