// Package profiles owns the self-card: a user's own published profile (base
// fields + freeform bio), what friends see when they view you. It is distinct
// from the about-card (notes a user keeps ABOUT another), which is client-local
// for now. Age is always derived from birthday, never stored.
package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

const (
	updateBodyLimit = 1 << 16 // 64 KiB — bios are text, not blobs.

	maxDisplayName = 100
	maxCountry     = 100
	maxTimezone    = 64
	maxBio         = 4000
	maxNickname    = 100
	maxCity        = 120
	// Jots are short text; this is generous while still bounding one row.
	maxNotesBytes = 1 << 15 // 32 KiB

	birthdayLayout = "2006-01-02"

	// Client-branching error codes.
	errInvalidBirthday   = "invalid_birthday"
	errInvalidVisibility = "invalid_visibility"
	errFieldTooLong      = "field_too_long"
	errProfileNotFound   = "profile_not_found"
)

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// profileResponse is the JSON-safe view of a self-card. Birthday is emitted as
// "YYYY-MM-DD" (or null); age is derived from it at read time. updated_at is nil
// for a profile that has never been saved. avatar_url/banner_url are
// server-relative, versioned serve-URLs (see types.MediaURL) — null until the
// user uploads that media kind.
type profileResponse struct {
	UserID      string  `json:"user_id"`
	DisplayName string  `json:"display_name"`
	Birthday    *string `json:"birthday"`
	Age         *int    `json:"age"`
	Country     string  `json:"country"`
	Timezone    string  `json:"timezone"`
	Bio         string  `json:"bio"`
	Visibility  string  `json:"visibility"`
	// Me-card facts. These are the per-viewer SHARED fields: on someone else's
	// read they are present only if you granted them that field, so a friend who
	// isn't in the right group sees them as empty/null rather than as a denial.
	Nickname  string          `json:"nickname"`
	City      string          `json:"city"`
	Notes     json.RawMessage `json:"notes"`
	AvatarURL *string         `json:"avatar_url"`
	BannerURL *string         `json:"banner_url"`
	UpdatedAt *time.Time      `json:"updated_at"`
}

// attachMedia fills the response's avatar/banner URLs from the user_media
// metadata. A failure is logged but never sinks the profile response — the
// card is still useful without its images.
func (h *Handler) attachMedia(ctx context.Context, resp *profileResponse) {
	avatar, banner, err := mediaURLs(ctx, h.pool, resp.UserID)
	if err != nil {
		logx.Error("profiles: media urls: %v", err)
		return
	}
	resp.AvatarURL = avatar
	resp.BannerURL = banner
}

// toResponse renders a stored record into the wire shape, deriving age from the
// birthday relative to `now`.
func toResponse(p *profileRecord, now time.Time) profileResponse {
	resp := profileResponse{
		UserID:      p.UserID,
		DisplayName: p.DisplayName,
		Country:     p.Country,
		Timezone:    p.Timezone,
		Bio:         p.Bio,
		Visibility:  p.Visibility,
		Nickname:    p.Nickname,
		City:        p.City,
		Notes:       notesOrEmpty(p.Notes),
	}
	if !p.UpdatedAt.IsZero() {
		resp.UpdatedAt = &p.UpdatedAt
	}
	if p.Birthday != nil {
		s := p.Birthday.Format(birthdayLayout)
		resp.Birthday = &s
		resp.Age = deriveAge(*p.Birthday, now)
	}
	return resp
}

// deriveAge computes whole years from birthday to now, decrementing if this
// year's birthday hasn't happened yet.
func deriveAge(birthday, now time.Time) *int {
	years := now.Year() - birthday.Year()
	if now.Month() < birthday.Month() || (now.Month() == birthday.Month() && now.Day() < birthday.Day()) {
		years--
	}
	if years < 0 {
		years = 0
	}
	return &years
}

// defaultProfile is what GetMine returns before the user has saved anything, so
// the client always receives an editable shape.
func defaultProfile(userID string) profileResponse {
	return profileResponse{UserID: userID, Visibility: VisibilityFriends}
}

