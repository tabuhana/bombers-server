package main

import (
	"flag"

	"github.com/tabuhana/bombers-server/internal/logx"
)

// `bombers update` — what you run after pulling and rebuilding: apply whatever
// migrations came with the new code, then serve. One command instead of
// remembering `migrate` and then `start`.
//
// It has to be the FRESHLY BUILT binary, because migrations are embedded in it:
// the old binary's `update` would apply the old set and report success.
func runUpdate(args []string) {
	flags := flag.NewFlagSet("update", flag.ExitOnError)
	noStart := flags.Bool("no-start", false, "apply migrations but don't serve (systemd/service setups)")
	headless := flags.Bool("headless", false, "serve without the interactive console")
	_ = flags.Parse(args)

	runMigrate(nil)

	if *noStart {
		logx.Info("update: migrations applied — start the service when you're ready")
		return
	}
	if *headless {
		runStart([]string{"--headless"})
		return
	}
	runStart(nil)
}
