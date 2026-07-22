package main

import (
	"context"
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
	if err := migrate.Up(ctx, connString); err != nil {
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
