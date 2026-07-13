package media

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/types"
)

const (
	// Per-kind byte caps. Avatars are small squares; banners are wide strips.
	// GIFs are allowed, hence caps roomier than a single still image needs.
	maxAvatarBytes = 5 << 20  // 5 MiB
	maxBannerBytes = 10 << 20 // 10 MiB

	// Client-branching error codes.
	errInvalidMediaKind     = "invalid_media_kind"
	errUnsupportedMediaType = "unsupported_media_type"
	errMediaTooLarge        = "media_too_large"
	errMediaNotFound        = "media_not_found"
)

// allowedContentTypes is the accept-list for uploads, keyed by the type
// http.DetectContentType sniffs from the actual bytes — the declared header is
// ignored, so a mislabeled or malicious body can't smuggle a non-image in.
var allowedContentTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

type Handler struct {
	pool    *pgxpool.Pool
	storage Store
}

func NewHandler(pool *pgxpool.Pool, storage Store) *Handler {
	return &Handler{pool: pool, storage: storage}
}

// mediaResponse is the JSON shape returned by a successful upload — the same
// metadata the profile endpoints expose, so the client can update its view
// without refetching the profile.
type mediaResponse struct {
	Kind        string    `json:"kind"`
	URL         string    `json:"url"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toResponse(m *mediaRecord) mediaResponse {
	return mediaResponse{
		Kind:        m.Kind,
		URL:         types.MediaURL(m.UserID, m.Kind, m.UpdatedAt),
		ContentType: m.ContentType,
		SizeBytes:   m.SizeBytes,
		UpdatedAt:   m.UpdatedAt,
	}
}

func maxBytesFor(kind string) int64 {
	if kind == types.MediaKindBanner {
		return maxBannerBytes
	}
	return maxAvatarBytes
}

// Upload sets (or replaces) the authed user's avatar or banner. The body is
// the raw image bytes — no multipart envelope, keeping the API client-agnostic
// and trivial to call. The content type is sniffed from the bytes, never
// trusted from the header. PUT semantics: same key every time, so a reupload
// replaces the previous image in place.
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	kind := chi.URLParam(r, "kind")
	if !types.ValidMediaKind(kind) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidMediaKind)
		return
	}

	// Buffer the whole image (bounded by the cap) — we need the full bytes to
	// sniff the type and know the exact size before the object write. At these
	// caps and friends-and-family scale, in-memory is the boring right answer.
	limit := maxBytesFor(kind)
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, errMediaTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	contentType := http.DetectContentType(data)
	if !allowedContentTypes[contentType] {
		httpx.WriteError(w, http.StatusUnsupportedMediaType, errUnsupportedMediaType)
		return
	}

	if err := h.storage.Put(r.Context(), authedID, kind, data, contentType); err != nil {
		logx.Error("media: put object: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store media")
		return
	}

	m, err := upsertMedia(r.Context(), h.pool, authedID, kind, contentType, int64(len(data)))
	if err != nil {
		logx.Error("media: upsert metadata: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store media")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(m))
}

// Delete clears the authed user's avatar or banner. Idempotent — deleting
// media that doesn't exist is a successful no-op (mirrors DELETE /me/about).
// The metadata row goes first: once it's gone the media stops being served
// and stops appearing on the profile, and a leftover object is harmless (it
// gets overwritten by the next upload to the same key).
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	kind := chi.URLParam(r, "kind")
	if !types.ValidMediaKind(kind) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidMediaKind)
		return
	}

	if err := deleteMedia(r.Context(), h.pool, authedID, kind); err != nil {
		logx.Error("media: delete metadata: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not delete media")
		return
	}
	if err := h.storage.Remove(r.Context(), authedID, kind); err != nil {
		// Best-effort: the row is gone so nothing serves or links this object
		// anymore; log and move on rather than failing the delete.
		logx.Error("media: remove object: %v", err)
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// Serve streams a user's avatar or banner through the server — the
// pass-through the product requires. The client never touches the bucket; the
// server checks authorization on every fetch. Allowed viewers: the owner
// always, and an accepted friend when the owner's profile visibility is
// 'friends'. Every not-allowed case — not friends, owner set private, no such
// user, no media uploaded, even a bogus kind — collapses to the same opaque
// 404 media_not_found so the endpoint can't be used to probe.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	ownerID := chi.URLParam(r, "userID")
	kind := chi.URLParam(r, "kind")
	if ownerID == "" || !types.ValidMediaKind(kind) {
		httpx.WriteError(w, http.StatusNotFound, errMediaNotFound)
		return
	}

	if ownerID != authedID {
		friends, err := areFriends(r.Context(), h.pool, authedID, ownerID)
		if err != nil {
			logx.Error("media: are friends: %v", err)
			httpx.WriteError(w, http.StatusNotFound, errMediaNotFound)
			return
		}
		if !friends {
			httpx.WriteError(w, http.StatusNotFound, errMediaNotFound)
			return
		}
		visibility, err := profileVisibility(r.Context(), h.pool, ownerID)
		if err != nil {
			logx.Error("media: profile visibility: %v", err)
			httpx.WriteError(w, http.StatusNotFound, errMediaNotFound)
			return
		}
		if visibility != "friends" {
			httpx.WriteError(w, http.StatusNotFound, errMediaNotFound)
			return
		}
	}

	m, err := getMedia(r.Context(), h.pool, ownerID, kind)
	if err != nil {
		if !errors.Is(err, ErrMediaNotFound) {
			logx.Error("media: get metadata: %v", err)
		}
		httpx.WriteError(w, http.StatusNotFound, errMediaNotFound)
		return
	}

	// Cheap conditional-request support: updated_at is the version, so the
	// ETag can answer an If-None-Match revalidation without touching the
	// object store at all.
	etag := fmt.Sprintf(`"%s-%d"`, m.Kind, m.UpdatedAt.Unix())
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	obj, err := h.storage.Get(r.Context(), ownerID, kind)
	if err != nil {
		// A metadata row without an object is an inconsistency worth logging
		// loudly, but the client just sees the same opaque 404.
		logx.Error("media: get object for %s/%s: %v", ownerID, kind, err)
		httpx.WriteError(w, http.StatusNotFound, errMediaNotFound)
		return
	}
	defer obj.Close()

	w.Header().Set("Content-Type", m.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(m.SizeBytes, 10))
	w.Header().Set("ETag", etag)
	// Private (per-viewer authorization) + revalidate-friendly. The profile's
	// ?v= cache-buster handles replaces; the ETag handles everything else.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, obj); err != nil {
		// Headers are flushed; nothing to send the client. Usually a canceled
		// download, not a server fault.
		logx.Error("media: stream object: %v", err)
	}
}
