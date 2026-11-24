package patchnotes

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

// The HTTP surface. Reading is open to any authenticated user — it is the app
// drawing its patch-notes screen. Writing is operator-only and ADMIN-gated at
// the route, like every other store.

const (
	errNotFound     = "patch_notes_not_found"
	errInvalidVer   = "invalid_version"
	errInvalidNote  = "invalid_patch_notes"
	errNoteTooLarge = "patch_notes_too_large"

	// A full note is 20 parts of at most 4200 characters; this is several times
	// that, so it only ever stops a mistake.
	maxBodyBytes = 256 << 10
)

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// List → GET /patch-notes : every version's note, newest first.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	entries, err := List(r.Context(), h.pool)
	if err != nil {
		logx.Error("patchnotes: list: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not read patch notes")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"patch_notes": entries})
}

// Get → GET /patch-notes/{version}
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")
	if _, _, _, ok := parseVersion(version); !ok {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidVer)
		return
	}
	entry, err := Get(r.Context(), h.pool, version)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, errNotFound)
		return
	}
	if err != nil {
		logx.Error("patchnotes: get %s: %v", version, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not read patch notes")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, entry)
}

// Put → PUT /patch-notes/{version} : write a version's note. ADMIN-ONLY.
// 201 when it is new, 200 when it replaced one.
//
// The version and the body are both judged before the database is touched.
func (h *Handler) Put(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")
	major, minor, patch, ok := parseVersion(version)
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidVer)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, errNoteTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, errInvalidNote)
		return
	}
	parts, err := parseNote(raw)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidNote)
		return
	}

	inserted, err := Put(r.Context(), h.pool, version, major, minor, patch, parts)
	if err != nil {
		logx.Error("patchnotes: put %s: %v", version, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not write patch notes")
		return
	}

	status := http.StatusOK
	if inserted {
		status = http.StatusCreated
	}
	logx.Info("patchnotes: wrote %s (%d parts, new=%v)", version, len(parts), inserted)
	httpx.WriteJSON(w, status, Entry{Version: version, Parts: parts})
}

// Delete → DELETE /patch-notes/{version} : ADMIN-ONLY. Idempotent — removing
// what isn't there reports `removed: false` rather than 404, the same answer
// releases' unpublish gives.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")
	if _, _, _, ok := parseVersion(version); !ok {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidVer)
		return
	}
	removed, err := Delete(r.Context(), h.pool, version)
	if err != nil {
		logx.Error("patchnotes: delete %s: %v", version, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not delete patch notes")
		return
	}
	if removed {
		logx.Info("patchnotes: deleted %s", version)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"version": version, "removed": removed})
}
