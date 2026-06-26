package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// About-card visibility values — mirror the CHECK constraint in the migration.
// 'private' (author only) is the default; 'subject' also lets the person the
// card is about read it.
const (
	AboutVisibilityPrivate = "private"
	AboutVisibilitySubject = "subject"
)

// ErrAboutNotFound is returned when no about-card row exists for an (author,
// subject) pair. Callers branch on it (e.g. GetMyAbout returns an empty default).
var ErrAboutNotFound = errors.New("about-card not found")

// aboutRecord is the in-memory view of an about_cards row. Content is the opaque
// client-owned JSON blob, carried as raw bytes (the server never inspects it).
type aboutRecord struct {
	AuthorID   string
	SubjectID  string
	Content    json.RawMessage
	Visibility string
	UpdatedAt  time.Time
}

const getAboutSQL = `
SELECT author_id, subject_id, content, visibility, updated_at
FROM about_cards
WHERE author_id = $1 AND subject_id = $2
`

func getAbout(ctx context.Context, db dbExecutor, authorID, subjectID string) (*aboutRecord, error) {
	var a aboutRecord
	err := db.QueryRow(ctx, getAboutSQL, authorID, subjectID).Scan(
		&a.AuthorID, &a.SubjectID, &a.Content, &a.Visibility, &a.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAboutNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get about-card: %w", err)
	}
	return &a, nil
}

// upsertAbout inserts or replaces the caller's about-card for one subject. One
// row per (author, subject), so ON CONFLICT overwrites content + visibility and
// bumps updated_at; created_at is preserved.
const upsertAboutSQL = `
INSERT INTO about_cards (author_id, subject_id, content, visibility, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (author_id, subject_id) DO UPDATE SET
  content    = EXCLUDED.content,
  visibility = EXCLUDED.visibility,
  updated_at = NOW()
RETURNING author_id, subject_id, content, visibility, updated_at
`

func upsertAbout(ctx context.Context, pool *pgxpool.Pool, a *aboutRecord) (*aboutRecord, error) {
	var out aboutRecord
	err := pool.QueryRow(ctx, upsertAboutSQL, a.AuthorID, a.SubjectID, a.Content, a.Visibility).Scan(
		&out.AuthorID, &out.SubjectID, &out.Content, &out.Visibility, &out.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert about-card: %w", err)
	}
	return &out, nil
}

const deleteAboutSQL = `DELETE FROM about_cards WHERE author_id = $1 AND subject_id = $2`

func deleteAbout(ctx context.Context, db dbExecutor, authorID, subjectID string) error {
	if _, err := db.Exec(ctx, deleteAboutSQL, authorID, subjectID); err != nil {
		return fmt.Errorf("delete about-card: %w", err)
	}
	return nil
}

// listAboutByAuthor returns all about-cards the author has written, regardless
// of current friendship — this drives cross-device restore of a user's whole
// private notebook. Ordered by subject for a stable response.
const listAboutByAuthorSQL = `
SELECT author_id, subject_id, content, visibility, updated_at
FROM about_cards
WHERE author_id = $1
ORDER BY subject_id
`

func listAboutByAuthor(ctx context.Context, db dbExecutor, authorID string) ([]aboutRecord, error) {
	rows, err := db.Query(ctx, listAboutByAuthorSQL, authorID)
	if err != nil {
		return nil, fmt.Errorf("list about-cards: %w", err)
	}
	defer rows.Close()

	var out []aboutRecord
	for rows.Next() {
		var a aboutRecord
		if err := rows.Scan(&a.AuthorID, &a.SubjectID, &a.Content, &a.Visibility, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan about-card: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate about-cards: %w", err)
	}
	return out, nil
}
