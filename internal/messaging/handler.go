// Package messaging owns direct messages: text DMs between two users, persisted
// indefinitely in Postgres (DMs are not ephemeral; rooms are). A message has a kind
// (kinds.go): what a person types is text, and the other kinds are minted by the
// server, never declared by a client. Pictures wait for the later S3 phase. A
// message can answer an earlier one of the same conversation, and either person
// can pin it or react to it. Live delivery carries the message: once a write is
// stored, the Notify func
// below (wired to internal/notify in main) pushes it to both people in the
// conversation, so a client applies it directly instead of going back to ask. This
// domain stays the durable REST source of truth — history for a fresh reader, a
// seq-ordered catch-up feed for one that missed frames, and a full-text search over
// the caller's own conversations. You may only message an accepted friend, and
// every not-allowed case (non-friend, blocked, nonexistent recipient) collapses to
// the same opaque 404 so you can't probe.
package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
	"github.com/rivo/uniseg"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

const (
	sendBodyLimit = 1 << 16 // 64 KiB — DMs are text, not blobs.
	maxBody       = 4000    // max characters per message
	maxNonce      = 64      // max characters of a client's nonce

	defaultHistoryLimit = 100
	maxHistoryLimit     = 200

	defaultConversationsLimit = 50
	maxConversationsLimit     = 200

	defaultUpdatesLimit = 200
	maxUpdatesLimit     = 500

	// maxPins is the most messages one conversation may keep pinned. An uncapped
	// list is a list nobody can use.
	maxPins = 50

	// A reaction is an emoji, not text. maxEmojiRunes and maxEmojiBytes bound the
	// size of one (cleanEmoji says what else it must be): the longest real sequences
	// are a four-person family with skin tones (about 11 runes) and a subdivision
	// flag (about 8), so 16 leaves headroom. A rune is at most 4 bytes, so the byte
	// cap cannot bind while the rune cap is 16; it is the bound that still holds if
	// the rune cap is ever raised. maxReactionEmoji and maxReactions bound how much
	// of a message reactions can take up: its distinct emoji, and all reactions on it
	// across everyone.
	maxEmojiRunes    = 16
	maxEmojiBytes    = 64
	maxReactionEmoji = 20
	maxReactions     = 50

	defaultSearchLimit = 50
	maxSearchLimit     = 100

	// maxQuery is the most characters a search may be.
	maxQuery = 200

	// replyPreviewChars is how much of the original a reply's preview carries.
	replyPreviewChars = 120

	// maxMentions is the most mention ids one message reports.
	maxMentions = 16

	// pushTimeout bounds the reads that build a frame once its write has landed.
	pushTimeout = 5 * time.Second

	// Client-branching error codes. errRecipient is deliberately opaque: it
	// covers non-friend, blocked, nonexistent, and self-target alike.
	// errMessageNotFound is the same idea for a message id: it covers missing,
	// already deleted and not yours, so an id can't be probed for.
	errInvalidBody      = "invalid_body"
	errEmptyBody        = "empty_body"
	errBodyTooLong      = "body_too_long"
	errNonceTooLong     = "nonce_too_long"
	errBadReply         = "bad_reply"
	errTooManyPins      = "too_many_pins"
	errBadEmoji         = "bad_emoji"
	errTooManyReactions = "too_many_reactions"
	errEmptyQuery       = "empty_query"
	errQueryTooLong     = "query_too_long"
	errRecipient        = "recipient_not_found"
	errMessageNotFound  = "message_not_found"
	// errBadCursor is a ?before= naming no message the caller may page from. It
	// is an error and not an empty page: an empty page reads as "there is nothing
	// older", which is a different statement.
	errBadCursor = "bad_cursor"
	// errConflictingCursors is ?around= and ?before= asked for together.
	errConflictingCursors = "conflicting_cursors"

	// The things a dm frame can say happened to a message. A pin and an unpin are
	// both opPin: the message in the frame says which, by its pinned_at. A reaction
	// added and one removed are both opReact: the message in the frame carries the
	// reactions as they now stand, relative to whoever is reading.
	opNew    = "new"
	opEdit   = "edit"
	opDelete = "delete"
	opPin    = "pin"
	opReact  = "react"
)

// KindDM is the notify kind messaging pushes under. It repeats the value of
// notify.KindDM because messaging may not import that package; a test in
// cmd/bombers pins the two together.
const KindDM = "dm"

// KindTyping and KindRead repeat notify.KindTyping and notify.KindRead for the
// same reason, and the same test pins them.
const (
	KindTyping = "typing"
	KindRead   = "read"
)

// Notify delivers one frame of the given kind to every listed user, if anything
// is wired up to hear it. It is how a stored message reaches the people in the
// conversation while they have the app open. A function rather than an import of
// the notify package: messaging has no business knowing what a WebSocket is, and
// a domain reaching into another domain is the thing this codebase does not do.
//
// Nil is a valid value and means nobody is listening — the console and the tests
// both build handlers that way.
type Notify func(userIDs []string, kind string, data any)

type Handler struct {
	db     dbExecutor
	begin  beginFunc
	notify Notify
}

func NewHandler(pool *pgxpool.Pool, notify Notify) *Handler {
	return newHandler(pool, pool.Begin, notify)
}

// newHandler is the seam: a test builds a Handler over a fake database.
func newHandler(db dbExecutor, begin beginFunc, notify Notify) *Handler {
	return &Handler{db: db, begin: begin, notify: notify}
}

// push delivers a frame to the listed users, if anything is wired up to hear it.
func (h *Handler) push(userIDs []string, kind string, data any) {
	if h.notify != nil {
		h.notify(userIDs, kind, data)
	}
}

// dmFrame is the `d` of a dm frame: what happened, who it happened to, and the
// message itself. `from` is always the message's sender, which is also who edited
// or deleted it (only the sender may); a pin or a reaction is not the sender's act,
// and `from` still names the author, so it never changes meaning from one op to the
// next.
// Nonce rides only on a new message, and only when the sender supplied one.
type dmFrame struct {
	Op      string          `json:"op"`
	From    string          `json:"from"`
	Message messageResponse `json:"message"`
	Nonce   string          `json:"nonce,omitempty"`
}

// pushDM tells BOTH people in the conversation what happened to a message. The
// sender is included on purpose: it is how their other windows and devices stay
// in step with the one that acted. The ids are the message's own two parties, so
// there is no list to get wrong.
//
// Each person gets a frame of their own, because the message is not the same for
// both: a reaction's `me` and the mentions reported are relative to whoever is
// reading, and one shared frame would hand somebody another person's view.
//
// It is built after the write has committed, from a context that outlives the
// request: a client that hangs up the instant it has sent must not take the
// recipient's live frame with it. A frame that cannot be built is logged and
// skipped; the durable copy is the row, and the catch-up feed closes the gap.
func (h *Handler) pushDM(ctx context.Context, op string, m *messageRecord, nonce string) {
	if h.notify == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
	defer cancel()
	for _, viewerID := range []string{m.RecipientID, m.SenderID} {
		msg, err := h.respondOne(ctx, viewerID, m)
		if err != nil {
			logx.Error("messaging: build frame: %v", err)
			continue
		}
		h.push([]string{viewerID}, KindDM, dmFrame{
			Op:      op,
			From:    m.SenderID,
			Message: msg,
			Nonce:   nonce,
		})
	}
}

