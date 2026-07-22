package main

import (
	"flag"

	"github.com/tabuhana/bombers-server/internal/config"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/setup"
)

// `bombers update` - what you run after pulling and rebuilding: make sure the
// schema matches the new code, then serve. One command instead of remembering
// `migrate` and then `start`.
//
// It has to be the FRESHLY BUILT binary, because migrations are embedded in it:
// the old binary would apply the old set and report success.
//
// It also has to respect which database this host uses, and that distinction is
// the whole reason this isn't a shell alias:
//
//   - EMBEDDED Postgres (local self-host): the server starts the database
//     itself and migrates during startup. Nothing is listening beforehand, so a
//     standalone migrate here would simply fail to connect. `update` therefore
//     goes straight to `start`, which does both.
//   - EXTERNAL Postgres (docker, managed, or a system service): the database is
//     already up and nobody else applies migrations, so migrate first, then serve.
func runUpdate(args []string) {
	flags := flag.NewFlagSet("update", flag.ExitOnError)
	noStart := flags.Bool("no-start", false, "apply migrations but don't serve (systemd/service setups)")
	headless := flags.Bool("headless", false, "serve without the interactive console")
	_ = flags.Parse(args)

	if embeddedBackend() {
		logx.Info("update: this server runs its own Postgres - migrations are applied as it starts")
		if *noStart {
			logx.Info("update: nothing to do without starting; run `bombers start` when ready")
			return
		}
		startWith(*headless)
		return
	}

	runMigrate(nil)

	if *noStart {
		logx.Info("update: migrations applied - start the service when you're ready")
		return
	}
	startWith(*headless)
}

func startWith(headless bool) {
	if headless {
		runStart([]string{"--headless"})
		return
	}
	runStart(nil)
}

// embeddedBackend reports whether this host runs the server-managed Postgres.
// Reads config the same way the server does (saved local config layered under
// the environment); any failure answers "external", which is the default and the
// safe assumption - at worst the caller gets the ordinary connect error.
func embeddedBackend() bool {
	if dir, err := setup.DataDir(); err == nil {
		if fc, ferr := setup.Load(dir); ferr == nil {
			setup.EnsureSecret(fc)
			fc.Apply()
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	return cfg.DBBackend == "embedded"
}
