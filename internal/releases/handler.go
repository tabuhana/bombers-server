package releases

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/media"
)

// The HTTP surface. Reading is open to any authenticated user — the app checking
// for its own update, and the website serving a first install to someone who
// just signed up, are the same two requests. Publishing is operator-only and
// ADMIN-gated at the route, the same two-ways-into-one-operation the other
// stores have.

const (
	errReleaseNotFound = "release_not_found"

	errInvalidBody      = "invalid_body"
	errInvalidVersion   = "invalid_version"
	errInvalidArtifact  = "invalid_artifact"
	errMissingSignature = "missing_signature"
	errArtifactTooLarge = "artifact_too_large"
	errNoMediaStore     = "media_store_unavailable"
)

type Handler struct {
	pool    *pgxpool.Pool
	storage media.Store
}

func NewHandler(pool *pgxpool.Pool, storage media.Store) *Handler {
	return &Handler{pool: pool, storage: storage}
}

// platformEntry is one platform's half of an update manifest: what to fetch and
// what it should verify as.
type platformEntry struct {
	Signature string `json:"signature"`
	URL       string `json:"url"`
}

// manifest is the shape the Tauri updater expects, and the reason this handler
// doesn't get to invent its own. Field names and the RFC 3339 date are the
// plugin's contract, not ours.
type manifest struct {
	Version   string                   `json:"version"`
	Notes     string                   `json:"notes"`
	PubDate   string                   `json:"pub_date"`
	Platforms map[string]platformEntry `json:"platforms"`
}

// publicOrigin is where a client should come back to for the download, worked
// out from the request that just arrived rather than from configuration.
//
// The updater asked THIS server for the manifest, so the host it used is by
// definition one it can reach — which is more reliable than a configured value
// somebody has to remember to change, and it makes a LAN dev server work with no
// setup at all. Behind Caddy the scheme comes from X-Forwarded-Proto; a direct
// bind has no TLS and no proxy, and http is correct there.
func publicOrigin(r *http.Request) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		// A proxy chain sets a list; the first entry is the client's.
		scheme = strings.TrimSpace(strings.Split(proto, ",")[0])
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	return scheme + "://" + host
}

// Latest → GET /releases/latest : the update manifest, or 204 when there is
// nothing to take.
//
// The updater fills `current` in from the version it is running. 204 is the
// plugin's "no update" answer and is the normal case — almost every check any
// client ever makes lands here, which is why it costs one indexed row read and
// writes no body.
func (h *Handler) Latest(w http.ResponseWriter, r *http.Request) {
	rec, err := Latest(r.Context(), h.pool)
	if errors.Is(err, ErrNotFound) {
		// Nothing published yet. Not an error — a server with no releases is a
		// server whose clients are all up to date.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		logx.Error("releases: latest: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not read releases")
		return
	}

	// Deliberately equality, not "newer than". The client's store rule is
	// "different from what's published" so an operator rolling a bad build back
	// reaches everyone; the app half applies the same comparison, and the two
	// have to agree or a rollback would be offered here and refused there.
	if current := strings.TrimSpace(r.URL.Query().Get("current")); current != "" && current == rec.Version {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, manifest{
		Version: rec.Version,
		Notes:   rec.Notes,
		PubDate: rec.PublishedAt.UTC().Format(time.RFC3339),
		Platforms: map[string]platformEntry{
			rec.Platform: {
				Signature: rec.Signature,
				URL:       publicOrigin(r) + "/releases/" + rec.Version + "/download",
			},
		},
	})
}

// Download → GET /releases/{version}/download : the installer's bytes, streamed
// THROUGH the server. Never a bucket or presigned URL — the same rule profile
// media and pack assets follow.
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")
	if !ValidVersion(version) {
		httpx.WriteError(w, http.StatusNotFound, errReleaseNotFound)
		return
	}

	rec, err := Get(r.Context(), h.pool, version)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, errReleaseNotFound)
		return
	}
	if err != nil {
		logx.Error("releases: get %s: %v", version, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch release")
		return
	}
	if h.storage == nil {
		httpx.WriteError(w, http.StatusInternalServerError, errNoMediaStore)
		return
	}

	body, err := h.storage.GetObject(r.Context(), ArtifactKey(rec.Version, rec.Artifact))
	if err != nil {
		if errors.Is(err, media.ErrObjectNotFound) {
			// The row is there and the file isn't: a publish that stopped between
			// its two steps. Same answer as an unknown version — from out here
			// they are the same thing, an update you cannot take.
			httpx.WriteError(w, http.StatusNotFound, errReleaseNotFound)
			return
		}
		logx.Error("releases: read artifact %s: %v", version, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch release")
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", rec.ContentType)
	if rec.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(rec.Size, 10))
	}
	// So a browser save — the website's gated download — lands with the name the
	// operator published rather than "download".
	w.Header().Set("Content-Disposition", `attachment; filename="`+rec.Artifact+`"`)
	if _, err := io.Copy(w, body); err != nil {
		logx.Error("releases: stream artifact %s: %v", version, err)
	}
}

// publishRequest is what an operator declares before sending any bytes.
type publishRequest struct {
	Version   string `json:"version"`
	Notes     string `json:"notes"`
	Signature string `json:"signature"`
	Artifact  string `json:"artifact"`
	Platform  string `json:"platform"`
}

