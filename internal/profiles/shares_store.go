package profiles

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sharing storage: which of an owner's self-card FIELDS each viewer may see.
//
// The unit is (owner, field, viewer) — never a group. The client's relationship
// groups ("Partner", "Family", whatever the user invented) are resolved to user
// ids before they get here, so the server can stay ignorant of a vocabulary that
// only exists in one person's notebook and can be renamed or deleted at will.

// The field keys the server recognises. Anything else in a publish is rejected,
// so a typo can't silently create a grant nobody can ever read.
const (
	FieldBirthday = "birthday"
	FieldLocation = "location"
	FieldNickname = "nickname"

	// NotePrefix marks a per-CATEGORY note field: "note:<categoryID>". The
	// categories are the user's own ("Favorites", "Dislikes", anything they
	// invent), so the set can't be fixed here — the server validates the SHAPE
	// of the key and stays ignorant of what the category means, exactly as it
	// stays ignorant of relationship groups.
	NotePrefix = "note:"

	maxCategoryIDLen = 64
)

// isKnownField validates a field key from a publish.
func isKnownField(key string) bool {
	switch key {
	case FieldBirthday, FieldLocation, FieldNickname:
		return true
	}
	if !strings.HasPrefix(key, NotePrefix) {
		return false
	}
	id := strings.TrimPrefix(key, NotePrefix)
	if id == "" || len(id) > maxCategoryIDLen {
		return false
	}
	// Ids are client-generated (uuid or a short slug); keep them boring so a key
	// can never smuggle anything odd into a response.
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// maxGrantsPerPublish caps one publish so a bad client can't write an unbounded
// number of rows in a single request. Four fields × a big friend list, with room
// to spare.
const maxGrantsPerPublish = 4000

const getSharesSQL = `
SELECT field_key, viewer_id
FROM profile_shares
WHERE owner_id = $1
ORDER BY field_key, viewer_id
`

// getShares returns the owner's full grant map: field key → viewer ids. Every
// known field is present in the result (empty slice = shared with nobody), so
// the client always receives a complete, editable shape.
func getShares(ctx context.Context, db dbExecutor, ownerID string) (map[string][]string, error) {
	// Seed the fixed fields so the client always gets a complete shape for them.
	// Note categories are open-ended, so they appear only once granted.
	out := map[string][]string{
		FieldBirthday: {},
		FieldLocation: {},
		FieldNickname: {},
	}

	rows, err := db.Query(ctx, getSharesSQL, ownerID)
	if err != nil {
		return nil, fmt.Errorf("get shares: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var field, viewer string
		if err := rows.Scan(&field, &viewer); err != nil {
			return nil, fmt.Errorf("scan share: %w", err)
		}
		out[field] = append(out[field], viewer)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate shares: %w", err)
	}
	return out, nil
}

const deleteSharesSQL = `DELETE FROM profile_shares WHERE owner_id = $1`

const insertShareSQL = `
INSERT INTO profile_shares (owner_id, field_key, viewer_id)
VALUES ($1, $2, $3)
ON CONFLICT (owner_id, field_key, viewer_id) DO NOTHING
`

// replaceShares makes the stored grants exactly match `shares` in ONE
// transaction: wipe, then re-insert. A publish is the client stating its whole
// intent, so a replace (not a merge) is what keeps revocation working — dropping
// a group from a field, or deleting the group entirely, simply produces fewer
// rows next time.
//
// Only accepted friends may be granted anything: a viewer who isn't a friend is
// silently skipped rather than erroring, because the common cause is benign (you
// un-friended someone the client hasn't re-resolved yet) and a hard failure
// would block the whole publish over one stale id.
func replaceShares(ctx context.Context, pool *pgxpool.Pool, ownerID string, shares map[string][]string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin shares tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, deleteSharesSQL, ownerID); err != nil {
		return fmt.Errorf("clear shares: %w", err)
	}

	// One friendship lookup per distinct viewer, not per grant.
	allowed := map[string]bool{}
	for field, viewers := range shares {
		if !isKnownField(field) {
			continue
		}
		for _, viewer := range viewers {
			if viewer == "" || viewer == ownerID {
				continue
			}
			if _, seen := allowed[viewer]; seen {
				continue
			}
			ok, err := areFriends(ctx, tx, ownerID, viewer)
			if err != nil {
				return fmt.Errorf("check share viewer: %w", err)
			}
			allowed[viewer] = ok
		}
	}

	for field, viewers := range shares {
		if !isKnownField(field) {
			continue
		}
		for _, viewer := range viewers {
			if !allowed[viewer] {
				continue
			}
			if _, err := tx.Exec(ctx, insertShareSQL, ownerID, field, viewer); err != nil {
				return fmt.Errorf("insert share: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit shares: %w", err)
	}
	return nil
}

const sharedFieldsSQL = `
SELECT field_key
FROM profile_shares
WHERE owner_id = $1 AND viewer_id = $2
`

// sharedFieldsFor answers the read-path question: which of `ownerID`'s fields is
// `viewerID` allowed to see? Returns a set; an owner who never published gets an
// empty one, which is the correct default (private until you say otherwise).
func sharedFieldsFor(ctx context.Context, db dbExecutor, ownerID, viewerID string) (map[string]bool, error) {
	rows, err := db.Query(ctx, sharedFieldsSQL, ownerID, viewerID)
	if err != nil {
		return nil, fmt.Errorf("get shared fields: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var field string
		if err := rows.Scan(&field); err != nil {
			return nil, fmt.Errorf("scan shared field: %w", err)
		}
		out[field] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate shared fields: %w", err)
	}
	return out, nil
}

// countGrants totals the grants in a publish request (for the size cap).
func countGrants(shares map[string][]string) int {
	n := 0
	for _, viewers := range shares {
		n += len(viewers)
	}
	return n
}

// ensure pgx.Tx satisfies dbExecutor (compile-time check for the tx path above).
var _ dbExecutor = pgx.Tx(nil)
