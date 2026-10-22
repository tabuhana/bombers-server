// Package transfers is the doorbell for a file going straight from one person to
// another. The bytes never touch this server: the two clients open a WebRTC data
// channel between themselves (over a room, which is only the place they find each
// other), and this package does the one thing the clients cannot do for
// themselves — tell the receiver a file is on its way.
//
// The offer is a nudge, not a message, and that is deliberate. A file offer that
// lived in a DM would sit in history forever, name a room that is long gone, and
// be forgeable by anyone who could type the string. Here it is a single frame on
// the receiver's nudge socket, stores nothing, and exists only while both people
// are live — which a WebRTC transfer needs anyway.
//
// It never asks whether the receiver is listening, and nothing may be added that
// does. Whether a socket is open can change between the asking and the sending,
// so an answer built on it would be right only some of the time. The sender
// learns that nobody picked up the way it learns anything else about a live
// link: nobody joined the room before its own timeout.
package transfers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
)

const (
	offerBodyLimit = 1 << 12 // 4 KiB — four short fields.

	// maxName is the longest filename an offer may carry, in bytes. 255 is the
	// longest a name can be on the filesystems this app writes to.
	maxName = 255

	// maxRoom bounds the room id. Room ids are 26-character ULIDs; this leaves
	// room to spare without letting a field be used to carry a payload.
	maxRoom = 64

	// maxSize is the largest file an offer may name: 2^53 - 1, the last integer a
	// JavaScript number holds exactly. Every client of this API is JavaScript, so a
	// size beyond it would arrive already wrong.
	maxSize = 1<<53 - 1

	// Client-branching error codes. errRecipient is deliberately opaque: it
	// covers non-friend, blocked, nonexistent and self-target alike, exactly as
	// it does for a direct message.
	errInvalidBody = "invalid_body"
	errBadName     = "bad_name"
	errBadSize     = "bad_size"
	errBadRoom     = "bad_room"
	errRecipient   = "recipient_not_found"
)

// KindTransfer is the notify kind an offer is pushed under. It repeats the value
// of notify.KindTransfer because transfers may not import that package; a test in
// cmd/bombers pins the two together.
const KindTransfer = "transfer"

// Notify delivers one frame of the given kind to every listed user, if anything
// is wired up to hear it. A function rather than an import of the notify package,
// for the same reason messaging takes one: a domain does not reach into another.
//
// Nil is a valid value and means nobody is listening — how the tests build it.
type Notify func(userIDs []string, kind string, data any)

// friendChecker answers the one question the offer gate asks. *pgxpool.Pool
// satisfies it; a test hands in a fake.
type friendChecker interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Handler struct {
	db     friendChecker
	notify Notify
}

func NewHandler(db friendChecker, notify Notify) *Handler {
	return &Handler{db: db, notify: notify}
}

// offerRequest is what a sender posts: who it is for, which room the sender is
// waiting in, and what the file is called and how big.
type offerRequest struct {
	To   string `json:"to"`
	Room string `json:"room"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// offerFrame is the `d` of a transfer frame. `from` is stamped here from the
// authenticated caller and never read from the request, so an offer cannot claim
// to be from somebody else.
type offerFrame struct {
	From string `json:"from"`
	Room string `json:"room"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Offer tells a friend that the authed user has a file for them and is waiting in
// a room. It stores nothing and answers 204. The recipient must be an accepted
// friend, with the same opaque 404 as messaging them. The server does not check
// that the room exists or that it is the caller's: the receiver's join is already
// gated on the room's host, so an offer naming a room that is not the sender's
// gets a receiver a 404 on join and nothing more.
//
// The frame is allowed to go missing. If the receiver has no socket open it is
// simply not delivered, and the sender finds out as it finds out about any other
// absent peer — nobody joined its room in time.
func (h *Handler) Offer(w http.ResponseWriter, r *http.Request) {
	authedID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req offerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, offerBodyLimit)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, errInvalidBody)
		return
	}
	to := strings.TrimSpace(req.To)
	room := strings.TrimSpace(req.Room)
	// The name is NOT trimmed: it is checked exactly as given and sent exactly as
	// given, so what the receiver is told is what the sender's disk holds.
	if !validRoom(room) {
		httpx.WriteError(w, http.StatusBadRequest, errBadRoom)
		return
	}
	if !ValidName(req.Name) {
		httpx.WriteError(w, http.StatusBadRequest, errBadName)
		return
	}
	if req.Size < 1 || req.Size > maxSize {
		httpx.WriteError(w, http.StatusBadRequest, errBadSize)
		return
	}

	if to == "" || to == authedID {
		httpx.WriteError(w, http.StatusNotFound, errRecipient)
		return
	}
	if !h.mayOffer(r.Context(), authedID, to) {
		httpx.WriteError(w, http.StatusNotFound, errRecipient)
		return
	}

	if h.notify != nil {
		h.notify([]string{to}, KindTransfer, offerFrame{From: authedID, Room: room, Name: req.Name, Size: req.Size})
	}
	w.WriteHeader(http.StatusNoContent)
}

// areFriendsSQL is the narrow query transfers owns against the friendships table
// rather than importing internal/friends or messaging — the same loose-coupling
// tradeoff those two already make. The pair is matched in either order. A blocked
// or pending friendship is not 'accepted', so it is refused here without a
// separate check.
const areFriendsSQL = `
SELECT EXISTS (
  SELECT 1 FROM friendships
  WHERE state = 'accepted'
    AND ((user_a_id = $1 AND user_b_id = $2) OR (user_a_id = $2 AND user_b_id = $1))
)
`

// mayOffer reports whether the authed user may put a file in front of other. A
// database failure is logged and collapsed to a refusal, so the handler never
// leaks whether a user exists or is a friend.
func (h *Handler) mayOffer(ctx context.Context, authedID, otherID string) bool {
	var friends bool
	if err := h.db.QueryRow(ctx, areFriendsSQL, authedID, otherID).Scan(&friends); err != nil {
		if !errors.Is(err, context.Canceled) {
			logx.Error("transfers: are friends: %v", err)
		}
		return false
	}
	return friends
}

// validRoom reports whether s could be a room id: non-empty, short, and letters
// and digits only. It does not look the room up.
func validRoom(s string) bool {
	if s == "" || len(s) > maxRoom {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}

// ValidName reports whether s is a plain filename: one name, not a path. It
// refuses anything that could make a receiver write outside the folder it means
// to — a path separator of either kind, or `..` anywhere in the name — and the
// characters Windows will not hold in a filename, since every receiver is
// Windows and ':' in particular names an NTFS alternate stream, not a file.
// Control characters, a name ending in a dot or a space (which Windows silently
// rewrites to another name), and U+FFFD are refused too. The last is what invalid
// UTF-8 has already become by the time JSON decoding hands us the string: a name
// that carries it was damaged before it got here, and is not the name on the disk.
//
// This is the sender-side half of a two-sided rule: the receiver still decides
// the path it writes to and must not trust this.
func ValidName(s string) bool {
	if s == "" || len(s) > maxName || !utf8.ValidString(s) {
		return false
	}
	if strings.TrimSpace(s) == "" || s == "." || strings.Contains(s, "..") {
		return false
	}
	if last := s[len(s)-1]; last == '.' || last == ' ' {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f || c == utf8.RuneError || strings.ContainsRune(`/\<>:"|?*`, c) {
			return false
		}
	}
	return true
}
