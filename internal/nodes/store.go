package nodes

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Node is the catalog metadata for a published node (no bundle bytes). The JSON
// tags are the wire shape the client reads from GET /nodes.
type Node struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Width       int      `json:"width"`
	Height      int      `json:"height"`
	CanRotate   bool     `json:"canRotate"`
	Permissions []string `json:"permissions"`
	Hash        string   `json:"hash"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// List returns catalog metadata for every published node, ordered by name.
func (s *Store) List(ctx context.Context) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, version, description, author, width, height, can_rotate, permissions, hash
		FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Name, &n.Version, &n.Description, &n.Author,
			&n.Width, &n.Height, &n.CanRotate, &n.Permissions, &n.Hash); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Bundle returns the node.js bytes and its hash for a node id, or pgx.ErrNoRows.
func (s *Store) Bundle(ctx context.Context, id string) ([]byte, string, error) {
	var bundle []byte
	var hash string
	err := s.pool.QueryRow(ctx, `SELECT bundle, hash FROM nodes WHERE id = $1`, id).Scan(&bundle, &hash)
	if err != nil {
		return nil, "", err
	}
	return bundle, hash, nil
}

// Upsert publishes (or replaces) a node and its bundle.
func (s *Store) Upsert(ctx context.Context, n Node, bundle []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO nodes
			(id, name, version, description, author, width, height, can_rotate, permissions, hash, bundle, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name, version = EXCLUDED.version, description = EXCLUDED.description,
			author = EXCLUDED.author, width = EXCLUDED.width, height = EXCLUDED.height,
			can_rotate = EXCLUDED.can_rotate, permissions = EXCLUDED.permissions,
			hash = EXCLUDED.hash, bundle = EXCLUDED.bundle, updated_at = now()`,
		n.ID, n.Name, n.Version, n.Description, n.Author, n.Width, n.Height,
		n.CanRotate, n.Permissions, n.Hash, bundle)
	return err
}