// GetMine returns the authed user's own self-card (or an empty default).
func (h *Handler) GetMine(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	p, err := getProfile(r.Context(), h.pool, authedID)
	if err != nil {
		if errors.Is(err, ErrProfileNotFound) {
			// Media can exist before the text card is first saved, so the
			// default shape still carries the URLs.
			resp := defaultProfile(authedID)
			h.attachMedia(r.Context(), &resp)
			httpx.WriteJSON(w, http.StatusOK, resp)
			return
		}
		logx.Error("profiles: get mine: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch profile")
		return
	}
	resp := toResponse(p, time.Now())
	h.attachMedia(r.Context(), &resp)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

type updateProfileRequest struct {
	DisplayName string          `json:"display_name"`
	Birthday    string          `json:"birthday"` // "YYYY-MM-DD" or "" to clear
	Country     string          `json:"country"`
	Timezone    string          `json:"timezone"`
	Bio         string          `json:"bio"`
	Visibility  string          `json:"visibility"`
	Nickname    string          `json:"nickname"`
	City        string          `json:"city"`
	Notes       json.RawMessage `json:"notes"` // opaque array of jots
}

// UpdateMine upserts the authed user's self-card.
func (h *Handler) UpdateMine(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, updateBodyLimit)

	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req updateProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	rec, code := req.toRecord(authedID)
	if code != "" {
		httpx.WriteError(w, http.StatusBadRequest, code)
		return
	}

	saved, err := upsertProfile(r.Context(), h.pool, rec)
	if err != nil {
		logx.Error("profiles: upsert: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not save profile")
		return
	}
	resp := toResponse(saved, time.Now())
	h.attachMedia(r.Context(), &resp)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// toRecord validates and normalizes the request into a storable record. Returns
// a non-empty error code (for a 400) on invalid input.
func (req *updateProfileRequest) toRecord(userID string) (*profileRecord, string) {
	displayName := strings.TrimSpace(req.DisplayName)
	country := strings.TrimSpace(req.Country)
	timezone := strings.TrimSpace(req.Timezone)
	bio := strings.TrimSpace(req.Bio)

	nickname := strings.TrimSpace(req.Nickname)
	city := strings.TrimSpace(req.City)

	if len(displayName) > maxDisplayName || len(country) > maxCountry ||
		len(timezone) > maxTimezone || len(bio) > maxBio ||
		len(nickname) > maxNickname || len(city) > maxCity || len(req.Notes) > maxNotesBytes {
		return nil, errFieldTooLong
	}

	visibility := strings.TrimSpace(req.Visibility)
	if visibility == "" {
		visibility = VisibilityFriends
	}
	if visibility != VisibilityFriends && visibility != VisibilityPrivate {
		return nil, errInvalidVisibility
	}

	var birthday *time.Time
	if bd := strings.TrimSpace(req.Birthday); bd != "" {
		t, err := time.Parse(birthdayLayout, bd)
		if err != nil {
			return nil, errInvalidBirthday
		}
		birthday = &t
	}

	return &profileRecord{
		UserID:      userID,
		DisplayName: displayName,
		Birthday:    birthday,
		Country:     country,
		Timezone:    timezone,
		Bio:         bio,
		Visibility:  visibility,
		Nickname:    nickname,
		City:        city,
		Notes:       notesOrEmpty(req.Notes),
	}, ""
}

// GetForUser returns another user's self-card, subject to visibility. Viewing
// your own id is allowed (delegates to the same path as GetMine). For anyone
// else, the profile is returned only if you're accepted friends AND the owner's
// visibility is 'friends'. Every not-allowed case collapses to the same opaque
// 404 profile_not_found so you can't probe whether a user exists, is your
// friend, or has set their profile private.
func (h *Handler) GetForUser(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	targetID := chi.URLParam(r, "userID")
	if targetID == "" {
		httpx.WriteError(w, http.StatusNotFound, errProfileNotFound)
		return
	}

	p, code := h.resolveVisibleProfile(r.Context(), authedID, targetID)
	if code != "" {
		httpx.WriteError(w, http.StatusNotFound, code)
		return
	}
	resp := toResponse(p, time.Now())
	// Your own card is never redacted; anyone else sees only the fields you
	// granted THEM (see redactUnshared).
	if targetID != authedID {
		shared, err := sharedFieldsFor(r.Context(), h.pool, targetID, authedID)
		if err != nil {
			logx.Error("profiles: shared fields: %v", err)
			shared = map[string]bool{} // fail CLOSED - share nothing on error
		}
		redactUnshared(&resp, shared)
	}
	h.attachMedia(r.Context(), &resp)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// redactUnshared blanks every per-viewer field the viewer wasn't granted. The
// base card (name, bio, avatar, banner) stays visible to any accepted friend, as
// it always has - sharing governs the FACTS the Me card added, and an ungranted
// field simply reads as unset rather than as a refusal, so a viewer can't tell
// "not shared with me" from "never filled in".
func redactUnshared(resp *profileResponse, shared map[string]bool) {
	if !shared[FieldBirthday] {
		resp.Birthday = nil
		resp.Age = nil
	}
	if !shared[FieldLocation] {
		resp.Country = ""
		resp.Timezone = ""
		resp.City = ""
	}
	if !shared[FieldNickname] {
		resp.Nickname = ""
	}
	resp.Notes = filterNoteCategories(resp.Notes, shared)
}

// filterNoteCategories keeps only the note CATEGORIES this viewer was granted.
//
// Notes are published as an object keyed by category id — {"favorites": {...},
// "dislikes": {...}} — and each category is granted separately as
// "note:<id>". The server still never reads a note: it only decides which
// top-level keys survive, exactly as it decides which fields do. An owner who
// shared nothing gets an empty object, which reads as "no notes", not as a
// refusal.
func filterNoteCategories(raw json.RawMessage, shared map[string]bool) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	var byCategory map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byCategory); err != nil {
		// Not an object (an older row stored an array) — nothing to filter per
		// category, so share nothing rather than guess.
		return json.RawMessage("{}")
	}
	kept := map[string]json.RawMessage{}
	for id, body := range byCategory {
		if shared[NotePrefix+id] {
			kept[id] = body
		}
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}

// notesOrEmpty normalises stored/incoming jots to a valid JSON array, so the
// wire shape is never null and the jsonb column never gets an empty string.
func notesOrEmpty(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return json.RawMessage(raw)
}

// resolveVisibleProfile applies the authorization rules and returns either the
// record or an opaque error code (currently always errProfileNotFound on any
// not-allowed/not-found path). A 500-class DB failure is logged and also
// collapsed to the opaque code so the handler stays simple; acceptable at this
// scale, and it never leaks existence.
func (h *Handler) resolveVisibleProfile(ctx context.Context, authedID, targetID string) (*profileRecord, string) {
	if targetID == authedID {
		p, err := getProfile(ctx, h.pool, authedID)
		if err != nil {
			if errors.Is(err, ErrProfileNotFound) {
				// Represent the never-saved self-card as an empty record so the
				// caller still gets a 200 with a usable shape.
				return &profileRecord{UserID: authedID, Visibility: VisibilityFriends}, ""
			}
			logx.Error("profiles: get own (via for-user): %v", err)
			return nil, errProfileNotFound
		}
		return p, ""
	}

	exists, err := userExists(ctx, h.pool, targetID)
	if err != nil {
		logx.Error("profiles: user exists: %v", err)
		return nil, errProfileNotFound
	}
	if !exists {
		return nil, errProfileNotFound
	}

	friends, err := areFriends(ctx, h.pool, authedID, targetID)
	if err != nil {
		logx.Error("profiles: are friends: %v", err)
		return nil, errProfileNotFound
	}
	if !friends {
		return nil, errProfileNotFound
	}

	p, err := getProfile(ctx, h.pool, targetID)
	if err != nil {
		if errors.Is(err, ErrProfileNotFound) {
			return nil, errProfileNotFound
		}
		logx.Error("profiles: get for user: %v", err)
		return nil, errProfileNotFound
	}
	if p.Visibility != VisibilityFriends {
		return nil, errProfileNotFound
	}
	return p, ""
}
