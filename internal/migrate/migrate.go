// Package migrate applies the embedded SQL migrations programmatically at
// startup for local self-host mode, so a self-hoster never installs goose or
// runs a separate step (LOCAL_MODE.md §9). It drives the goose v3 LIBRARY over
// the migrations embedded by the migrations package, reusing the same
// goose_db_version tracking table the goose CLI uses — so the CLI and this stay
// perfectly consistent.
//
// Only the embedded-Postgres backend calls this. External Postgres (managed or
// local-external) keeps today's behavior: the operator runs goose deliberately.
package migrate

import (
	"context"
	"database/sql"
	"fmt"

	// Registers the "pgx" database/sql driver as a side effect. It ships with
	// the pgx dependency the server already uses; goose talks to Postgres over
	// database/sql, so it needs a registered driver.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/migrations"
)

// Up opens a database/sql connection to connString and applies every pending
// migration embedded in the binary, then closes the connection. It is
// idempotent: a schema already at the latest version is a no-op ("schema up to
// date"). goose reads the .sql files from the embedded FS rooted at ".".
func Up(ctx context.Context, connString string) error {
	return up(ctx, connString, "")
}

// UpFrom is Up, reading the migrations from a DIRECTORY on disk instead of the
// ones embedded in this binary.
//
// It exists for `update`, which has a genuine chicken-and-egg problem: the
// migrations are embedded, so the running binary only ever carries the ones it
// was built with, and `update` is by definition the OLD binary. It used to solve
// that by rebuilding and then exec'ing the NEW binary with a hidden argument —
// which meant a private command existed forever, and any change to its name
// broke every installed copy in the world, because the OLD binary is the one
// choosing what to call.
//
// This is the same problem answered without a second process: `update` just
// built from a checkout, so the newest migrations are sitting right there on
// disk. Read them from source and there is nothing to exec and no command to
// name.
func UpFrom(ctx context.Context, connString, dir string) error {
	return up(ctx, connString, dir)
}

// up does the work for both. An empty dir means the embedded FS.
func up(ctx context.Context, connString, dir string) error {
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Silence goose's own logger (it would otherwise print via the stdlib log
	// package, which the rest of the server deliberately avoids); the single
	// summary line below reports the outcome through logx. Errors are still
	// surfaced — UpContext RETURNS them, it does not log-and-exit.
	goose.SetLogger(goose.NopLogger())

	// The whole difference between the two entry points. goose reads real files
	// unless you hand it a base FS, and the setting is global — so a directory
	// run must CLEAR it rather than merely not set it, or an earlier embedded
	// run in the same process would still be in force.
	root := "."
	if dir == "" {
		goose.SetBaseFS(migrations.FS)
	} else {
		goose.SetBaseFS(nil)
		root = dir
	}

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	// Current version before applying — GetDBVersionContext creates the tracking
	// table on a fresh DB and returns 0. Any error here (e.g. a not-yet-applied
	// table) is treated as version 0 so a first run still migrates; a genuine
	// connection failure resurfaces from UpContext right below.
	before, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		before = 0
	}

	if err := goose.UpContext(ctx, db, root); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	after, verr := goose.GetDBVersionContext(ctx, db)
	switch {
	case verr != nil:
		logx.Info("migrations applied")
	case after == before:
		logx.Info("database schema up to date (version %d)", after)
	default:
		applied := 0
		if migs, cerr := goose.CollectMigrations(root, before, after); cerr == nil {
			applied = len(migs)
		}
		if applied > 0 {
			logx.Info("applied %d migration(s); schema now at version %d", applied, after)
		} else {
			logx.Info("migrations applied; schema now at version %d", after)
		}
	}
	return nil
}

// Version reports the schema version currently recorded in the database. Used by
// `bombers update` to state plainly where the schema ended up.
func Version(ctx context.Context, connString string) (int64, error) {
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return 0, fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetLogger(goose.NopLogger())
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return 0, fmt.Errorf("set goose dialect: %w", err)
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}
