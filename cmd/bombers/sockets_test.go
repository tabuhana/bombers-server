package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tabuhana/bombers-server/internal/deadline"
)

// A socket route must outlive the Quick limit; this fails if one is ever
// registered outside registerSockets and falls back to it.
func TestSocketRoutesAreLive(t *testing.T) {
	l := deadline.Limits{Quick: 150 * time.Millisecond, TransferTotal: 2 * time.Second, TransferStall: 200 * time.Millisecond}
	dl := deadline.New(l)
	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		if r.Context().Err() != nil {
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}
	r := chi.NewRouter()
	r.Use(dl.Middleware)
	registerSockets(r, dl, slow, slow)

	srv := httptest.NewUnstartedServer(r)
	srv.Config.ReadTimeout = l.Quick
	srv.Config.WriteTimeout = l.Quick
	srv.Start()
	defer srv.Close()

	for _, path := range []string{"/rooms/x/ws", "/notify/ws"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != "ok" {
			t.Fatalf("%s: body %q err %v, want ok", path, body, err)
		}
	}
}
