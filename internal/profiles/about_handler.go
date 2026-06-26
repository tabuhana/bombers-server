package profiles

// About-card HTTP handlers (the private notes a user keeps about a friend). These
// hang off the same Handler as the self-card. Routes:
//
//	GET    /me/about              list all my about-cards (cross-device restore)
//	GET    /me/about/{subjectID}  my about-card for one friend (empty default)
//	PUT    /me/about/{subjectID}  upsert my about-card for one friend
//	DELETE /me/about/{subjectID}  delete my about-card for one friend
//	GET    /about/{authorID}      what a friend wrote about ME, if they shared it
//
// Content is an opaque client-owned JSON object the server stores verbatim; the
// server only enforces a size cap and that it's a JSON object. Visibility is
// 'private' (default) or 'subject'. Reads of someone else's card about you are
// gated by visibility='subject' + accepted friendship, collapsing every
// not-allowed case to an opaque 404.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
)

const (
	aboutBodyLimit  = 1 << 16  // 64 KiB — notes are text, not blobs.
	maxAboutContent = 32 << 10 // 32 KiB of JSON content per card

	errInvalidContent   = "invalid_content"
	errContentTooLarge  = "content_too_large"
	errAboutNotFound    = "about_not_found"
	errSubjectNotFriend = "subject_not_friend"
)

// aboutResponse is the JSON-safe view of an about-card. Content is re-emitted as
// raw JSON (never re-encoded into a Go shape — the server stays content-agnostic).
// updated_at is nil for a card that has never been saved.
type aboutResponse struct {
	AuthorID   string          `json:"author_id"`
	SubjectID  string          `json:"subject_id"`
	Content    json.RawMessage `json:"content"`
	Visibility string          `json:"visibility"`
	UpdatedAt  *time.Time      `json:"updated_at"`
}

func toAboutResponse(a *aboutRecord) aboutResponse {
	resp := aboutResponse{
		AuthorID:   a.AuthorID,
		SubjectID:  a.SubjectID,
		Content:    a.Content,
		Visibility: a.Visibility,
	}
	if !a.UpdatedAt.IsZero() {
		resp.UpdatedAt = &a.UpdatedAt
	}
	return resp
}

// emptyAbout is what GetMyAbout returns before anything is saved, so the client
// always receives an editable shape.
func emptyAbout(authorID, subjectID string) aboutResponse {
	return aboutResponse{
		AuthorID:   authorID,
		SubjectID:  subjectID,
		Content:    json.RawMessage(`{}`),
		Visibility: AboutVisibilityPrivate,
	}
}

// ListMyAbout returns every about-card the authed user has written, for restore
// onto a new device.
func (h *Handler) ListMyAbout(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	rows, err := listAboutByAuthor(r.Context(), h.pool, authedID)
	if err != nil {
		log.Printf("profiles: list about: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not list about-cards")
		return
	}
	out := make([]aboutResponse, len(rows))
	for i := range rows {
		out[i] = toAboutResponse(&rows[i])
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"about_cards": out})
}

// GetMyAbout returns the authed user's about-card for one subject (or an empty
// default). No friendship check — it's the caller's own data.
func (h *Handler) GetMyAbout(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	subjectID := chi.URLParam(r, "subjectID")
	if subjectID == "" {
		httpx.WriteError(w, http.StatusNotFound, errAboutNotFound)
		return
	}

	a, err := getAbout(r.Context(), h.pool, authedID, subjectID)
	if err != nil {
		if errors.Is(err, ErrAboutNotFound) {
			httpx.WriteJSON(w, http.StatusOK, emptyAbout(authedID, subjectID))
			return
		}
		log.Printf("profiles: get my about: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch about-card")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAboutResponse(a))
}

type upsertAboutRequest struct {
	Content    json.RawMessage `json:"content"`
	Visibility string          `json:"visibility"`
}

