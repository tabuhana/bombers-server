package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/tabuhana/bombers-server/internal/config"
	"github.com/tabuhana/bombers-server/internal/embeddedpg"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/migrate"
	"github.com/tabuhana/bombers-server/internal/setup"
)

// `bombers update` — fetch the new code, build it, bring the database up to
// date, then get out of the way.
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

	// Refuse while the server is up.
	//
	// Migrating needs a database, and with the embedded backend `update` starts
	// one — but a running server already has its own on that port. Worse, the
	// port check reads a LIVE database as a leftover from a crash and tries to
	// stop it, so the command that was supposed to update the server instead
	// reaches over and kills the database out from under it.
	//
	// Stopping first is a decision the operator has to make anyway: an update
	// replaces the binary, and the running process is the old one until it is
	// restarted regardless.
	// Two ways to be running, and the pidfile only knows about one of them: a
	// service-launched server never backgrounds itself, so it writes none. The
	// database port catches that case.
	if embeddedpg.InUse() {
		logx.Fatal("something is already using the database — the server is probably running. Stop it first:\n" +
			"    bombers stop && bombers update && bombers start\n" +
			"  (or `sudo bombers service stop` if it runs as a service)")
	}
	if pidPath, _, perr := runtimePaths(); perr == nil {
		if pid := runningPid(pidPath); pid != 0 {
			logx.Fatal("the server is running (pid %d) — stop it first:\n"+
				"    bombers stop && bombers update && bombers start\n"+
				"  (or `bombers service stop` if it runs as a service)", pid)
		}
	}

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
		// Pull first. `update` used to rebuild whatever happened to be in the
		// checkout, which made "I updated and nothing changed" a normal
		// experience — the code you meant to run was still on the remote.
		//
		// It refuses on a dirty tree rather than stashing or forcing: local
		// changes on a server are either something you're mid-way through or
		// something you forgot, and neither is improved by a command throwing
		// them away to save you a step.
		if err := pullSource(rec.Source); err != nil {
			logx.Fatal("update: %v", err)
		}
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

// pullSource brings the recorded checkout up to date with its remote.
//
// Skipped silently when the source isn't a git checkout at all — somebody may
// have unpacked a tarball, and "you didn't clone this" is not a reason to
// refuse to rebuild it.
func pullSource(source string) error {
	if _, err := os.Stat(filepath.Join(source, ".git")); err != nil {
		logx.Info("update: %s isn't a git checkout — building what's there", source)
		return nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		logx.Warn("update: git isn't installed — building what's already in %s", source)
		return nil
	}

	// A dirty tree stops the update. Building it would work; the problem is
	// that the binary would then be from code that exists nowhere else, and
	// nobody would know until they tried to reproduce it.
	dirty := exec.Command("git", "status", "--porcelain")
	dirty.Dir = source
	out, err := dirty.Output()
	if err != nil {
		return fmt.Errorf("checking %s for local changes: %w", source, err)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return fmt.Errorf("%s has uncommitted changes — commit, stash or discard them first:\n%s", source, strings.TrimSpace(string(out)))
	}

	pull := exec.Command("git", "pull", "--ff-only")
	pull.Dir = source
	pull.Stdout, pull.Stderr = os.Stdout, os.Stderr
	logx.Info("update: pulling %s", source)
	if err := pull.Run(); err != nil {
		// --ff-only, so this is a real divergence rather than a merge waiting to
		// be resolved. Say so plainly instead of leaving a half-merged checkout.
		return fmt.Errorf("git pull failed — the checkout has diverged from its remote and needs a look: %w", err)
	}
	return nil
}
