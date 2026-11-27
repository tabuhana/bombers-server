package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tabuhana/bombers-server/internal/deadline"
)

// registerSockets mounts the two WebSocket routes in the Live class: no time
// limit. Everything else defaults to Quick, so a socket route that was not
// registered here would be cut off after 30 seconds.
func registerSockets(r chi.Router, dl *deadline.Deadlines, roomJoin, notifyListen http.HandlerFunc) {
	// Activity rooms: the realtime relay. The JOIN is a WebSocket upgrade and so
	// sits OUTSIDE the RequireAuth group - a webview cannot put an Authorization
	// header on a socket, so the access token rides the `bearer` subprotocol and
	// the handler verifies it with the same issuer.
	r.With(dl.Live).Get("/rooms/{roomID}/ws", roomJoin)

	// The per-user nudge socket, same auth trick and outside RequireAuth for the
	// same reason. It carries no content — only "go and re-read something" — so a
	// client that never connects is stale, never wrong.
	r.With(dl.Live).Get("/notify/ws", notifyListen)
}
