// Package migrations embeds the SQL migration files into the binary so the
// server can apply them programmatically at startup in local mode, without the
// goose CLI installed or a separate deploy step (LOCAL_MODE.md §9).
//
// The goose CLI ignores this .go file (it only reads the .sql files), so the
// existing `goose -dir migrations postgres <url> up` workflow keeps working for
// the managed/manual path — both the CLI and internal/migrate run the exact
// same `-- +goose Up/Down` files.
package migrations

import "embed"

// FS is the embedded set of `*.sql` migration files, rooted at this directory.
// internal/migrate roots goose at "." against this FS.
//
//go:embed *.sql
var FS embed.FS