// typingFrame is the `d` of a typing frame: who is composing.
type typingFrame struct {
	From string `json:"from"`
}

// readFrame is the `d` of a read frame: who read, and how far. `at` and `id` are
// the reader's new marker, the (created_at, id) of the newest message of the
// recipient's they have read, so the person who sent the messages can mark
// everything up to that position as seen.
type readFrame struct {
	From string    `json:"from"`
	At   time.Time `json:"at"`
	ID   string    `json:"id"`
}

// messageResponse is the JSON-safe view of a message. Every field is always
// present: edited_at, deleted_at, reply_to and pinned_at are null when unset and
// reactions and mentions are [] when empty, so a client never has to tell "absent"
// from "null" or from "empty". A deleted message has deleted_at set and an empty
// body.
//
// reply_to, reactions and mentions are not columns of the message. They are read
// when the message is, and the last two are relative to the person asking, so the
// same message is not the same bytes for both people in the conversation.
type messageResponse struct {
	ID          string            `json:"id"`
	Seq         int64             `json:"seq"`
	SenderID    string            `json:"sender_id"`
	RecipientID string            `json:"recipient_id"`
	Body        string            `json:"body"`
	CreatedAt   time.Time         `json:"created_at"`
	EditedAt    *time.Time        `json:"edited_at"`
	DeletedAt   *time.Time        `json:"deleted_at"`
	ReplyTo     *replyPreview     `json:"reply_to"`
	PinnedAt    *time.Time        `json:"pinned_at"`
	Reactions   []reactionSummary `json:"reactions"`
	Mentions    []string          `json:"mentions"`
	// Kind is always present: text, invite or image. Payload is what a non-text kind
	// carries, and is LEFT OUT (not null) for text and for a deleted message, so a
	// client that predates kinds sees the shape it always did plus one field.
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// replyPreview is what a reply shows of the message it answers: resolved every time
// the reply is read, never stored, so an edit of the original shows through. A
// deleted original reports deleted with an empty body.
type replyPreview struct {
	ID       string `json:"id"`
	SenderID string `json:"sender_id"`
	Body     string `json:"body"`
	Deleted  bool   `json:"deleted"`
}

// reactionSummary is one emoji on a message: how many people used it and whether
// the person reading is one of them.
type reactionSummary struct {
	Emoji string `json:"emoji"`
	Count int64  `json:"count"`
	Me    bool   `json:"me"`
}

// toResponse is the part of the shape that lives in the message's own row, with the
// derived parts empty. respond fills those in; nothing that serves a message to a
// client should skip it.
func toResponse(m *messageRecord) messageResponse {
	return messageResponse{
		ID:          m.ID,
		Seq:         m.Seq,
		SenderID:    m.SenderID,
		RecipientID: m.RecipientID,
		Body:        m.Body,
		CreatedAt:   m.CreatedAt,
		EditedAt:    m.EditedAt,
		DeletedAt:   m.DeletedAt,
		PinnedAt:    m.PinnedAt,
		Reactions:   []reactionSummary{},
		Mentions:    []string{},
		Kind:        m.Kind,
		Payload:     m.Payload,
	}
}

// partialMentionTail is a mention cut off at the end of a string: a lone "<", or "<@"
// and some of an id, with no closing ">".
var partialMentionTail = regexp.MustCompile(`<(?:@[A-Za-z0-9]*)?$`)

// toReplyPreview shortens the original to the first replyPreviewChars characters
// (not bytes: a cut through a multi-byte character would be invalid text). When that
// cut lands inside a mention it drops the half token instead of shipping it: "<@01JAB"
// is rendered as literal characters, which is correct for something that is not a
// mention but reads as corruption in a preview. Only a cut is repaired: an original
// that is short enough ends exactly as its author wrote it. A deleted original has no
// words to show.
func toReplyPreview(r replyRecord) replyPreview {
	p := replyPreview{ID: r.OriginalID, SenderID: r.SenderID, Deleted: r.Deleted}
	if !r.Deleted {
		runes := []rune(r.Body)
		if len(runes) > replyPreviewChars {
			p.Body = partialMentionTail.ReplaceAllString(string(runes[:replyPreviewChars]), "")
		} else {
			p.Body = r.Body
		}
	}
	return p
}

// mentionPattern is how a mention is written in a body: the id, never a name. A
// name would break the moment somebody is renamed and would be ambiguous between
// two friends who share one.
var mentionPattern = regexp.MustCompile(`<@([A-Za-z0-9]+)>`)

// mentionCandidates returns the ids written as <@id> in body, each once, in the
// order they first appear. Whether any of them counts is reportedMentions' call.
func mentionCandidates(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mentionPattern.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// reportedMentions keeps the candidates the reader may be told about: the reader
// themself and the reader's accepted friends. Anything else stays in the text as
// plain characters and is simply not reported; a body is text, and failing a send
// over something that merely looks like a mention would be worse than ignoring it.
// At most maxMentions are reported. The result is never nil.
func reportedMentions(candidates []string, readerID string, friends map[string]bool) []string {
	out := []string{}
	for _, id := range candidates {
		if id != readerID && !friends[id] {
			continue
		}
		out = append(out, id)
		if len(out) == maxMentions {
			break
		}
	}
	return out
}

// respond builds the response for each message as viewerID sees it, in order. It
// reads the three derived parts for the whole page at once, one query each and only
// when the page has something to resolve, so a page of a hundred messages costs the
// same handful of statements as a page of one:
//
//   - reply_to, from the originals the page's replies point at;
//   - reactions, tallied per emoji, with `me` meaning viewerID;
//   - mentions, which are derived from the body every time and never stored, so an
//     edit that removes one removes it everywhere. Only the viewer and the viewer's
//     accepted friends are reported.
//
// Every path that returns a message goes through here.
func (h *Handler) respond(ctx context.Context, viewerID string, ms []messageRecord) ([]messageResponse, error) {
	out := make([]messageResponse, len(ms))
	if len(ms) == 0 {
		return out, nil
	}

	index := make(map[string]int, len(ms))
	ids := make([]string, len(ms))
	var replying, allCandidates []string
	candidates := make([][]string, len(ms))
	seen := map[string]bool{}
	for i := range ms {
		out[i] = toResponse(&ms[i])
		index[ms[i].ID] = i
		ids[i] = ms[i].ID
		if ms[i].ReplyToID != nil {
			replying = append(replying, ms[i].ID)
		}
		candidates[i] = mentionCandidates(ms[i].Body)
		for _, c := range candidates[i] {
			if c != viewerID && !seen[c] {
				seen[c] = true
				allCandidates = append(allCandidates, c)
			}
		}
	}

	if len(replying) > 0 {
		replies, err := loadReplies(ctx, h.db, replying)
		if err != nil {
			return nil, err
		}
		for id, r := range replies {
			p := toReplyPreview(r)
			out[index[id]].ReplyTo = &p
		}
	}

	reactions, err := loadReactions(ctx, h.db, ids, viewerID)
	if err != nil {
		return nil, err
	}
	for _, r := range reactions {
		i := index[r.MessageID]
		out[i].Reactions = append(out[i].Reactions, reactionSummary{Emoji: r.Emoji, Count: r.Count, Me: r.Me})
	}

	var friends map[string]bool
	if len(allCandidates) > 0 {
		if friends, err = loadMentionable(ctx, h.db, viewerID, allCandidates); err != nil {
			return nil, err
		}
	}
	for i := range ms {
		if len(candidates[i]) > 0 {
			out[i].Mentions = reportedMentions(candidates[i], viewerID, friends)
		}
	}
	return out, nil
}

// respondOne is respond for a single message.
func (h *Handler) respondOne(ctx context.Context, viewerID string, m *messageRecord) (messageResponse, error) {
	out, err := h.respond(ctx, viewerID, []messageRecord{*m})
	if err != nil {
		return messageResponse{}, err
	}
	return out[0], nil
}

type sendMessageRequest struct {
	RecipientID string `json:"recipient_id"`
	Body        string `json:"body"`
	Nonce       string `json:"nonce"`
	ReplyToID   string `json:"reply_to_id"`
	// Kind and Payload are READ only to be refused. A client cannot declare a kind
	// (see kinds.go): they are here so that naming one is an error rather than an
	// unknown field the decoder drops, which would turn `kind: "invite"` into a
	// plain text message nobody was told about. Nothing below copies either onto the
	// message that is stored.
	Kind    *string         `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// clientDeclaredAKind is whether the request tries to say anything about kind or
// payload other than what every send already is: text with no payload. Saying
// "text" out loud is allowed, and so is a payload of JSON null (no payload).
func (req *sendMessageRequest) clientDeclaredAKind() bool {
	if req.Kind != nil && *req.Kind != kindText {
		return true
	}
	if len(req.Payload) != 0 && !bytes.Equal(bytes.TrimSpace(req.Payload), []byte("null")) {
		return true
	}
	return false
}

// sendResponse is the message plus the nonce the sender supplied, echoed so their
// client can swap the row it drew optimistically for this one. The nonce is also
// the sender's idempotency key: see Send.
type sendResponse struct {
	messageResponse
	Nonce string `json:"nonce,omitempty"`
}

type editMessageRequest struct {
	Body string `json:"body"`
}

// plainText is raw with the bytes Postgres refuses in a text column or parameter
// taken out: invalid UTF-8 and NUL. Every string that a person supplies and that
// reaches a statement goes through it first, because a request Postgres rejects is a
// 500, and no text anyone can type may be a 500. It must run BEFORE a trim or a
// length count, so that text made of nothing but those bytes is empty and is judged
// empty, rather than being stored as a blank or reaching the database.
func plainText(raw string) string {
	return strings.Map(func(r rune) rune {
		if r == 0 {
			return -1
		}
		return r
	}, strings.ToValidUTF8(raw, ""))
}

// urlParam and queryParam read what the URL carries as plainText, at the point it is
// read and not inside each query. A NUL or invalid byte in an id or a cursor would
// otherwise reach Postgres and come back a 500, where an id that names nothing is
// the opaque 404 or bad_cursor. A parameter of nothing but those bytes is therefore
// exactly as empty as a missing one, and one with a NUL inside a real value is that
// value without it; either way it is judged by the same rules as the clean spelling,
// including the checks that compare ids (a NUL cannot be used to dodge `== authedID`).
func urlParam(r *http.Request, name string) string { return plainText(chi.URLParam(r, name)) }

func queryParam(r *http.Request, name string) string { return plainText(r.URL.Query().Get(name)) }

// cleanBody applies the one rule for what a message may say, shared by send and
// edit so the two can't drift: plainText, trimmed, not empty, at most maxBody
// characters. It returns the trimmed body, or the error code to answer with.
func cleanBody(raw string) (body, code string) {
	body = strings.TrimSpace(plainText(raw))
	if body == "" {
		return "", errEmptyBody
	}
	if len([]rune(body)) > maxBody {
		return "", errBodyTooLong
	}
	return body, ""
}

// Send persists a DM from the authed user to a recipient. The recipient must be
// an accepted friend; otherwise the request collapses to an opaque 404 so the
// sender can't distinguish "not a friend" from "no such user".
//
// A nonce makes the send idempotent on (sender, recipient, nonce). The first send
// answers 201 and pushes the message; a repeat — a retry after a response was
// lost — answers 200 with the message already stored and pushes nothing, because
// the recipient already has it and a second frame would show it twice. The
// recipient is part of the key so the same nonce sent to someone else is a new
// message that gets delivered. No nonce, no idempotency: two identical sends are
// two messages.
//
// reply_to_id, when given, must name a message of THIS conversation, in either
// direction and deleted or not; anything else is a 400 bad_reply, whether it is
// another thread's message or none at all. The check is a security boundary and
// not politeness: the reply carries a preview of the original, so without it a
// reply would read one line out of a conversation the sender is not in. It runs
// after the friendship gate, so a stranger learns nothing from it.
func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, sendBodyLimit)

	senderID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// THE RULE: a client cannot declare a kind. This is checked before anything else
	// about the message, and the answer names no reason beyond "invalid body".
	if req.clientDeclaredAKind() {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}

	recipientID := strings.TrimSpace(plainText(req.RecipientID))
	body, code := cleanBody(req.Body)
	if code != "" {
		httpx.WriteError(w, http.StatusBadRequest, code)
		return
	}
	nonce := strings.TrimSpace(plainText(req.Nonce))
	if len([]rune(nonce)) > maxNonce {
		httpx.WriteError(w, http.StatusBadRequest, errNonceTooLong)
		return
	}
	if recipientID == "" || recipientID == senderID {
		// Self-DM and missing target both collapse to the opaque recipient code.
		httpx.WriteError(w, http.StatusNotFound, errRecipient)
		return
	}

	if code := h.guardRecipient(r.Context(), senderID, recipientID); code != "" {
		httpx.WriteError(w, http.StatusNotFound, code)
		return
	}

	var replyTo *string
	if id := strings.TrimSpace(plainText(req.ReplyToID)); id != "" {
		inPair, err := replyTargetInPair(r.Context(), h.db, senderID, recipientID, id)
		if err != nil {
			logx.Error("messaging: reply target: %v", err)
			httpx.WriteError(w, http.StatusInternalServerError, "could not send message")
			return
		}
		if !inPair {
			httpx.WriteError(w, http.StatusBadRequest, errBadReply)
			return
		}
		replyTo = &id
	}

	var created bool
	saved, err := writeSeq(r.Context(), h.begin, func(tx dbExecutor) (*messageRecord, error) {
		m, wasCreated, err := insertOnce(r.Context(), tx, &messageRecord{
			ID:          ulid.Make().String(),
			SenderID:    senderID,
			RecipientID: recipientID,
			Body:        body,
			Nonce:       nonce,
			ReplyToID:   replyTo,
			Kind:        kindText, // never req.Kind: see sendMessageRequest
		})
		created = wasCreated
		return m, err
	})
	if err != nil {
		logx.Error("messaging: insert: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not send message")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		// Stored, so it's safe to say so. After the write and not before: a frame
		// for a message that failed to save would show the recipient something that
		// isn't there.
		h.pushDM(r.Context(), opNew, saved, nonce)
	}
	msg, err := h.respondOne(r.Context(), senderID, saved)
	if err != nil {
		logx.Error("messaging: respond: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not send message")
		return
	}
	httpx.WriteJSON(w, status, sendResponse{messageResponse: msg, Nonce: nonce})
}

// Edit rewrites the body of a message the authed user sent. An edit ADDS words to
// a thread, so it needs the friendship that sending needed: after an unfriend or a
// block the sender can no longer put new text in front of the other person, and no
// dm frame is pushed to them.
//
// Every refusal — someone else's message, a message that does not exist, one
// already deleted, and a recipient who is no longer a friend — is the same opaque
// 404 message_not_found, so a message id can't be probed for. The order is what
// keeps it so: the message is looked up scoped to the caller as its sender, so a
// non-sender is turned away on the lookup alone and never reaches the friendship
// check; only the sender can get as far as learning that the recipient is no longer
// a friend, and they already know both. There is no edit window. The body is all an
// edit can change: reply_to_id is not read from the request and no UPDATE sets it,
// so what a message answers is fixed when it is sent.
func (h *Handler) Edit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, sendBodyLimit)

	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req editMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	body, code := cleanBody(req.Body)
	if code != "" {
		httpx.WriteError(w, http.StatusBadRequest, code)
		return
	}

	id := strings.TrimSpace(urlParam(r, "messageID"))
	recipientID, err := ownMessageRecipient(r.Context(), h.db, id, authedID)
	if errors.Is(err, errNoMessage) {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}
	if err != nil {
		logx.Error("messaging: edit: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not edit message")
		return
	}
	if h.guardRecipient(r.Context(), authedID, recipientID) != "" {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}

	saved, err := writeSeq(r.Context(), h.begin, func(tx dbExecutor) (*messageRecord, error) {
		return editMessage(r.Context(), tx, id, authedID, body)
	})
	if errors.Is(err, errNoMessage) {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}
	if err != nil {
		logx.Error("messaging: edit: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not edit message")
		return
	}
	h.pushDM(r.Context(), opEdit, saved, "")
	msg, err := h.respondOne(r.Context(), authedID, saved)
	if err != nil {
		logx.Error("messaging: edit respond: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not edit message")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, msg)
}

// Delete turns a message the authed user sent into a tombstone: the body is
// cleared, deleted_at is set, and the row stays so a client that cached the
// message learns it is gone. Same opaque 404 as Edit for anything that is not a
// live message of the caller's.
//
// Delete is NOT friend-gated, and must not be: it is the one write on a thread
// that only takes words away. An edit adds words, so it needs the friendship that
// sending needed; a delete removes the sender's own words, and its dm frame carries
// no body at all (a delete clears it). Someone who has been unfriended or blocked
// keeps the right to take their own words back, and refusing it would protect
// nobody, because the result is strictly less text in front of the other person.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	id := strings.TrimSpace(urlParam(r, "messageID"))
	saved, err := writeSeq(r.Context(), h.begin, func(tx dbExecutor) (*messageRecord, error) {
		return deleteMessage(r.Context(), tx, id, authedID)
	})
	if errors.Is(err, errNoMessage) {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}
	if err != nil {
		logx.Error("messaging: delete: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not delete message")
		return
	}
	h.pushDM(r.Context(), opDelete, saved, "")
	w.WriteHeader(http.StatusNoContent)
}

// Updates is the catch-up feed: every message the authed user sent or received
// with someone they are still accepted friends with that changed after ?since=,
// ordered by seq ascending, tombstones included —
// that is how a client that was away learns something was deleted. seq moves on
// every edit and delete, so this is the ONLY thing seq orders; a conversation is
// ordered by (created_at, id). `cursor` is the highest seq in the page (the
// requested since when the page is empty), and `has_more` is true when the page
// filled to the limit, so a client that was offline for a week pages forward
// until it is false. The feed is friends-only like history and the conversation
// list: a thread with someone who is no longer a friend is not in it.
func (h *Handler) Updates(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	since := parseSince(queryParam(r, "since"))
	limit := clampLimit(queryParam(r, "limit"), defaultUpdatesLimit, maxUpdatesLimit)
	rows, err := listUpdates(r.Context(), h.db, authedID, since, limit)
	if err != nil {
		logx.Error("messaging: updates: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
		return
	}

	out, err := h.respond(r.Context(), authedID, rows)
	if err != nil {
		logx.Error("messaging: updates respond: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
		return
	}
	cursor := since
	for i := range rows {
		cursor = rows[i].Seq
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"messages": out,
		"cursor":   cursor,
		"has_more": len(rows) == limit,
	})
}

// History returns one page of the conversation between the authed user and
// {userID}, ordered oldest→newest. With no ?before= it is the newest page; with
// one it is the page strictly older than that message id, keyset on
// (created_at, id) so it never skips or repeats a message as new ones arrive.
// ?limit= defaults to 100, max 200, and `has_more` is true when the page filled
// to the limit, so a client scrolling back stops when it comes back false.
// Tombstones are left out. The other party must be an accepted friend; otherwise
// opaque 404. A ?before= that names no message of this conversation is a 400
// bad_cursor. An empty conversation is a 200 with an empty list.
//
// ?around=<message id> instead asks for the page CENTRED on that message, which is how
// a client opens a search hit, a pin or a reply's original that is older than what it
// has loaded, in one request instead of paging back until it turns up. It cannot be
// combined with ?before=: both together is a 400 conflicting_cursors, not one quietly
// winning. See historyAround for the page and its different response shape.
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	otherID := urlParam(r, "userID")
	if otherID == "" || otherID == authedID {
		httpx.WriteError(w, http.StatusNotFound, errRecipient)
		return
	}

	if code := h.guardRecipient(r.Context(), authedID, otherID); code != "" {
		httpx.WriteError(w, http.StatusNotFound, code)
		return
	}

	limit := parseLimit(queryParam(r, "limit"))
	if around := strings.TrimSpace(queryParam(r, "around")); around != "" {
		if strings.TrimSpace(queryParam(r, "before")) != "" {
			httpx.WriteError(w, http.StatusBadRequest, errConflictingCursors)
			return
		}
		h.historyAround(w, r, authedID, otherID, around, limit)
		return
	}
	var before *cursor
	if id := strings.TrimSpace(queryParam(r, "before")); id != "" {
		c, err := historyCursor(r.Context(), h.db, authedID, otherID, id)
		if errors.Is(err, errNoCursor) {
			httpx.WriteError(w, http.StatusBadRequest, errBadCursor)
			return
		}
		if err != nil {
			logx.Error("messaging: history cursor: %v", err)
			httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
			return
		}
		before = c
	}

	rows, err := listConversation(r.Context(), h.db, authedID, otherID, limit, before)
	if err != nil {
		logx.Error("messaging: history: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
		return
	}

	responses, err := h.respond(r.Context(), authedID, rows)
	if err != nil {
		logx.Error("messaging: history respond: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
		return
	}
	// listConversation returns newest-first; reverse to ascending for rendering.
	out := make([]messageResponse, len(responses))
	for i, m := range responses {
		out[len(responses)-1-i] = m
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"messages": out,
		"has_more": len(rows) == limit,
	})
}

// historyAround answers History's ?around=: one page of the conversation centred on
// the message `around`, oldest→newest, in the shape {messages, has_more_older,
// has_more_newer}. It is a different shape from {messages, has_more} on purpose — a
// page that can be extended in two directions cannot say so with one flag — and the
// shape of the page-back responses is untouched.
//
// The page is at most `limit` messages: the older half, which includes the anchor
// when it is live, and the newer half, with an odd limit giving the extra place to
// the older side so that limit=1 is the anchor alone. The anchor is found with
// historyCursor like ?before=, so an id that names no message of this conversation
// is a 400 bad_cursor, and a tombstone still positions the page: a deleted message is
// still a place in the conversation, even though it is never returned. Keyset on
// (created_at, id), never seq. The friendship gate has already run.
func (h *Handler) historyAround(w http.ResponseWriter, r *http.Request, authedID, otherID, aroundID string, limit int) {
	anchor, err := historyCursor(r.Context(), h.db, authedID, otherID, aroundID)
	if errors.Is(err, errNoCursor) {
		httpx.WriteError(w, http.StatusBadRequest, errBadCursor)
		return
	}
	if err != nil {
		logx.Error("messaging: around cursor: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
		return
	}

	olderN := (limit + 1) / 2
	newerN := limit - olderN
	older, newer, err := listAround(r.Context(), h.db, authedID, otherID, *anchor, olderN, newerN)
	if err != nil {
		logx.Error("messaging: around: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
		return
	}

	// older arrives newest-first and is trimmed to its place count; the row past it
	// was only fetched to learn whether there is more. Ascending is older reversed
	// then newer.
	hasOlder, hasNewer := len(older) > olderN, len(newer) > newerN
	if hasOlder {
		older = older[:olderN]
	}
	if hasNewer {
		newer = newer[:newerN]
	}
	rows := make([]messageRecord, 0, len(older)+len(newer))
	for i := len(older) - 1; i >= 0; i-- {
		rows = append(rows, older[i])
	}
	rows = append(rows, newer...)

	out, err := h.respond(r.Context(), authedID, rows)
	if err != nil {
		logx.Error("messaging: around respond: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load messages")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"messages":       out,
		"has_more_older": hasOlder,
		"has_more_newer": hasNewer,
	})
}

// conversationResponse is one line of the conversation list. Each read marker is
// a position, so it is two fields, `*_read_at` and `*_read_id`; all four are
// present either way and null until that person has read anything. A marker from
// before positions existed has its time and a null id, and means everything up to
// that time is read.
type conversationResponse struct {
	UserID      string          `json:"user_id"`
	LastMessage messageResponse `json:"last_message"`
	Unread      int64           `json:"unread"`
	MyReadAt    *time.Time      `json:"my_read_at"`
	MyReadID    *string         `json:"my_read_id"`
	PeerReadAt  *time.Time      `json:"peer_read_at"`
	PeerReadID  *string         `json:"peer_read_id"`
}

// Conversations is the conversation list: one entry per accepted friend the
// authed user has any live message with, in either direction, ordered by the last
// message's created_at descending. `last_message` is the newest message that has
// not been deleted, so a pair whose every message was deleted is absent. With
// ?before=<last_message.id of the previous page's final entry> it is the next
// page, keyset on that message's (created_at, id); ?limit= defaults to 50, max
// 200, and `has_more` is true when the page filled to the limit. A ?before= that
// names no message of the caller's is a 400 bad_cursor.
func (h *Handler) Conversations(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	limit := clampLimit(queryParam(r, "limit"), defaultConversationsLimit, maxConversationsLimit)
	var before *cursor
	if id := strings.TrimSpace(queryParam(r, "before")); id != "" {
		c, err := inboxCursor(r.Context(), h.db, authedID, id)
		if errors.Is(err, errNoCursor) {
			httpx.WriteError(w, http.StatusBadRequest, errBadCursor)
			return
		}
		if err != nil {
			logx.Error("messaging: conversations cursor: %v", err)
			httpx.WriteError(w, http.StatusInternalServerError, "could not load conversations")
			return
		}
		before = c
	}

	rows, err := listSummaries(r.Context(), h.db, authedID, limit, before)
	if err != nil {
		logx.Error("messaging: conversations: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load conversations")
		return
	}

	lasts := make([]messageRecord, len(rows))
	for i := range rows {
		lasts[i] = rows[i].Last
	}
	lastResponses, err := h.respond(r.Context(), authedID, lasts)
	if err != nil {
		logx.Error("messaging: conversations respond: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load conversations")
		return
	}

	out := make([]conversationResponse, len(rows))
	for i := range rows {
		out[i] = conversationResponse{
			UserID:      rows[i].PeerID,
			LastMessage: lastResponses[i],
			Unread:      rows[i].Unread,
			MyReadAt:    rows[i].MyReadAt,
			MyReadID:    rows[i].MyReadID,
			PeerReadAt:  rows[i].PeerReadAt,
			PeerReadID:  rows[i].PeerReadID,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"conversations": out,
		"has_more":      len(rows) == limit,
	})
}

// Pin pins a message of the conversation the authed user is in, and Unpin takes the
// pin off. Either person may pin or unpin either person's message: a pin is a
// property of the conversation, not of the message's author, which is why this is
// NOT sender-only the way Edit and Delete are. Both answer 200 with the message.
//
// Unpin, unlike RemoveReaction, is friend-gated, and that is deliberate. A reaction is
// its owner's own mark and only they can remove it, so gating the removal would strand
// it. A pin belongs to the conversation and either person can clear it, so a pinner who
// has been unfriended or blocked leaves nothing stuck: the other person can always
// unpin.
//
// The refusals are the opaque message_not_found: a message that is missing, deleted
// or between two other people (so a pin can't probe for an id), and a conversation
// whose other person is no longer a friend (friend-gated like reading it). A
// message that is already in the state asked for is a success that writes nothing:
// no seq is drawn and no frame is pushed, because a repeat is not news. A
// conversation holds at most maxPins; past that, 400 too_many_pins. A pin or unpin
// that does change something draws a fresh seq, which is how it reaches a client
// that was offline, and pushes an op:"pin" frame to both people.
func (h *Handler) Pin(w http.ResponseWriter, r *http.Request)   { h.setPin(w, r, true) }
func (h *Handler) Unpin(w http.ResponseWriter, r *http.Request) { h.setPin(w, r, false) }

func (h *Handler) setPin(w http.ResponseWriter, r *http.Request, pin bool) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	id := strings.TrimSpace(urlParam(r, "messageID"))
	if !h.guardParticipant(w, r, id, authedID, true, "could not change pin") {
		return
	}

	var changed bool
	saved, err := writeSeq(r.Context(), h.begin, func(tx dbExecutor) (*messageRecord, error) {
		var (
			m   *messageRecord
			err error
		)
		if pin {
			m, changed, err = pinMessage(r.Context(), tx, id, authedID)
		} else {
			m, changed, err = unpinMessage(r.Context(), tx, id, authedID)
		}
		return m, err
	})
	if errors.Is(err, errNoMessage) {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}
	if errors.Is(err, errPinLimit) {
		httpx.WriteError(w, http.StatusBadRequest, errTooManyPins)
		return
	}
	if err != nil {
		logx.Error("messaging: pin: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not change pin")
		return
	}
	if changed {
		h.pushDM(r.Context(), opPin, saved, "")
	}
	msg, err := h.respondOne(r.Context(), authedID, saved)
	if err != nil {
		logx.Error("messaging: pin respond: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not change pin")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, msg)
}

// Pins lists the pinned messages of the conversation between the authed user and
// {userID}, newest pinned first (at most maxPins). Friend-gated like history, with
// the same opaque 404.
func (h *Handler) Pins(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	otherID := urlParam(r, "userID")
	if otherID == "" || otherID == authedID {
		httpx.WriteError(w, http.StatusNotFound, errRecipient)
		return
	}
	if code := h.guardRecipient(r.Context(), authedID, otherID); code != "" {
		httpx.WriteError(w, http.StatusNotFound, code)
		return
	}

	rows, err := listPins(r.Context(), h.db, authedID, otherID)
	if err != nil {
		logx.Error("messaging: pins: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load pins")
		return
	}
	out, err := h.respond(r.Context(), authedID, rows)
	if err != nil {
		logx.Error("messaging: pins respond: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load pins")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"messages": out})
}

type reactRequest struct {
	Emoji string `json:"emoji"`
}

// Runes cleanEmoji accepts besides symbols. They are what the sequences that make
// up one emoji are glued together with. Written as numbers because they draw
// nothing, and a literal in the source could not be seen.
const (
	runeZWJ      rune = 0x200D // joins the parts of a family, a couple, a profession
	runeVS15     rune = 0xFE0E // text presentation selector
	runeVS16     rune = 0xFE0F // emoji presentation selector
	runeKeycap   rune = 0x20E3 // combining enclosing keycap
	runeTagFirst rune = 0xE0020
	runeTagLast  rune = 0xE007F // tag characters, the body and terminator of a subdivision flag
)

// Named ranges the rule below needs: the skin-tone modifiers, which are Sk and so not
// reachable by category, and where text ends. Everything below latinEnd is ASCII and
// Latin-1, which is text and typography; the only emoji down there are © and ®.
const (
	skinToneFirst rune = 0x1F3FB
	skinToneLast  rune = 0x1F3FF
	latinEnd      rune = 0x100
)

// emojiOutsideSymbols are real emoji that the symbol class below does not reach: code
// points Unicode files under a category other than So (ℹ is a letter, ‼ ⁉ 〽 〰 are
// punctuation, ↔ ⤴ ⤵ are math symbols), and © and ®, which are So but sit in Latin-1,
// below latinEnd. cleanEmoji approximates Unicode's emoji property with Go's general
// categories, because Go ships no emoji data and a dependency for it is not worth
// carrying. A small number of real emoji sit outside those categories; these ten are
// the ones found so far. If another turns up, the fix is to add it HERE as one more
// entry, not to rewrite the rule and not to widen it to a whole category: allowing Sm,
// for one, would also admit =, + and |, which are not emoji.
var emojiOutsideSymbols = map[rune]string{
	0x00A9: "copyright sign (So, below latinEnd)",
	0x00AE: "registered sign (So, below latinEnd)",
	0x2139: "information source, a letter (Ll)",
	0x203C: "double exclamation mark (Po)",
	0x2049: "exclamation question mark (Po)",
	0x303D: "part alternation mark (Po)",
	0x3030: "wavy dash (Pd)",
	0x2194: "left right arrow (Sm)",
	0x2934: "arrow pointing rightwards then curving upwards (Sm)",
	0x2935: "arrow pointing rightwards then curving downwards (Sm)",
}

// isEmojiSymbol is the symbol class of cleanEmoji's character rule: a "Symbol, other"
// above Latin-1, and not U+FFFD, which is filed as one and is what damaged text
// decodes to. Sk is not a class here: it would admit the ASCII and spacing accents
// (^ ` ¨ ¯ ´ ¸), and the only thing it was ever wanted for is the skin tones, which
// are named by range. The Latin-1 floor keeps ° and ¦, which are So and are
// typography. Sm, Sc, punctuation and the rest are not emoji and are not here.
func isEmojiSymbol(r rune) bool {
	return r >= latinEnd && r != utf8.RuneError && unicode.Is(unicode.So, r)
}

// isKeycapBase reports whether r, the rune at the start of runes, begins a keycap
// sequence: 0-9, # or * followed by the keycap, with or without the emoji selector
// between them.
func isKeycapBase(runes []rune) bool {
	r := runes[0]
	if !(r >= '0' && r <= '9') && r != '#' && r != '*' {
		return false
	}
	rest := runes[1:]
	if len(rest) > 0 && rest[0] == runeVS16 {
		rest = rest[1:]
	}
	return len(rest) > 0 && rest[0] == runeKeycap
}

// cleanEmoji is the one rule for what a reaction may be, shared by adding and
// removing so the two can't drift. It returns the trimmed emoji, or false. A reaction
// is exactly one grapheme cluster, made only of the runes an emoji is made of.
//
//  1. THE CLUSTER CLAUSE keeps WORDS out. The input must be exactly one
//     user-perceived character, as github.com/rivo/uniseg segments it (Unicode's
//     extended grapheme clusters, which is also what makes a flag, a skin tone, a ZWJ
//     family, a subdivision flag and a keycap each one). Nothing is, or is not, a
//     reaction because of what script it is in: "ⓗⓘ", a braille word, two flags and a
//     row of ideographs are all several clusters. uniseg was already in the module
//     graph (lipgloss pulls it in), so using it cost no new code in the build.
//  2. THE CHARACTER CLAUSE keeps TEXT out. Every rune of that cluster must be one of
//     - a symbol: isEmojiSymbol, one of the five skin-tone modifiers, or one of the
//     code points named in emojiOutsideSymbols;
//     - a zero-width joiner, a variation selector, a combining keycap, or a tag
//     character: the glue of families, flags and keycaps;
//     - 0-9, # or * as the FIRST rune of a keycap sequence and nowhere else.
//     Letters and numbers of any script, spaces, punctuation, marks, control
//     characters, math and currency symbols, ASCII and the Latin-1 accents, and U+FFFD
//     are none of those, so a lone letter, a lone "^" and a lone combining mark are
//     refused here even though each is one cluster.
//
// A cluster of only glue draws nothing and is refused too. The length is capped at
// maxEmojiRunes runes and maxEmojiBytes bytes, because a ZWJ chain can be one cluster
// of any length.
//
// WHAT IT ACCEPTS ON PURPOSE: a single unusual symbol, such as one enclosed ideograph
// (㊗), one braille cell, or one circled letter. That is not a hole and not an
// oversight, and it is not a reason to start a list of ranges again: the rule exists to
// stop the field becoming a second message body, and one character is not a body.
//
// It is still not an emoji test, which Go (and uniseg) cannot do without emoji data.
func cleanEmoji(raw string) (string, bool) {
	emoji := strings.TrimSpace(raw)
	if emoji == "" || !utf8.ValidString(emoji) || len(emoji) > maxEmojiBytes {
		return "", false
	}
	runes := []rune(emoji)
	if len(runes) > maxEmojiRunes || uniseg.GraphemeClusterCount(emoji) != 1 {
		return "", false
	}
	visible := false
	for i, r := range runes {
		switch {
		case isEmojiSymbol(r),
			r >= skinToneFirst && r <= skinToneLast,
			emojiOutsideSymbols[r] != "":
			visible = true
		case r == runeZWJ, r == runeVS15, r == runeVS16, r == runeKeycap,
			r >= runeTagFirst && r <= runeTagLast:
			// glue: allowed, draws nothing by itself
		case isKeycapBase(runes[i:]):
			visible = true
		default:
			return "", false
		}
	}
	if !visible {
		return "", false
	}
	return emoji, true
}

// AddReaction puts the authed user's reaction on a message of a conversation they
// are in, and RemoveReaction takes it off. Either person may react to either
// person's message — a reaction, like a pin, belongs to the conversation, so this is
// NOT sender-only the way Edit and Delete are. Both answer 200 with the message as
// the caller now sees it. AddReaction takes `{"emoji": "👍"}`; RemoveReaction takes
// the emoji percent-encoded in the path. An emoji that fails cleanEmoji is a 400
// bad_emoji.
//
// The refusals are the opaque message_not_found, as for a pin: a message that is
// missing, deleted or between two other people and, for AddReaction only, a
// conversation whose other person is no longer a friend. AddReaction is friend-gated
// because it puts something in front of the other person, like an edit adding words.
// RemoveReaction is deliberately NOT, for the reason Delete is not: the reaction is
// the caller's own mark, and being unfriended or blocked must not leave it stuck on
// somebody's message forever. It still requires the caller to be a party to the
// message, so a stranger can't probe ids. Its frame still goes to BOTH people, a
// former friend included, exactly as Delete's does, and on purpose: it carries no
// new words from them, only the withdrawal of their own mark on a message that is
// already the other person's, and without the push that person could never learn of
// it, because a thread with a former friend is not in their catch-up feed. A message
// holds at most maxReactionEmoji distinct
// emoji and maxReactions in all; past either, 400 too_many_reactions. Adding what
// the caller already added, or removing what they never added, is a success that
// writes nothing: no seq is drawn and no frame is pushed, because a repeat is not
// news — which is what makes a double-tap and a retry safe. A reaction that does
// change something draws a fresh seq on the message, which is how it reaches a
// client that was offline through the ordinary catch-up feed, and pushes an
// op:"react" frame to both people, each frame with that reader's own `me`.
func (h *Handler) AddReaction(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, sendBodyLimit)

	var req reactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	h.setReaction(w, r, req.Emoji, true)
}

func (h *Handler) RemoveReaction(w http.ResponseWriter, r *http.Request) {
	// chi hands back the segment as the router saw it: still percent-encoded when
	// the URL carried an escape it keeps raw, already decoded otherwise. An emoji
	// has no '%' in it, so decoding a decoded one changes nothing, and an escape that
	// does not decode is as bad an emoji as any other.
	emoji, err := url.PathUnescape(chi.URLParam(r, "emoji"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errBadEmoji)
		return
	}
	h.setReaction(w, r, emoji, false)
}

func (h *Handler) setReaction(w http.ResponseWriter, r *http.Request, rawEmoji string, add bool) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	emoji, valid := cleanEmoji(rawEmoji)
	if !valid {
		httpx.WriteError(w, http.StatusBadRequest, errBadEmoji)
		return
	}

	id := strings.TrimSpace(urlParam(r, "messageID"))
	if !h.guardParticipant(w, r, id, authedID, add, "could not change reaction") {
		return
	}

	var changed bool
	saved, err := writeSeq(r.Context(), h.begin, func(tx dbExecutor) (*messageRecord, error) {
		var (
			m   *messageRecord
			err error
		)
		if add {
			m, changed, err = addReaction(r.Context(), tx, id, authedID, emoji)
		} else {
			m, changed, err = removeReaction(r.Context(), tx, id, authedID, emoji)
		}
		return m, err
	})
	if errors.Is(err, errNoMessage) {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return
	}
	if errors.Is(err, errReactionLimit) {
		httpx.WriteError(w, http.StatusBadRequest, errTooManyReactions)
		return
	}
	if err != nil {
		logx.Error("messaging: reaction: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not change reaction")
		return
	}
	if changed {
		h.pushDM(r.Context(), opReact, saved, "")
	}
	msg, err := h.respondOne(r.Context(), authedID, saved)
	if err != nil {
		logx.Error("messaging: reaction respond: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not change reaction")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, msg)
}

