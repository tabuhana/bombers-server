// Package noderecords owns the people axis: a node putting something in front
// of named friends, and collecting what they say back.
//
// TWO SIDES, because nobody writes anyone else's data (the client's hard rule):
// the OWNER publishes a record and names who may see it, and each RECIPIENT
// attaches their own response. That split is what makes it general — a record
// plus responses is an invite with RSVPs, a poll with votes, a shared list with
// additions, a challenge with everyone's results, or a "here, have this" that
// expects no answer.
//
// Both bodies are opaque JSON the server stores and hands back without looking
// inside (the dumb-blob rule sync and nodeshare already follow). The server has
// no idea what an invite is.
//
// It is the DURABLE sibling of internal/rooms. A room is live, in-memory, and
// dies when the last person leaves — right for a game lobby, wrong for an
// invite, which has to survive both people being offline and sit there for
// days. And it is not nodeshare, which sends a node's SOURCE one way.
//
// You may only address an accepted friend; every not-allowed case (non-friend,
// nonexistent, not yours, no such record) collapses to the same opaque 404 so
// nothing here can be used to probe.
package noderecords

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

const (
	// A record is TEXT — an invite, a question, a list. Not a bundle, which is
	// what nodeshare's 4 MiB is for. 64 KiB is already a lot of JSON, and a
	// generous cap on something small is how a blob store becomes a file store
	// by accident.
	bodyLimit = 64 << 10

	// How many people one record may be addressed to. This is a friends-and-
	// family app; a hundred is far past any real guest list, and the number
	// exists so a bug can't write an unbounded array.
	maxAudience = 100

	// Node ids carry a namespace (`core:weather`), so they are short.
	maxNodeIDLen = 128

	// Client-branching error codes. errRecord is deliberately opaque — it
	// covers "no such record", "not yours" and "not addressed to you" alike —
	// and errAudience likewise never says WHICH id was the problem.
	errInvalidBody = "invalid_body"
	errInvalidNode = "invalid_node_id"
	errAudience    = "audience_not_found"
	errRecord      = "record_not_found"
)

// Notify is how a write tells the people it concerns to come and re-read. It is
// handed in rather than imported so this package never learns what a WebSocket
// is. Nil means nobody is listening, which is how the tests build a handler.
type Notify func(userIDs []string, nodeID, fromID string)

type Handler struct {
	pool   *pgxpool.Pool
	notify Notify
}

func NewHandler(pool *pgxpool.Pool, notify Notify) *Handler {
	return &Handler{pool: pool, notify: notify}
}

// responseView is one person's answer as JSON. There is no id: a response is
// keyed by who wrote it, because a person has one answer per record.
type responseView struct {
	AuthorID       string          `json:"author_id"`
	AuthorUsername string          `json:"author_username"`
	Body           json.RawMessage `json:"body"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// recordView is a record plus its answers.
//
// `audience` goes out to the audience, not just the owner — a guest list, not a
// blind copy. That is deliberate: every use this was built for (who else is
// invited, who has voted, who is on the list) is the question a recipient
// actually has, and the responses would reveal most of it anyway.
type recordView struct {
	ID            string          `json:"id"`
	NodeID        string          `json:"node_id"`
	OwnerID       string          `json:"owner_id"`
	OwnerUsername string          `json:"owner_username"`
	Body          json.RawMessage `json:"body"`
	Audience      []string        `json:"audience"`
	Responses     []responseView  `json:"responses"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type writeRecordRequest struct {
	NodeID   string          `json:"node_id"`
	Body     json.RawMessage `json:"body"`
	Audience []string        `json:"audience"`
}

type writeResponseRequest struct {
	Body json.RawMessage `json:"body"`
}

// Create publishes a record addressed to accepted friends. The id is the
// server's: a client learns it from the response and keeps it. (A lost response
// therefore means a retry writes a second record — the same trade nodeshare
// makes, and at this scale a duplicate invite is visible and deletable.)
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)

	ownerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req writeRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	nodeID := strings.TrimSpace(req.NodeID)
	if nodeID == "" || len(nodeID) > maxNodeIDLen {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidNode)
		return
	}
	if !isJSONObject(req.Body) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}
	audience, tooMany := normalizeAudience(req.Audience, ownerID)
	if tooMany {
		httpx.WriteError(w, http.StatusBadRequest, errAudience)
		return
	}
	if !h.audienceAllowed(r.Context(), ownerID, audience) {
		httpx.WriteError(w, http.StatusNotFound, errAudience)
		return
	}

	rec := &record{
		ID:       ulid.Make().String(),
		NodeID:   nodeID,
		OwnerID:  ownerID,
		Body:     req.Body,
		Audience: audience,
	}
	if err := insertRecord(r.Context(), h.pool, rec); err != nil {
		logx.Error("noderecords: insert: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not send record")
		return
	}

	h.nudge(audience, nodeID, ownerID)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":         rec.ID,
		"created_at": rec.CreatedAt,
		"updated_at": rec.UpdatedAt,
	})
}

