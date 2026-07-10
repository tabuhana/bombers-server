// Package httpx holds tiny, framework-agnostic helpers for writing HTTP
// responses. The single error envelope defined here is the project-wide
// JSON error contract — every endpoint that returns an error must use it.
package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/tabuhana/bombers-server/internal/logx"
)

// ErrorBody is the JSON shape returned for any non-2xx response: {"error": "..."}.
type ErrorBody struct {
	Error string `json:"error"`
}

// WriteJSON writes body as JSON with the given status. A nil body sends only
// the status line. Encoding failures are logged but cannot be reported to the
// client (headers have already been flushed).
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logx.Error("httpx: encode response: %v", err)
	}
}

// WriteError writes the standard error envelope.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, ErrorBody{Error: message})
}