// cleanQuery applies the one rule for what a search may be: plainText, trimmed, not
// empty, at most maxQuery characters. A person's search box must never be a 500.
// It returns the query, or the error code to answer with.
func cleanQuery(raw string) (q, code string) {
	q = strings.TrimSpace(plainText(raw))
	if q == "" {
		return "", errEmptyQuery
	}
	if utf8.RuneCountInString(q) > maxQuery {
		return "", errQueryTooLong
	}
	return q, ""
}

// Search finds messages whose words match ?q=, among the conversations the authed
// user has with people they are still accepted friends with — the same set the
// catch-up feed and the conversation list read through, so a thread history refuses
// to serve is not one search can open. ?with=<userID> narrows to one conversation;
// a `with` who is not an accepted friend is the opaque 404 recipient_not_found, as for
// history.
//
// The query is what a person types, handed to Postgres as websearch_to_tsquery:
// quoted phrases and -exclusions work, and punctuation is harmless. It is 1 to
// maxQuery characters; empty is 400 empty_query and longer is 400 query_too_long.
// A deleted message never matches.
//
// Results are NEWEST FIRST by (created_at, id), not by relevance: chat search is
// "when did we talk about this", and a relevance order makes a conversation
// unreadable. ?before=<message id> pages strictly older than that message on the same
// (created_at, id), never on seq, which an edit or a reaction moves. A `before` that
// names no message of the caller's (of the pair, with `with`) is 400 bad_cursor.
// ?limit= defaults to 50, max 100, and `has_more` is true when the page filled to the
// limit. Each result is a full message, with its reply, reactions and mentions.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	query, code := cleanQuery(r.URL.Query().Get("q"))
	if code != "" {
		httpx.WriteError(w, http.StatusBadRequest, code)
		return
	}
	limit := clampLimit(queryParam(r, "limit"), defaultSearchLimit, maxSearchLimit)

	with := strings.TrimSpace(queryParam(r, "with"))
	if with != "" {
		if with == authedID {
			httpx.WriteError(w, http.StatusNotFound, errRecipient)
			return
		}
		if code := h.guardRecipient(r.Context(), authedID, with); code != "" {
			httpx.WriteError(w, http.StatusNotFound, code)
			return
		}
	}

	var before *cursor
	if id := strings.TrimSpace(queryParam(r, "before")); id != "" {
		var (
			c   *cursor
			err error
		)
		if with != "" {
			c, err = historyCursor(r.Context(), h.db, authedID, with, id)
		} else {
			c, err = inboxCursor(r.Context(), h.db, authedID, id)
		}
		if errors.Is(err, errNoCursor) {
			httpx.WriteError(w, http.StatusBadRequest, errBadCursor)
			return
		}
		if err != nil {
			logx.Error("messaging: search cursor: %v", err)
			httpx.WriteError(w, http.StatusInternalServerError, "could not search messages")
			return
		}
		before = c
	}

	rows, err := searchMessages(r.Context(), h.db, authedID, query, with, limit, before)
	if err != nil {
		logx.Error("messaging: search: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not search messages")
		return
	}
	out, err := h.respond(r.Context(), authedID, rows)
	if err != nil {
		logx.Error("messaging: search respond: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not search messages")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"messages": out,
		"has_more": len(rows) == limit,
	})
}

