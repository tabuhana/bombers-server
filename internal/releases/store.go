// Package releases is how a running copy of the desktop app gets a newer one.
//
// It is the fourth store on this server and by far the smallest: one file and a
// signature, where a node, a pack or a game is a bundle plus a spread of assets.
// What's different is the reader. A catalogue is browsed by a person who can see
// that something looks wrong and not press Install; a release is read by the
// updater inside an app that is about to replace its own executable. So the
// signature is a required field, the bytes go through the server like every
// other file here, and "what is the newest" is a question the OPERATOR answers
// (most recently published wins) rather than one decided by comparing numbers.
//
// That last part matches the client's store rule exactly — "different from
// what's published", not "greater than what I have" — so republishing an older
// build is how you roll a bad release back, and it reaches everyone.
package releases

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned for a version that isn't published.
var ErrNotFound = errors.New("release not found")

// ArtifactLimit caps one installer. A Windows build of this app is tens of
// megabytes, so this is roughly four times the largest plausible one: the cap is
// here to stop a mistake — a wrong path, a video, a whole target/ directory —
// not to accommodate one. The body is read into memory to be stored, and this is
// what bounds that.
const ArtifactLimit = 128 << 20 // 128 MiB

// DefaultPlatform is the only platform there is. The client is Windows-only and
// permanently so; this exists as a constant rather than a literal because the
// update manifest keys its platform map by exactly this string, and the two must
// agree or an updater sees a manifest with nothing in it for the machine it's on.
const DefaultPlatform = "windows-x86_64"

// Record is one published release.
type Record struct {
	Version     string
	Platform    string
	Notes       string
	Signature   string
	Artifact    string // the installer's filename, e.g. bombers_0.1.1_x64-setup.exe
	ContentType string
	Size        int64
	PublishedAt time.Time
}

// ArtifactKey is where an installer's bytes live in object storage. One rule,
// used by the writer and the reader, so they can't disagree.
func ArtifactKey(version, artifact string) string {
	return fmt.Sprintf("releases/%s/%s", version, artifact)
}

// ValidVersion guards one half of that key. A version arrives in a request body
// and becomes a FOLDER, so it gets the same narrow, positive treatment a pack id
// does: the characters a version number is made of, and nothing that could name
// a directory.
func ValidVersion(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '+', r == '_':
		default:
			return false
		}
	}
	return v != "." && v != ".."
}

// ValidArtifact guards the other half. Unlike a pack asset this is a single
// FILE, never a path — the installer sits directly in its version's folder — so
// a separator of any kind is simply wrong rather than something to sanitise.
func ValidArtifact(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return false
	}
	return true
}

const upsertSQL = `
INSERT INTO releases (version, platform, notes, signature, artifact, content_type, size_bytes, published_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
ON CONFLICT (version) DO UPDATE SET
  platform     = EXCLUDED.platform,
  notes        = EXCLUDED.notes,
  signature    = EXCLUDED.signature,
  artifact     = EXCLUDED.artifact,
  content_type = EXCLUDED.content_type,
  size_bytes   = EXCLUDED.size_bytes,
  published_at = NOW()
RETURNING published_at
`

// Upsert publishes (or republishes) a release's metadata. The bytes follow
// separately, which is why size starts at whatever the caller knows — usually
// zero until the artifact lands.
func Upsert(ctx context.Context, pool *pgxpool.Pool, rec Record) (time.Time, error) {
	if rec.Platform == "" {
		rec.Platform = DefaultPlatform
	}
	if rec.ContentType == "" {
		rec.ContentType = "application/octet-stream"
	}
	var published time.Time
	err := pool.QueryRow(ctx, upsertSQL,
		rec.Version, rec.Platform, rec.Notes, rec.Signature, rec.Artifact, rec.ContentType, rec.Size,
	).Scan(&published)
	if err != nil {
		return time.Time{}, fmt.Errorf("publish release: %w", err)
	}
	return published, nil
}

const setArtifactSQL = `
UPDATE releases
SET artifact = $2, content_type = $3, size_bytes = $4
WHERE version = $1
`

