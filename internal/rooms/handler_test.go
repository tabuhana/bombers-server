package rooms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tabuhana/bombers-server/internal/auth"
)

// getRoom runs Get the way the router would: the room id as a URL param, and
// the caller already through RequireAuth.
func getRoom(h *Handler, roomID, userID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/rooms/"+roomID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("roomID", roomID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	req = req.WithContext(auth.WithUserID(ctx, userID))
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	return rec
}

// A host asking after their own room never reaches the friendship query, which
// is what lets these run without a database. The friend path is the same
// allowed() that Join uses.
func TestGetOpenRoomForHost(t *testing.T) {
	h := NewHandler(nil, nil)
	h.hub.Create("room1", "amber-lantern", "host", time.Now())

	rec := getRoom(h, "room1", "host")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got roomResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "room1" || got.Name != "amber-lantern" || got.HostID != "host" {
		t.Fatalf("body = %+v", got)
	}
}

// An unknown id and an ended room answer identically — an invite can't tell
// "never existed" from "over", and neither can anyone probing ids.
func TestGetMissingOrClosedRoomIs404(t *testing.T) {
	h := NewHandler(nil, nil)
	h.hub.Create("room2", "old-lantern", "host", time.Now()).Close()

	for _, id := range []string{"nope", "room2"} {
		rec := getRoom(h, id, "host")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", id, rec.Code)
		}
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error != errRoomNotFound {
			t.Fatalf("%s: error = %q, want %q", id, body.Error, errRoomNotFound)
		}
	}
}