// UpsertMyAbout saves the authed user's about-card for one subject. The subject
// must be an accepted friend (you keep notes about friends); a non-friend or
// nonexistent subject collapses to an opaque subject_not_friend.
func (h *Handler) UpsertMyAbout(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, aboutBodyLimit)

	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	subjectID := chi.URLParam(r, "subjectID")
	if subjectID == "" || subjectID == authedID {
		httpx.WriteError(w, http.StatusNotFound, errSubjectNotFriend)
		return
	}

	var req upsertAboutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	content, code := normalizeContent(req.Content)
	if code != "" {
		httpx.WriteError(w, http.StatusBadRequest, code)
		return
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = AboutVisibilityPrivate
	}
	if visibility != AboutVisibilityPrivate && visibility != AboutVisibilitySubject {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidVisibility)
		return
	}

	// Gate on friendship so about-cards only exist for actual friends.
	if code := h.guardSubject(r.Context(), authedID, subjectID); code != "" {
		httpx.WriteError(w, http.StatusNotFound, code)
		return
	}

	saved, err := upsertAbout(r.Context(), h.pool, &aboutRecord{
		AuthorID:   authedID,
		SubjectID:  subjectID,
		Content:    content,
		Visibility: visibility,
	})
	if err != nil {
		log.Printf("profiles: upsert about: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not save about-card")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAboutResponse(saved))
}

// DeleteMyAbout removes the authed user's about-card for one subject. Idempotent
// (deleting a nonexistent card is a 204), and never friendship-gated — cleanup
// after an unfriend must always succeed.
func (h *Handler) DeleteMyAbout(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	subjectID := chi.URLParam(r, "subjectID")
	if subjectID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := deleteAbout(r.Context(), h.pool, authedID, subjectID); err != nil {
		log.Printf("profiles: delete about: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not delete about-card")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetSharedAbout returns the about-card that {authorID} wrote about the AUTHED
// user, but only if the author set visibility='subject' and the two are accepted
// friends. Every not-allowed case (no card, still private, not friends, no such
// user) collapses to the same opaque 404 so you can't probe what others have
// written about you.
func (h *Handler) GetSharedAbout(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	authorID := chi.URLParam(r, "authorID")
	if authorID == "" || authorID == authedID {
		httpx.WriteError(w, http.StatusNotFound, errAboutNotFound)
		return
	}

	a, err := getAbout(r.Context(), h.pool, authorID, authedID)
	if err != nil {
		if errors.Is(err, ErrAboutNotFound) {
			httpx.WriteError(w, http.StatusNotFound, errAboutNotFound)
			return
		}
		log.Printf("profiles: get shared about: %v", err)
		httpx.WriteError(w, http.StatusNotFound, errAboutNotFound)
		return
	}
	if a.Visibility != AboutVisibilitySubject {
		httpx.WriteError(w, http.StatusNotFound, errAboutNotFound)
		return
	}

	friends, err := areFriends(r.Context(), h.pool, authorID, authedID)
	if err != nil {
		log.Printf("profiles: shared about friendship: %v", err)
		httpx.WriteError(w, http.StatusNotFound, errAboutNotFound)
		return
	}
	if !friends {
		httpx.WriteError(w, http.StatusNotFound, errAboutNotFound)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAboutResponse(a))
}

// guardSubject returns "" if the authed user may keep an about-card for subject
// (i.e. they're accepted friends), else the opaque subject_not_friend code. A DB
// failure is logged and collapsed to the same code.
func (h *Handler) guardSubject(ctx context.Context, authedID, subjectID string) string {
	exists, err := userExists(ctx, h.pool, subjectID)
	if err != nil {
		log.Printf("profiles: about user exists: %v", err)
		return errSubjectNotFriend
	}
	if !exists {
		return errSubjectNotFriend
	}
	friends, err := areFriends(ctx, h.pool, authedID, subjectID)
	if err != nil {
		log.Printf("profiles: about are friends: %v", err)
		return errSubjectNotFriend
	}
	if !friends {
		return errSubjectNotFriend
	}
	return ""
}

// normalizeContent validates that the body's content is a JSON object within the
// size cap and returns it as compact raw JSON. Missing/null content becomes "{}".
func normalizeContent(raw json.RawMessage) (json.RawMessage, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{}`), ""
	}
	if len(raw) > maxAboutContent {
		return nil, errContentTooLarge
	}
	// Must be a JSON object (not an array/scalar) so the client's keyed shape holds.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errInvalidContent
	}
	return raw, ""
}
