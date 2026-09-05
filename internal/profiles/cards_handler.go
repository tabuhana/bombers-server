package profiles

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

// Publishing the notes half of your person card, already narrowed to each
// reader.
//
// The wire shape is `{"<viewerId>": <anything>, …}` and the server's entire
// understanding of it is that map. It does not know what a note is, what a
// category is, or that some notes departed from their category's audience —
// the client settled all of that before sending, which is what keeps
// user-invented categories and per-note exceptions from ever becoming a schema.
//
// Facts are not here. They're the same for everyone you're linked to, so they
// live on the profile row and any accepted friend reads them.

const (
	// Viewers × their cards. The per-card cap is enforced separately; this is
	// the outer bound on one request.
	cardsBodyLimit = 8 << 20 // 8 MiB

	errCardNotFound  = "card_not_found"
	errCardTooLarge  = "card_too_large"
	errTooManyCards  = "too_many_cards"
	errInvalidCardID = "invalid_viewer_id"
)

// PutMyCard REPLACES everything the authed user publishes. Anything absent is
// revoked, which is what makes "I unshared that category" take effect with no
// separate revoke call — the row simply stops existing.
//
// It answers with the viewers actually stored, not the ones asked for: a viewer
// who isn't an accepted friend is dropped, and a client that shows "shared with
// 5 people" should be showing the server's number rather than its own hope.
func (h *Handler) PutMyCard(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, cardsBodyLimit)).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, errCardTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "invalid_body")
		return
	}
	if len(req) > MaxViewersPerPublish {
		httpx.WriteError(w, http.StatusBadRequest, errTooManyCards)
		return
	}
	for viewerID, content := range req {
		if viewerID == "" {
			httpx.WriteError(w, http.StatusBadRequest, errInvalidCardID)
			return
		}
		if len(content) > CardLimit {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, errCardTooLarge)
			return
		}
	}

	if _, err := ReplaceCards(r.Context(), h.pool, authedID, req); err != nil {
		logx.Error("profiles: publish cards: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not publish card")
		return
	}

	viewers, err := CardViewers(r.Context(), h.pool, authedID)
	if err != nil {
		logx.Error("profiles: list card viewers: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not publish card")
		return
	}
	// Exactly the people whose copy just changed — a better list than "all my
	// friends", and it's already in hand. Someone who was dropped from the share
	// isn't told, which is correct: they'll find out by their copy 404ing.
	if h.notify != nil {
		h.notify(viewers)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"viewers": viewers})
}

// GetCardFrom returns what one person published FOR ME.
//
// Friend-gated, and every refusal is the same opaque 404: not friends, no such
// user, nothing published, shared with everyone but you. A viewer must not be
// able to tell "they wrote nothing" from "they wrote plenty and none of it is
// for you" — the second is a fact about their opinion of you, and the server has
// no business leaking it.
func (h *Handler) GetCardFrom(w http.ResponseWriter, r *http.Request) {
	viewerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ownerID := chi.URLParam(r, "ownerID")
	if ownerID == "" || ownerID == viewerID {
		// Your own card is the local one; there is nothing here for you.
		httpx.WriteError(w, http.StatusNotFound, errCardNotFound)
		return
	}

	friends, err := areFriends(r.Context(), h.pool, ownerID, viewerID)
	if err != nil {
		logx.Error("profiles: card friendship: %v", err)
		httpx.WriteError(w, http.StatusNotFound, errCardNotFound) // fail CLOSED
		return
	}
	if !friends {
		httpx.WriteError(w, http.StatusNotFound, errCardNotFound)
		return
	}

	content, err := GetCard(r.Context(), h.pool, ownerID, viewerID)
	if errors.Is(err, ErrNoCard) {
		httpx.WriteError(w, http.StatusNotFound, errCardNotFound)
		return
	}
	if err != nil {
		logx.Error("profiles: get card: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch card")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(cardResponse(ownerID, content))
}

// cardResponse wraps a stored card without re-encoding it.
//
// Splicing rather than unmarshal-and-marshal is the whole of the opaque
// promise: a round trip through Go would reorder object keys, reformat numbers
// and quietly drop anything the server's types don't model. What the owner
// published has to be byte-for-byte what their friend's client reads, because
// the client is the only half that knows what a card means.
func cardResponse(ownerID string, content json.RawMessage) []byte {
	owner, _ := json.Marshal(ownerID)
	if len(content) == 0 {
		content = json.RawMessage("null")
	}
	out := make([]byte, 0, len(content)+len(owner)+24)
	out = append(out, `{"owner_id":`...)
	out = append(out, owner...)
	out = append(out, `,"content":`...)
	out = append(out, content...)
	return append(out, '}')
}