// Update replaces a record's body and audience — an event moving to eight
// o'clock, or one more person being invited. Owner only; anyone else's record
// is the same opaque 404 as one that isn't there.
//
// It REPLACES rather than patches, including the audience, so taking somebody
// off the list is sending the list again without them. The responses are left
// alone: a guest who already said yes has said yes to a thing that changed, and
// deciding whether that still counts belongs to the node, not here.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)

	ownerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		httpx.WriteError(w, http.StatusNotFound, errRecord)
		return
	}

	var req writeRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !isJSONObject(req.Body) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}
	audience, tooMany := normalizeAudience(req.Audience, ownerID)
	if tooMany {
		httpx.WriteError(w, http.StatusBadRequest, errAudience)
		return
	}
	if !h.audienceAllowed(r.Context(), ownerID, audience) {
		httpx.WriteError(w, http.StatusNotFound, errAudience)
		return
	}

	// Who it USED to go to, so somebody just taken off the list is told to
	// re-read as well — otherwise their copy sits there until something else
	// makes them look.
	before, err := recordHeader(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("noderecords: header: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not update record")
		return
	}

	rec := &record{ID: id, OwnerID: ownerID, Body: req.Body, Audience: audience}
	updated, err := updateRecord(r.Context(), h.pool, rec)
	if err != nil {
		logx.Error("noderecords: update: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not update record")
		return
	}
	if !updated {
		httpx.WriteError(w, http.StatusNotFound, errRecord)
		return
	}

	told := audience
	if before != nil {
		told = union(audience, before.Audience)
	}
	h.nudge(told, rec.NodeID, ownerID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id":         rec.ID,
		"created_at": rec.CreatedAt,
		"updated_at": rec.UpdatedAt,
	})
}

// Withdraw deletes a record and, with it, the answers to it. Owner only.
func (h *Handler) Withdraw(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		httpx.WriteError(w, http.StatusNotFound, errRecord)
		return
	}

	// Read the audience before it is gone, so the people holding a copy are
	// told it is withdrawn.
	before, err := recordHeader(r.Context(), h.pool, id)
	if err != nil {
		logx.Error("noderecords: header: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not withdraw record")
		return
	}

	deleted, err := deleteRecord(r.Context(), h.pool, id, ownerID)
	if err != nil {
		logx.Error("noderecords: delete: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not withdraw record")
		return
	}
	if !deleted {
		httpx.WriteError(w, http.StatusNotFound, errRecord)
		return
	}

	if before != nil {
		h.nudge(before.Audience, before.NodeID, ownerID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListReceived returns what one node has put in front of me, newest first, each
// with its answers. An empty inbox is a 200 with an empty list.
func (h *Handler) ListReceived(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, listReceivedSQL)
}

// ListSent returns what one node of mine has sent, newest first, each with its
// answers — the "who's coming" read.
func (h *Handler) ListSent(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, listSentSQL)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, sql string) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// The node id is REQUIRED, never defaulted to "everything": a reader asks
	// on behalf of one node, and a missing parameter is a client bug, not a
	// request to hand over every node's records at once.
	nodeID := strings.TrimSpace(r.URL.Query().Get("node"))
	if nodeID == "" || len(nodeID) > maxNodeIDLen {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidNode)
		return
	}

	rows, err := listRecords(r.Context(), h.pool, sql, nodeID, userID)
	if err != nil {
		logx.Error("noderecords: list: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not list records")
		return
	}

	ids := make([]string, len(rows))
	for i, rec := range rows {
		ids[i] = rec.ID
	}
	answers, err := listResponses(r.Context(), h.pool, ids)
	if err != nil {
		logx.Error("noderecords: list responses: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not list records")
		return
	}

	out := make([]recordView, len(rows))
	for i, rec := range rows {
		out[i] = toView(rec, answers[rec.ID])
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"records": out})
}

// Respond attaches MY answer to a record, replacing my previous one. Only the
// people it was addressed to may answer — plus the owner, since somebody
// running a poll has a vote too.
func (h *Handler) Respond(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)

	authorID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		httpx.WriteError(w, http.StatusNotFound, errRecord)
		return
	}

	var req writeResponseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !isJSONObject(req.Body) {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}

	head := h.readableRecord(r.Context(), id, authorID)
	if head == nil {
		httpx.WriteError(w, http.StatusNotFound, errRecord)
		return
	}

	res := &response{RecordID: id, AuthorID: authorID, Body: req.Body}
	if err := upsertResponse(r.Context(), h.pool, res); err != nil {
		logx.Error("noderecords: upsert response: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not save response")
		return
	}

	// The owner asked the question, so the owner hears the answer. The rest of
	// the audience hears it too, because a guest list where only the host can
	// see who is coming is half a feature.
	h.nudge(append(audienceWithout(head, authorID), head.OwnerID), head.NodeID, authorID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"created_at": res.CreatedAt,
		"updated_at": res.UpdatedAt,
	})
}

