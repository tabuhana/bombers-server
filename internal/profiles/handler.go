// Package profiles owns the self-card: a user's own published profile, what
// friends see when they view you. It is distinct from the about-card (notes a
// user keeps ABOUT another), which is client-local for now. Age is always
// derived from birthday, never stored.
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
	updateBodyLimit = 1 << 16 // 64 KiB — a card is a handful of short text fields.

	maxDisplayName = 100
	maxCountry     = 100
	maxTimezone    = 64
	maxNickname    = 100
	maxCity        = 120

	// Crop bounds: x and y are CSS object-position percentages, scale is zoom
	// from none to 4×.
	maxCropPosition = 100
	minCropScale    = 1
	maxCropScale    = 4

	// A whole birthday: the one shape an age can be worked out from. Every shape
	// a birthday may take is in birthdayLayouts.
	wholeBirthdayLayout = "2006-01-02"

	// Client-branching error codes.
	errInvalidBirthday   = "invalid_birthday"
	errInvalidVisibility = "invalid_visibility"
	errFieldTooLong      = "field_too_long"
	errInvalidCrop       = "invalid_crop"
	errProfileNotFound   = "profile_not_found"
)

// Notify is called when a change here matters to somebody else — today, when
// you save your own card, so the friends holding a copy of
// it stop showing last month's details. ownerID is whose card changed, so a
// client re-reads that one person instead of every card it holds. A function
// rather than an import of the notify package: a domain reaching into another
// domain is the thing this codebase does not do.
//
// Nil means nobody is listening, which is how the tests build a handler.
type Notify func(ownerID string, viewerIDs []string)

type Handler struct {
	pool   *pgxpool.Pool
	notify Notify
}

func NewHandler(pool *pgxpool.Pool, notify Notify) *Handler {
	return &Handler{pool: pool, notify: notify}
}

// nudgeFriends tells everyone who holds a copy of this user's card that it
// changed. Best-effort and deliberately after the write: a failure to look up
// the friends list must not fail a save that already succeeded.
func (h *Handler) nudgeFriends(ctx context.Context, userID string) {
	if h.notify == nil {
		return
	}
	ids, err := acceptedFriendIDs(ctx, h.pool, userID)
	if err != nil {
		logx.Error("profiles: notify friends: %v", err)
		return
	}
	h.notify(userID, ids)
}

// profileResponse is the JSON-safe view of a self-card. Birthday is emitted as
// it was stored — as much of the date as the user knows — or null; age is
// derived from it at read time, and only from a whole date. updated_at is nil
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
	Visibility  string  `json:"visibility"`
	// Me-card facts. The same for everyone you're linked to — there is no
	// per-friend choice to make about your own birthday, so any accepted friend
	// reads these.
	Nickname  string  `json:"nickname"`
	City      string  `json:"city"`
	AvatarURL *string `json:"avatar_url"`
	BannerURL *string `json:"banner_url"`
	// How each image sits in its frame (see crop). Never null — a card with no
	// framing stored carries the defaults — so a client has no missing case to
	// guess a meaning for.
	AvatarCrop crop       `json:"avatar_crop"`
	BannerCrop crop       `json:"banner_crop"`
	UpdatedAt  *time.Time `json:"updated_at"`
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