// unreadConversation is one peer's entry in the unread summary.
type unreadConversation struct {
	UserID string `json:"user_id"`
	Count  int64  `json:"count"`
}

// Unread returns the authed user's unread DM summary in one request: a per-peer
// count of messages received but not yet read, plus the grand total. Read state
// is server-authoritative (a message_reads position per peer), so this reflects
// reads made on any device. Nothing unread → empty conversations list, total 0.
func (h *Handler) Unread(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	entries, err := unreadCounts(r.Context(), h.db, authedID)
	if err != nil {
		logx.Error("messaging: unread: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not load unread counts")
		return
	}

	// make (not nil) so an empty summary serializes as [] rather than null.
	conversations := make([]unreadConversation, len(entries))
	var total int64
	for i, e := range entries {
		conversations[i] = unreadConversation{UserID: e.PeerID, Count: e.Count}
		total += e.Count
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"conversations": conversations,
		"total":         total,
	})
}

// MarkRead marks the authed user's conversation with {userID} read up to the
// newest message {userID} has sent them that is not deleted, advancing the
// server-authoritative read marker so the unread count clears on every device. The
// marker is that message's (created_at, id), read out of the database by the
// statement itself: the request carries no time and no id, and no clock is
// consulted. A missing or self-target path param is a client mistake (there's no
// such conversation to mark) → 400. Success is 204, in every case below too.
//
// The marker itself is a personal bookmark and is not friend-gated. Telling the
// peer is: when the marker moved AND the two are accepted friends, the peer gets
// a read frame carrying exactly the position that was stored, so whoever sent the
// messages learns they were seen. A call that moved nothing tells nobody — there
// was nothing from the peer to read, or it was all read already — and neither does
// one aimed at somebody who is not a friend, otherwise this route would be a way
// to push a frame to any user id.
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	peerID := strings.TrimSpace(urlParam(r, "userID"))
	if peerID == "" || peerID == authedID {
		httpx.WriteError(w, http.StatusBadRequest, "invalid conversation")
		return
	}

	position, advanced, err := markRead(r.Context(), h.db, authedID, peerID)
	if err != nil {
		logx.Error("messaging: mark read: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not mark read")
		return
	}
	if advanced {
		// The marker is saved whatever happens next, so a failure here costs the
		// peer a receipt and the reader nothing: log it and still answer 204.
		friends, err := areFriends(r.Context(), h.db, authedID, peerID)
		if err != nil {
			logx.Error("messaging: are friends: %v", err)
		} else if friends {
			h.push([]string{peerID}, KindRead, readFrame{From: authedID, At: position.At.UTC(), ID: position.ID})
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// Typing tells {userID} that the authed user is composing a message to them. It
// stores nothing and answers 204. The peer must be an accepted friend, with the
// same opaque 404 as sending to one. The server does not throttle it: the client
// sends at most one every few seconds while a person is typing.
func (h *Handler) Typing(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	peerID := strings.TrimSpace(urlParam(r, "userID"))
	if peerID == "" || peerID == authedID {
		httpx.WriteError(w, http.StatusNotFound, errRecipient)
		return
	}
	if code := h.guardRecipient(r.Context(), authedID, peerID); code != "" {
		httpx.WriteError(w, http.StatusNotFound, code)
		return
	}
	h.push([]string{peerID}, KindTyping, typingFrame{From: authedID})
	w.WriteHeader(http.StatusNoContent)
}

// guardRecipient returns "" if the authed user may converse with other, else an
// opaque error code. A DB failure is logged and collapsed to the opaque code so
// the handler never leaks whether a user exists or is a friend.
func (h *Handler) guardRecipient(ctx context.Context, authedID, otherID string) string {
	exists, err := userExists(ctx, h.db, otherID)
	if err != nil {
		logx.Error("messaging: user exists: %v", err)
		return errRecipient
	}
	if !exists {
		return errRecipient
	}
	friends, err := areFriends(ctx, h.db, authedID, otherID)
	if err != nil {
		logx.Error("messaging: are friends: %v", err)
		return errRecipient
	}
	if !friends {
		return errRecipient
	}
	return ""
}

// guardParticipant is the gate in front of every write on an existing message that is
// not the sender's alone (a pin, a reaction): the message must be live and the caller
// a party to it, and — only when needsFriend — the other party must still be an
// accepted friend. Every refusal is the same opaque 404 message_not_found, so a
// message id can't be probed for, and a database failure is a 500 with failMsg. It
// writes that response itself and reports whether the caller may go on.
//
// needsFriend is the one place the rule differs between acts. Putting something in
// front of the other person — a pin, a reaction — needs the friendship that sending
// needed. TAKING AWAY one's own mark does not, for the reason Delete is not
// friend-gated: a reaction is the caller's own, and being unfriended or blocked must
// not leave it stuck on somebody's message forever. The party check is never skipped,
// so a non-participant is turned away the same way either way.
func (h *Handler) guardParticipant(w http.ResponseWriter, r *http.Request, id, authedID string, needsFriend bool, failMsg string) bool {
	cur, err := participantMessage(r.Context(), h.db, id, authedID)
	if errors.Is(err, errNoMessage) {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return false
	}
	if err != nil {
		logx.Error("messaging: message lookup: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, failMsg)
		return false
	}
	if !needsFriend {
		return true
	}
	peerID := cur.SenderID
	if peerID == authedID {
		peerID = cur.RecipientID
	}
	if h.guardRecipient(r.Context(), authedID, peerID) != "" {
		httpx.WriteError(w, http.StatusNotFound, errMessageNotFound)
		return false
	}
	return true
}

// parseLimit clamps the history ?limit= query param.
func parseLimit(raw string) int {
	return clampLimit(raw, defaultHistoryLimit, maxHistoryLimit)
}

// clampLimit clamps a ?limit= query param into [1, max], falling back to def for
// missing or unparseable values.
func clampLimit(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// parseSince reads the catch-up cursor. Missing, negative or unparseable all mean
// "from the beginning" — a client that lost its cursor should get everything, not
// an error it can do nothing with.
func parseSince(raw string) int64 {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