// SetArtifact records the file that actually arrived. Publishing states the
// intent; this states the fact, and until it runs a release's size is zero —
// which is exactly how the console shows an upload that never finished.
func SetArtifact(ctx context.Context, pool *pgxpool.Pool, version, artifact, contentType string, size int64) error {
	tag, err := pool.Exec(ctx, setArtifactSQL, version, artifact, contentType, size)
	if err != nil {
		return fmt.Errorf("record artifact: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const selectCols = `version, platform, notes, signature, artifact, content_type, size_bytes, published_at`

func scan(row pgx.Row) (*Record, error) {
	var r Record
	err := row.Scan(&r.Version, &r.Platform, &r.Notes, &r.Signature, &r.Artifact, &r.ContentType, &r.Size, &r.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read release: %w", err)
	}
	return &r, nil
}

// latestSQL is the whole of the "which release is being offered" decision, and
// both of its clauses are load-bearing:
//
//   - `size_bytes > 0` skips a row whose bytes never arrived. A half-finished
//     publish is a real state (metadata lands first, the file follows), and
//     offering an update that 404s on download would leave every client
//     retrying a broken install.
//   - ORDER BY published_at, not by version. The operator's last act is the
//     answer, which is what makes republishing an older build a rollback.
const latestSQL = `
SELECT ` + selectCols + ` FROM releases
WHERE size_bytes > 0
ORDER BY published_at DESC
LIMIT 1
`

// Latest is the release the updater is offered. See latestSQL.
func Latest(ctx context.Context, pool *pgxpool.Pool) (*Record, error) {
	return scan(pool.QueryRow(ctx, latestSQL))
}

// Get returns one published version.
func Get(ctx context.Context, pool *pgxpool.Pool, version string) (*Record, error) {
	return scan(pool.QueryRow(ctx, `SELECT `+selectCols+` FROM releases WHERE version = $1`, version))
}

// List returns every release, newest first — the console's listing.
func List(ctx context.Context, pool *pgxpool.Pool) ([]Record, error) {
	rows, err := pool.Query(ctx, `SELECT `+selectCols+` FROM releases ORDER BY published_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	defer rows.Close()

	out := []Record{}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Version, &r.Platform, &r.Notes, &r.Signature, &r.Artifact, &r.ContentType, &r.Size, &r.PublishedAt); err != nil {
			return nil, fmt.Errorf("scan release: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// KeepReleases is how many releases survive a publish.
//
// Not one, because `unpublish-release` is the fast rollback: pulling a bad build
// drops clients onto the newest release STILL published, and that only works if
// a previous one is there. Delete aggressively and you delete your undo.
//
// Not unbounded either, which is what it was — at 2.0.1, 1.0.0 was still sitting
// in object storage with nothing pointing at it. Three gives two rollback steps
// and stops the disk growing forever.
const KeepReleases = 3

// pruneSQL removes everything past the newest `keep` by publish time — the same
// ordering Latest uses, so "newest" means one thing in this package. A rollback
// republishes an old version, which makes it the newest, so it is never the row
// this deletes.
const pruneSQL = `
DELETE FROM releases
WHERE version IN (
  SELECT version FROM releases
  ORDER BY published_at DESC
  OFFSET $1
)
RETURNING version, artifact
`

// PruneOld drops all but the newest `keep` releases and reports what went, so
// the caller can remove their stored bytes too — the rows cascade nowhere, and
// object storage has no idea a release existed.
func PruneOld(ctx context.Context, pool *pgxpool.Pool, keep int) ([]Record, error) {
	if keep < 1 {
		// A publish that wiped every release including itself would be a very
		// expensive typo.
		return nil, fmt.Errorf("keep must be at least 1")
	}
	rows, err := pool.Query(ctx, pruneSQL, keep)
	if err != nil {
		return nil, fmt.Errorf("prune releases: %w", err)
	}
	defer rows.Close()

	var gone []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Version, &r.Artifact); err != nil {
			return nil, fmt.Errorf("scan pruned release: %w", err)
		}
		gone = append(gone, r)
	}
	return gone, rows.Err()
}

// Delete unpublishes a version. The caller removes the stored bytes — there is
// no cascade for object storage.
func Delete(ctx context.Context, pool *pgxpool.Pool, version string) (bool, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM releases WHERE version = $1`, version)
	if err != nil {
		return false, fmt.Errorf("unpublish release: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