// Publish → POST /releases : declare a release. ADMIN-ONLY.
//
// Two steps like the pack and game stores, because an installer is binary: this
// records what is coming — the version, the notes, the signature the build
// produced and the filename it produced — and PublishArtifact sends the file.
//
// The signature is required HERE, at the first step, rather than checked later.
// A release without one is unusable by every client, and the cheapest place to
// refuse it is before an operator spends a minute uploading eighty megabytes.
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}

	req.Version = strings.TrimSpace(req.Version)
	req.Artifact = strings.TrimSpace(req.Artifact)
	req.Signature = strings.TrimSpace(req.Signature)
	if !ValidVersion(req.Version) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidVersion)
		return
	}
	if !ValidArtifact(req.Artifact) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidArtifact)
		return
	}
	if req.Signature == "" {
		httpx.WriteError(w, http.StatusBadRequest, errMissingSignature)
		return
	}

	// Republishing starts the file over: the size goes back to zero so Latest
	// skips this version until the new bytes land. Otherwise a re-publish would
	// keep offering the OLD installer under the new signature, which every
	// client would download and then refuse to install.
	published, err := Upsert(r.Context(), h.pool, Record{
		Version:   req.Version,
		Platform:  strings.TrimSpace(req.Platform),
		Notes:     req.Notes,
		Signature: req.Signature,
		Artifact:  req.Artifact,
		Size:      0,
	})
	if err != nil {
		logx.Error("releases: publish %s: %v", req.Version, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not publish release")
		return
	}
	// Any bytes already under this version are from a previous attempt and are
	// about to be replaced. Clear them now so a failed upload can't leave the old
	// installer sitting where the new one should be.
	if h.storage != nil {
		if err := h.storage.RemovePrefix(r.Context(), "releases/"+req.Version+"/"); err != nil {
			logx.Error("releases: clear stored artifact %s: %v", req.Version, err)
		}
	}

	// Old releases are dead weight: nothing points at them, and their installers
	// sit in object storage forever. Prune after the new one lands, so a failure
	// here can never be what loses you the release you just published.
	//
	// Deliberately not fatal. The publish SUCCEEDED — refusing it because the
	// tidying failed would be the tail wagging the dog, and the next publish
	// prunes again anyway.
	if gone, perr := PruneOld(r.Context(), h.pool, KeepReleases); perr != nil {
		logx.Error("releases: prune: %v", perr)
	} else {
		for _, old := range gone {
			if h.storage != nil {
				if derr := h.storage.RemovePrefix(r.Context(), "releases/"+old.Version+"/"); derr != nil {
					logx.Error("releases: remove pruned artifact %s: %v", old.Version, derr)
				}
			}
			logx.Info("releases: pruned %s (keeping the newest %d)", old.Version, KeepReleases)
		}
	}

	logx.Info("releases: declared %s (%s)", req.Version, req.Artifact)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"version":      req.Version,
		"artifact":     req.Artifact,
		"published_at": published.UTC().Format(time.RFC3339),
	})
}

// PublishArtifact → PUT /releases/{version}/artifact : the installer's raw
// bytes, as step two. ADMIN-ONLY.
//
// Body-is-the-bytes, no multipart envelope — the same shape profile media and
// pack assets use, so a publishing script needs nothing but a PUT. The FILENAME
// isn't in this request on purpose: it was declared at step one, so there is
// exactly one place that decides what the file is called and no way for the two
// steps to disagree.
func (h *Handler) PublishArtifact(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")
	if !ValidVersion(version) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidVersion)
		return
	}
	if h.storage == nil {
		httpx.WriteError(w, http.StatusInternalServerError, errNoMediaStore)
		return
	}

	// The version has to be declared first: it carries the filename these bytes
	// are stored under and the signature they'll be checked against.
	rec, err := Get(r.Context(), h.pool, version)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, errReleaseNotFound)
		return
	}
	if err != nil {
		logx.Error("releases: get %s: %v", version, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store artifact")
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ArtifactLimit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, errArtifactTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}
	if len(data) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}

	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	key := ArtifactKey(rec.Version, rec.Artifact)
	if err := h.storage.PutObject(r.Context(), key, data, contentType); err != nil {
		logx.Error("releases: put artifact %s: %v", version, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store artifact")
		return
	}
	// The size lands LAST, and it is what makes the release live: until this row
	// says how big the file is, Latest skips the version entirely.
	if err := SetArtifact(r.Context(), h.pool, rec.Version, rec.Artifact, contentType, int64(len(data))); err != nil {
		logx.Error("releases: record artifact %s: %v", version, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not store artifact")
		return
	}

	logx.Info("releases: published %s (%s, %d bytes)", rec.Version, rec.Artifact, len(data))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"version":    rec.Version,
		"artifact":   rec.Artifact,
		"size_bytes": len(data),
	})
}

// Unpublish → DELETE /releases/{version} : remove a release and its stored
// bytes. ADMIN-ONLY. Idempotent — removing what isn't there reports
// `removed: false` rather than 404, so a retry after a dropped response isn't an
// error (the contract the other stores' unpublish gives).
func (h *Handler) Unpublish(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")
	if !ValidVersion(version) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidVersion)
		return
	}

	removed, err := Delete(r.Context(), h.pool, version)
	if err != nil {
		logx.Error("releases: unpublish %s: %v", version, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not unpublish release")
		return
	}
	// Run even when no row went, so an interrupted publish's bytes are still
	// cleared — this is the only thing that ever removes them.
	if h.storage != nil {
		if err := h.storage.RemovePrefix(r.Context(), "releases/"+version+"/"); err != nil {
			logx.Error("releases: remove stored artifact %s: %v", version, err)
		}
	}
	if removed {
		logx.Info("releases: unpublished %s", version)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"version": version, "removed": removed})
}