// toResponse renders a stored record into the wire shape, deriving age from a
// whole birthday relative to `now`.
func toResponse(p *profileRecord, now time.Time) profileResponse {
	resp := profileResponse{
		UserID:      p.UserID,
		DisplayName: p.DisplayName,
		Country:     p.Country,
		Timezone:    p.Timezone,
		Visibility:  p.Visibility,
		Nickname:    p.Nickname,
		City:        p.City,
		AvatarCrop:  p.AvatarCrop.orDefault(),
		BannerCrop:  p.BannerCrop.orDefault(),
	}
	if !p.UpdatedAt.IsZero() {
		resp.UpdatedAt = &p.UpdatedAt
	}
	if p.Birthday != nil {
		resp.Birthday = p.Birthday
		// Only a whole date has an age: without the year there's nothing to count
		// from, and without the day nobody can say whether this year's has passed.
		if bd, err := time.Parse(wholeBirthdayLayout, *p.Birthday); err == nil {
			resp.Age = deriveAge(bd, now)
		}
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
// the client always receives an editable shape — crops included, at their
// defaults.
func defaultProfile(userID string) profileResponse {
	return profileResponse{
		UserID:     userID,
		Visibility: VisibilityFriends,
		AvatarCrop: defaultCrop,
		BannerCrop: defaultCrop,
	}
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
	DisplayName string `json:"display_name"`
	Birthday    string `json:"birthday"` // "YYYY-MM-DD", "MM-DD", "YYYY-MM", "YYYY", "MM", or "" to clear
	Country     string `json:"country"`
	Timezone    string `json:"timezone"`
	Visibility  string `json:"visibility"`
	Nickname    string `json:"nickname"`
	City        string `json:"city"`
	// Optional, unlike everything above: nil (omitted or null) keeps the stored
	// framing — see upsertProfileSQL for why.
	AvatarCrop *crop `json:"avatar_crop"`
	BannerCrop *crop `json:"banner_crop"`
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
	h.nudgeFriends(r.Context(), authedID)
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
	nickname := strings.TrimSpace(req.Nickname)
	city := strings.TrimSpace(req.City)

	if len(displayName) > maxDisplayName || len(country) > maxCountry ||
		len(timezone) > maxTimezone || len(nickname) > maxNickname ||
		len(city) > maxCity {
		return nil, errFieldTooLong
	}

	visibility := strings.TrimSpace(req.Visibility)
	if visibility == "" {
		visibility = VisibilityFriends
	}
	if visibility != VisibilityFriends && visibility != VisibilityPrivate {
		return nil, errInvalidVisibility
	}

	var birthday *string // nil when cleared, which stores NULL
	if bd := strings.TrimSpace(req.Birthday); bd != "" {
		if !validBirthday(bd) {
			return nil, errInvalidBirthday
		}
		birthday = &bd
	}

	for _, c := range []*crop{req.AvatarCrop, req.BannerCrop} {
		if c != nil && !c.valid() {
			return nil, errInvalidCrop
		}
	}

	return &profileRecord{
		UserID:      userID,
		DisplayName: displayName,
		Birthday:    birthday,
		Country:     country,
		Timezone:    timezone,
		Visibility:  visibility,
		Nickname:    nickname,
		City:        city,
		AvatarCrop:  req.AvatarCrop,
		BannerCrop:  req.BannerCrop,
	}, ""
}

// birthdayLayouts are the shapes a birthday may take: as much of the date as the
// user knows. Each is exact — zero-padded, nothing either side — so "1990-3-4"
// and "3/14" are refused rather than guessed at.
var birthdayLayouts = []string{
	wholeBirthdayLayout, // "YYYY-MM-DD"
	"01-02",             // "MM-DD", no year
	"2006-01",           // "YYYY-MM"
	"2006",              // "YYYY"
	"01",                // "MM"
}

// validBirthday reports whether s takes one of birthdayLayouts' shapes and names
// a date that can exist. time.Parse does the calendar: a whole date has to be a
// real one, and a month and day with no year are checked against year 0, a leap
// year — so "02-29" passes, since somebody born on one still has a birthday. The
// one thing it lets through is year 0 itself, which nobody was born in.
func validBirthday(s string) bool {
	if strings.HasPrefix(s, "0000") {
		return false
	}
	for _, layout := range birthdayLayouts {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

// valid reports whether a crop can be drawn as sent: a position inside the frame
// and a zoom in range. Refused rather than clamped, so a client with a cropper
// bug hears about it instead of saving a quietly different framing.
func (c *crop) valid() bool {
	return c.X >= 0 && c.X <= maxCropPosition &&
		c.Y >= 0 && c.Y <= maxCropPosition &&
		c.Scale >= minCropScale && c.Scale <= maxCropScale
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
	// No redaction: the facts on a self-card are the same for everyone you're
	// linked to, and friendship + visibility (checked above) is the whole gate.
	resp := toResponse(p, time.Now())
	h.attachMedia(r.Context(), &resp)
	httpx.WriteJSON(w, http.StatusOK, resp)
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
				// caller still gets a 200 with a usable shape. It has no crops,
				// which toResponse renders as the 50/50/1 defaults.
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
