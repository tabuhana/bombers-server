package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"

	"github.com/tabuhana/bombers-server/internal/config"
	"github.com/tabuhana/bombers-server/internal/embeddedpg"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/migrate"
	"github.com/tabuhana/bombers-server/internal/setup"
)

// `bombers update` — bring the database up to date with the code you just built,
// then get out of the way.
//
// It does NOT serve and it does NOT open the admin console. Updating and running
// are separate acts: this brings up only what it needs, migrates, puts it back
// down, and tells you where the schema landed. You start the server yourself.
//
// The one wrinkle it handles for you: with embedded Postgres there is no
// database running between server starts, so `update` starts it, migrates, and
// stops it again. With an external Postgres (docker, systemd, managed) it just
// migrates what's already there.
func runUpdate(_ []string) {
	loadEnvAndConfig()
	logx.Init()

	// Rebuild from the checkout recorded at install time, so `bombers update`
	// works from any directory.
	//
	// Then migrate from that SAME checkout, in this process. The migrations are
	// embedded in the binary, and this binary is by definition the old one — so
	// its own embed is stale by the time it gets here. Reading them off disk
	// solves that with nothing up its sleeve: `update` just built from those
	// files, so they are the newest ones in existence.
	//
	// It used to rebuild and then exec the new binary with a hidden argument,
	// which made a private command that could never be renamed — the OLD binary
	// is the one choosing what to call, so any change to the name broke every
	// installed copy that hadn't updated yet. There is no such command now, and
	// nothing to keep in step.
	if rec, rerr := loadInstallRecord(); rerr == nil {
		if err := buildInto(rec.Source, rec.Binary); err != nil {
			logx.Fatal("update: %v", err)
		}
		logx.Info("update: rebuilt %s", rec.Binary)
		migrateNow(filepath.Join(rec.Source, migrationsDir))
		return
	}
	logx.Warn("update: no install record — migrating with this binary's own migrations (run `bombers install` to enable rebuilds)")

	migrateNow("")
}

// Where the .sql files live inside a source checkout — the directory the
// `migrations` package embeds.
const migrationsDir = "migrations"

// migrateNow brings the schema up to date, starting the server's own Postgres
// first when that's the backend (nothing is listening between server runs, so a
// plain migrate would have nothing to connect to) and stopping it after.
//
// `dir` is a migrations directory to read from, or "" for the ones embedded in
// this binary. A fresh `setup` uses the embed — the binary you just built IS the
// newest thing there is — and `update` passes the checkout it rebuilt from.
func migrateNow(dir string) {
	cfg, err := config.Load()
	if err != nil {
		logx.Fatal("config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	connString := cfg.DatabaseURL

	if cfg.DBBackend == "embedded" {
		dataDir, derr := setup.DataDir()
		if derr != nil {
			logx.Fatal("update: embedded Postgres needs a data directory: %v", derr)
		}
		logx.Info("update: starting the database")
		epg, conn, serr := embeddedpg.Start(dataDir)
		if serr != nil {
			logx.Fatal("update: starting Postgres: %v", serr)
		}
		connString = conn
		// Always put it back down — this command owns the database only for as
		// long as the migration takes.
		defer func() {
			logx.Info("update: stopping the database")
			if err := epg.Stop(); err != nil {
				logx.Error("update: stopping Postgres: %v", err)
			}
		}()
	}

	logx.Info("update: applying migrations")
	if err := migrate.UpFrom(ctx, connString, dir); err != nil {
		if isUnreachableDB(err) {
			logx.Fatal("could not reach the database at %s - is Postgres running?", redactDBURL(connString))
		}
		logx.Fatal("update: %v", err)
	}

	version, verr := migrate.Version(ctx, connString)
	if verr != nil {
		logx.Info("Server migrated!")
		return
	}
	logx.Info("Server migrated to version %d!", version)
}

// loadEnvAndConfig layers the saved local config under the environment, the same
// way the server does at startup, so `update` sees the same database either way.
func loadEnvAndConfig() {
	_ = godotenv.Load()
	if dir, err := setup.DataDir(); err == nil {
		if fc, ferr := setup.Load(dir); ferr == nil {
			setup.EnsureSecret(fc)
			fc.Apply()
		}
	}
}