// Unrespond takes my answer back, which is not the same as answering "no" —
// that is a body like any other. Idempotent: never having answered is the state
// this leaves you in either way.
func (h *Handler) Unrespond(w http.ResponseWriter, r *http.Request) {
	authorID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		httpx.WriteError(w, http.StatusNotFound, errRecord)
		return
	}

	head := h.readableRecord(r.Context(), id, authorID)
	if head == nil {
		httpx.WriteError(w, http.StatusNotFound, errRecord)
		return
	}
	if _, err := deleteResponse(r.Context(), h.pool, id, authorID); err != nil {
		logx.Error("noderecords: delete response: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not withdraw response")
		return
	}

	h.nudge(append(audienceWithout(head, authorID), head.OwnerID), head.NodeID, authorID)
	w.WriteHeader(http.StatusNoContent)
}

// readableRecord returns the record's header if this user may see it (owner or
// audience), else nil. A DB failure is logged and reads as "no", so a refusal
// never depends on a query having succeeded.
func (h *Handler) readableRecord(ctx context.Context, id, userID string) *header {
	head, err := recordHeader(ctx, h.pool, id)
	if err != nil {
		logx.Error("noderecords: header: %v", err)
		return nil
	}
	if head == nil {
		return nil
	}
	if head.OwnerID == userID || slices.Contains(head.Audience, userID) {
		return head
	}
	return nil
}

// audienceAllowed reports whether every id is an accepted friend of the owner.
// An empty audience is allowed: a record addressed to nobody is one only its
// owner can see, which is what an invite looks like before anyone is on it.
//
// A DB failure reads as "not allowed" so the handler never leaks whether a user
// exists by letting a write through when the check couldn't run.
func (h *Handler) audienceAllowed(ctx context.Context, ownerID string, audience []string) bool {
	if len(audience) == 0 {
		return true
	}
	friends, err := acceptedFriends(ctx, h.pool, ownerID, audience)
	if err != nil {
		logx.Error("noderecords: accepted friends: %v", err)
		return false
	}
	for _, id := range audience {
		if !friends[id] {
			return false
		}
	}
	return true
}

// nudge tells people something changed, never what. Self is dropped: the device
// that just wrote already knows, and its other windows re-read on their own.
func (h *Handler) nudge(userIDs []string, nodeID, fromID string) {
	if h.notify == nil {
		return
	}
	told := make([]string, 0, len(userIDs))
	seen := map[string]bool{fromID: true}
	for _, id := range userIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		told = append(told, id)
	}
	if len(told) == 0 {
		return
	}
	h.notify(told, nodeID, fromID)
}

// normalizeAudience cleans a submitted guest list: trimmed, de-duplicated, and
// without the owner (they always see their own record, so naming themselves is
// a no-op rather than an error). Reports whether it was over the cap.
func normalizeAudience(ids []string, ownerID string) ([]string, bool) {
	if len(ids) > maxAudience {
		return nil, true
	}
	out := make([]string, 0, len(ids))
	seen := map[string]bool{ownerID: true}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, false
}

// union merges two id lists, keeping the first list's order and dropping
// duplicates.
func union(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	seen := make(map[string]bool, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, id := range list {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// audienceWithout copies a record's audience minus one id, without touching the
// slice the header holds.
func audienceWithout(head *header, exclude string) []string {
	out := make([]string, 0, len(head.Audience))
	for _, id := range head.Audience {
		if id != exclude {
			out = append(out, id)
		}
	}
	return out
}

func toView(rec record, answers []response) recordView {
	out := recordView{
		ID:            rec.ID,
		NodeID:        rec.NodeID,
		OwnerID:       rec.OwnerID,
		OwnerUsername: rec.OwnerUsername,
		Body:          rec.Body,
		Audience:      rec.Audience,
		Responses:     make([]responseView, len(answers)),
		CreatedAt:     rec.CreatedAt,
		UpdatedAt:     rec.UpdatedAt,
	}
	// A nil audience would marshal as `null`; an empty list is what it means.
	if out.Audience == nil {
		out.Audience = []string{}
	}
	for i, res := range answers {
		out.Responses[i] = responseView{
			AuthorID:       res.AuthorID,
			AuthorUsername: res.AuthorUsername,
			Body:           res.Body,
			CreatedAt:      res.CreatedAt,
			UpdatedAt:      res.UpdatedAt,
		}
	}
	return out
}

// isJSONObject reports whether raw is a JSON object ({...}). The decoder has
// already validated it as JSON; this rejects arrays, strings and null so a
// stored body is always the object shape a node can add fields to later.
func isJSONObject(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}
