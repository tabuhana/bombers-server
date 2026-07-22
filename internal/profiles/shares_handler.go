package profiles

import (
	"encoding/json"
	"net/http"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

// The sharing endpoints: who may see which fields of your self-card.
//
// The wire shape is deliberately dumb — `{"birthday": ["userId", …], …}`. The
// client owns the grouping (its user-made, user-deletable relationships) and
// sends the resolved membership; the server stores grants and answers reads.
// Nothing here knows a group exists, which is exactly why groups can be created
// and deleted freely on the client without a migration or a server deploy.

const (
	sharesBodyLimit = 1 << 18 // 256 KiB — ids × fields, generous.

	errUnknownField  = "unknown_share_field"
	errTooManyShares = "too_many_shares"
)

// GetMyShares returns the authed user's whole grant map. Every known field is
// present (empty list = private), so the client can render the picker directly
// from the response.
func (h *Handler) GetMyShares(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	shares, err := getShares(r.Context(), h.pool, authedID)
	if err != nil {
		logx.Error("profiles: get shares: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch shares")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, shares)
}

// PutMyShares REPLACES the grant map with the body. A publish states the whole
// intent, so anything absent is revoked — that's what makes "I deleted that
// group" or "I dropped Family from my birthday" take effect without a separate
// revoke call.
func (h *Handler) PutMyShares(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req map[string][]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, sharesBodyLimit)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_body")
		return
	}

	// Reject unknown field keys loudly: a typo that silently stored a grant
	// nobody could ever read would look like sharing that isn't working.
	for field := range req {
		if !knownFields[field] {
			httpx.WriteError(w, http.StatusBadRequest, errUnknownField)
			return
		}
	}
	if countGrants(req) > maxGrantsPerPublish {
		httpx.WriteError(w, http.StatusBadRequest, errTooManyShares)
		return
	}

	if err := replaceShares(r.Context(), h.pool, authedID, req); err != nil {
		logx.Error("profiles: replace shares: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not save shares")
		return
	}

	// Echo the stored truth back, not the request: non-friend viewers were
	// dropped, so the client learns which grants actually took.
	shares, err := getShares(r.Context(), h.pool, authedID)
	if err != nil {
		logx.Error("profiles: get shares after save: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not fetch shares")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, shares)
}
