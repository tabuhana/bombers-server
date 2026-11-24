// Package patchnotes is the archive of what changed in every release of the
// desktop app, one note per version.
//
// It is its own table and its own route family, not a column on `releases`,
// because `POST /releases` prunes to the newest three and these are NEVER
// pruned: the app's patch-notes screen lists every release that ever shipped.
//
// Unlike the opaque store bundles, this server DOES judge the shape of what it
// stores. A note is a small, fixed contract — a list of {title, body} parts —
// and the client draws exactly a title and a paragraph for each, so a typo'd
// field refused here is better than a blank part stored there.
package patchnotes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned for a version with no note.
var ErrNotFound = errors.New("patch notes not found")

// The limits of a note. Lengths are in characters (runes), not bytes, so a
// title in any script gets the same room.
const (
	MaxParts       = 20
	MaxTitle       = 200
	MaxBody        = 4000
	maxVersionPart = 1000000 // the largest MAJOR, MINOR or PATCH
)

// Part is one titled paragraph of a note.
type Part struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Entry is one version's note as it goes over the wire. There are no dates on
// purpose: an archived note's upload date is not its release date, and showing
// it would mislead.
type Entry struct {
	Version string `json:"version"`
	Parts   []Part `json:"parts"`
}

// parseVersion accepts exactly MAJOR.MINOR.PATCH — each a decimal integer with
// no sign, no leading zeros (bar `0` itself) and at most maxVersionPart — and
// returns the three numbers. The canonical string is the input itself, because
// nothing but the canonical form is accepted. This is narrower than
// releases.ValidVersion on purpose: that one admits prerelease and build
// suffixes because it only guards an object-storage key.
func parseVersion(s string) (major, minor, patch int, ok bool) {
	fields := strings.Split(s, ".")
	if len(fields) != 3 {
		return 0, 0, 0, false
	}
	var nums [3]int
	for i, f := range fields {
		if f == "" || (len(f) > 1 && f[0] == '0') {
			return 0, 0, 0, false
		}
		for j := range len(f) {
			if f[j] < '0' || f[j] > '9' {
				return 0, 0, 0, false
			}
		}
		n, err := strconv.Atoi(f)
		if err != nil || n > maxVersionPart {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

var errBadShape = errors.New("invalid patch notes")

// parseNote decodes and validates a request body into the parts to store,
// trimmed. Every failure is errBadShape; the handler does not say which
// rule, only that the shape is wrong.
func parseNote(raw []byte) ([]Part, error) {
	// Go's decoder silently turns invalid UTF-8 into U+FFFD, which would store
	// a note the sender never wrote. Refuse it instead.
	if !utf8.Valid(raw) {
		return nil, errBadShape
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var note struct {
		Parts []Part `json:"parts"`
	}
	if err := dec.Decode(&note); err != nil {
		return nil, errBadShape
	}
	// Anything after the object, even whitespace-separated JSON, is refused.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errBadShape
	}

	if len(note.Parts) < 1 || len(note.Parts) > MaxParts {
		return nil, errBadShape
	}
	out := make([]Part, len(note.Parts))
	for i, p := range note.Parts {
		title := strings.TrimSpace(p.Title)
		body := strings.TrimSpace(p.Body)
		// Postgres JSONB rejects \u0000, which would surface as a 500, so a NUL
		// is refused here rather than stripped.
		if strings.ContainsRune(p.Title, 0) || strings.ContainsRune(p.Body, 0) {
			return nil, errBadShape
		}
		// The raw-bytes check above cannot see a broken character sent as an
		// escape: the decoder turns a lone surrogate (`\udc00`) into U+FFFD just
		// as silently. Nobody writes U+FFFD on purpose, so its presence means the
		// text was damaged on the way here, and it is refused like the rest.
		if strings.ContainsRune(p.Title, utf8.RuneError) || strings.ContainsRune(p.Body, utf8.RuneError) {
			return nil, errBadShape
		}
		if n := utf8.RuneCountInString(title); n < 1 || n > MaxTitle {
			return nil, errBadShape
		}
		if n := utf8.RuneCountInString(body); n < 1 || n > MaxBody {
			return nil, errBadShape
		}
		out[i] = Part{Title: title, Body: body}
	}
	return out, nil
}

// listSQL orders by the version NUMBERS — never by upload time and never by the
// text. The archive is uploaded in one go, so upload order is meaningless; a
// reworded old note is a republish that must not jump to the top; and text
// order would put 0.1.9 above 0.1.10. No timestamp is selected: none goes on
// the wire.
const listSQL = `
SELECT version, parts FROM patch_notes
ORDER BY major DESC, minor DESC, patch DESC
`

const getSQL = `SELECT version, parts FROM patch_notes WHERE version = $1`

// upsertSQL is one statement so there is no read-then-write race. `xmax = 0` is
// true only for a row this statement inserted, which is how the caller tells a
// new note (201) from a replaced one (200). A replace keeps created_at.
const upsertSQL = `
INSERT INTO patch_notes (version, major, minor, patch, parts)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (version) DO UPDATE SET
  parts      = EXCLUDED.parts,
  updated_at = now()
RETURNING (xmax = 0) AS inserted
`

const deleteSQL = `DELETE FROM patch_notes WHERE version = $1`

// List returns every note, newest version first. Never nil.
func List(ctx context.Context, pool *pgxpool.Pool) ([]Entry, error) {
	rows, err := pool.Query(ctx, listSQL)
	if err != nil {
		return nil, fmt.Errorf("list patch notes: %w", err)
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Version, &e.Parts); err != nil {
			return nil, fmt.Errorf("scan patch notes: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get returns one version's note.
func Get(ctx context.Context, pool *pgxpool.Pool, version string) (*Entry, error) {
	var e Entry
	err := pool.QueryRow(ctx, getSQL, version).Scan(&e.Version, &e.Parts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read patch notes: %w", err)
	}
	return &e, nil
}

// Put writes a note and reports whether it was new.
func Put(ctx context.Context, pool *pgxpool.Pool, version string, major, minor, patch int, parts []Part) (bool, error) {
	data, err := json.Marshal(parts)
	if err != nil {
		return false, fmt.Errorf("encode patch notes: %w", err)
	}
	var inserted bool
	if err := pool.QueryRow(ctx, upsertSQL, version, major, minor, patch, data).Scan(&inserted); err != nil {
		return false, fmt.Errorf("write patch notes: %w", err)
	}
	return inserted, nil
}

// Delete removes a version's note and reports whether there was one.
func Delete(ctx context.Context, pool *pgxpool.Pool, version string) (bool, error) {
	tag, err := pool.Exec(ctx, deleteSQL, version)
	if err != nil {
		return false, fmt.Errorf("delete patch notes: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
