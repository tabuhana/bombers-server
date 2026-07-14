package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/kardianos/service"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/config"
	"github.com/tabuhana/bombers-server/internal/console"
	"github.com/tabuhana/bombers-server/internal/embeddedpg"
	"github.com/tabuhana/bombers-server/internal/friends"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/media"
	"github.com/tabuhana/bombers-server/internal/messaging"
	"github.com/tabuhana/bombers-server/internal/migrate"
	"github.com/tabuhana/bombers-server/internal/nodes"
	"github.com/tabuhana/bombers-server/internal/nodeshare"
	"github.com/tabuhana/bombers-server/internal/profiles"
	"github.com/tabuhana/bombers-server/internal/setup"
	"github.com/tabuhana/bombers-server/internal/store"
	"github.com/tabuhana/bombers-server/internal/svc"
	"github.com/tabuhana/bombers-server/internal/sync"
	"github.com/tabuhana/bombers-server/internal/users"
)

// version is the bombers CLI version, reported by `bombers version`.
const version = "0.1.0-dev"

// main is a small subcommand dispatcher. A bare invocation (or one that starts
// straight into flags like --headless) defaults to `start`, so the pre-CLI
// behavior — run the server — is preserved; help/version short-circuit before
// any work happens.
func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			printHelp()
			return
		case "version", "--version":
			printVersion()
			return
		}
	}

	// The command is the first non-flag word; consume it. Otherwise (no args, or
	// a leading flag) default to `start` so `bombers` and `bombers --headless`
	// still launch the server exactly as before.
	cmd := "start"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		rest = args[1:]
	}

	// Build the OS-service handle once: the `service` subcommands drive it, an
	// SCM-launched `start` runs through it, and `uninstall` removes it. Building
	// it is cheap and permission-free — only install/uninstall/start/stop/status
	// actually touch the service manager.
	prg := newProgram()
	s, serr := service.New(prg, svc.Config())
	if serr != nil {
		logx.Init()
		logx.Fatal("configuring OS service: %v", serr)
	}

	switch cmd {
	case "start":
		// Route by how we were launched. Only the OS service manager starts us
		// non-interactively (per kardianos), and that process runs through
		// s.Run(), which drives program.Start/Stop headlessly. A human terminal —
		// including `start --headless` — stays on the original interactive CLI
		// path below, byte-for-byte unchanged.
		if !svc.Interactive() {
			if err := s.Run(); err != nil {
				logx.Init()
				logx.Error("service run: %v", err)
			}
			return
		}
		runStart(rest)
	case "setup":
		runSetup(rest)
	case "doctor":
		runDoctor(rest)
	case "service":
		runService(s, rest)
	case "uninstall":
		runUninstall(s, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		fmt.Fprintln(os.Stderr, "run `bombers help` to see available commands")
		os.Exit(2)
	}
}

// printVersion prints the CLI version line.
func printVersion() {
	fmt.Printf("bombers %s\n", version)
}

// printHelp prints a concise usage block for the bombers CLI.
func printHelp() {
	fmt.Print(`bombers — the Bombers notebook server

Usage:
  bombers [command] [flags]

Commands:
  start      Run the server (the default when no command is given)
  setup      (Re)configure local self-host settings, then exit — does not serve
  doctor     Check the local setup for problems
  service    Manage the OS background service (see actions below)
  uninstall  Remove the OS service and delete the local data directory
  version    Print the version and exit
  help       Show this help and exit

Flags:
  --headless   (start) serve without the interactive admin console; stop with SIGINT/SIGTERM

Service actions:
  bombers service install     Register the binary as an OS background service
  bombers service start       Start the installed service
  bombers service stop        Stop the running service
  bombers service restart     Restart the service
  bombers service status      Report whether the service is installed/running
  bombers service uninstall   Deregister the service (leaves data in place)

  install and uninstall register/remove a system service and need an ELEVATED
  terminal — an Administrator PowerShell on Windows, or sudo on Linux/macOS.

Uninstall:
  bombers uninstall [--yes]   Remove the OS service (if any), then delete the
                              data directory (config.json, filesystem media,
                              embedded-Postgres data + cached binaries) after a
                              confirmation. --yes skips the prompt for scripts.

A bare "bombers" (or "bombers --headless") runs start. On a local self-host,
start auto-detects the saved config and runs the first-run wizard when the
config is incomplete; setup forces that wizard to reconfigure. A managed/cloud
run stays pure-env: nothing is written to disk. Run "bombers setup" once before
"bombers service install" so the service boots from a complete config.
`)
}

// app is a built, listening server plus the resources its shutdown must
// release. buildAndServe returns one; both the CLI (runStart) and the OS
// service (program) drive it, then call shutdown() to tear it down in the same
// order. storage is carried so the CLI can hand it to the admin console.
type app struct {
	srv       *http.Server
	pool      *pgxpool.Pool
	storage   media.Store
	epg       *embeddedpg.Instance
	startedAt time.Time
	host      string
	port      string
}

// buildAndServe layers any local self-host config UNDER the environment (a
// no-op on a fully-env managed host), loads config, opens the DB and media
// store, wires routes, and starts the HTTP listener goroutine — everything up
// to and including "http listening". It returns a handle the caller drives to
// shutdown. Unlike the old inline runStart it does NOT logx.Fatal on setup
// failure: it returns the error (after stopping an already-started embedded
// Postgres) so the service path can react. runStart still turns that error into
// a Fatal to preserve today's CLI behavior. Callers must have already run
// godotenv.Load + logx.Init (buildAndServe logs through logx).
func buildAndServe() (*app, error) {
	// Resolve the local data directory ONCE up front: the local-config layer
	// below uses it, and so does the embedded-Postgres DB backend later. It only
	// resolves a path (nothing is created unless something is actually saved), so
	// a managed/cloud host that never persists config leaves no trace on disk.
	dataDir, dataDirErr := setup.DataDir()

	// Local self-host config layers a persisted JSON file UNDER the environment
	// before config.Load runs: load the file, run the first-run wizard when the
	// required config isn't satisfiable yet, mint a TOKEN_SECRET if needed, then
	// push every set value into the environment — but only where env didn't
	// already define it, so a real env var always wins. This is ALWAYS attempted
	// but a no-op on a managed/cloud host: with complete env, NeedsWizard is
	// false, EnsureSecret does nothing (env has the secret), nothing is saved,
	// and Apply overwrites nothing. It also never creates a data dir — DataDir
	// only resolves a path; only Save (never reached here when env is complete)
	// would create one.
	if dataDirErr != nil {
		logx.Warn("no data directory (%v); using environment only", dataDirErr)
	} else {
		fc, ferr := setup.Load(dataDir)
		if ferr != nil {
			return nil, fmt.Errorf("loading local config: %w", ferr)
		}
		if setup.NeedsWizard(fc) {
			if console.Interactive(os.Stdin) {
				setup.Wizard(fc, dataDir)
				setup.EnsureSecret(fc)
				if err := fc.Save(dataDir); err != nil {
					return nil, fmt.Errorf("saving local config: %w", err)
				}
				logx.Info("configuration saved to %s", filepath.Join(dataDir, "config.json"))
			} else {
				return nil, errors.New("configuration incomplete — run `bombers setup` in a terminal, or set the required environment variables")
			}
		} else {
			// Config is already complete; still mint + persist a secret if one
			// was never generated, so tokens survive restarts. On a managed host
			// the env already supplies TOKEN_SECRET, so EnsureSecret is a no-op
			// and nothing is written.
			had := fc.TokenSecret != ""
			setup.EnsureSecret(fc)
			if !had && fc.TokenSecret != "" {
				_ = fc.Save(dataDir) // persist a freshly-minted secret
			}
		}
		fc.Apply()
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	logx.Info("config loaded (.env)")

	// Database. Two backends behind the same pool: an external DATABASE_URL (the
	// managed default, and a valid local pick too) or an embedded Postgres the
	// server runs itself for local self-host (LOCAL_MODE.md §6). Only the
	// embedded path auto-runs migrations; external keeps today's behavior — the
	// operator runs goose deliberately.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var pool *pgxpool.Pool
	var epg *embeddedpg.Instance
	if cfg.DBBackend == "embedded" {
		if dataDirErr != nil {
			cancel()
			return nil, fmt.Errorf("embedded Postgres needs a data directory: %w", dataDirErr)
		}
		var connStr string
		epg, connStr, err = embeddedpg.Start(dataDir)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("starting embedded Postgres: %w", err)
		}
		// Migrate BEFORE opening the app pool, so the first query hits a ready
		// schema. Its own timeout — a first run may apply the whole history from
		// scratch. A failure stops the embedded PG (it is already running) before
		// bailing so it doesn't hold its port for the next launch.
		mctx, mcancel := context.WithTimeout(context.Background(), 60*time.Second)
		if merr := migrate.Up(mctx, connStr); merr != nil {
			mcancel()
			_ = epg.Stop()
			cancel()
			return nil, fmt.Errorf("running migrations: %w", merr)
		}
		mcancel()
		// Starting + migrating the embedded Postgres can take far longer than the
		// 10s pool-connect budget (a first run downloads a binary and inits the
		// cluster), which would have drained the ctx above. Refresh it so the pool
		// gets its full connect window.
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		pool, err = store.NewPool(ctx, connStr)
	} else {
		pool, err = store.NewPool(ctx, cfg.DatabaseURL)
	}
	cancel()
	if err != nil {
		if epg != nil {
			_ = epg.Stop()
		}
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	logx.Info("db connected")

	// Media byte store (profile media) is part of the stack like Postgres is:
	// unreachable/unusable at startup → fatal, same as the DB. The backend is a
	// config pick — the local filesystem (self-host default) or S3-compatible
	// object storage (managed default) — both behind media.Store, so the
	// handlers and /health never see which one they got.
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	var storage media.Store
	if cfg.MediaBackend == "fs" {
		storage, err = media.NewFSStore(cfg.MediaDir)
	} else {
		storage, err = media.NewStorage(ctx, cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL)
	}
	cancel()
	if err != nil {
		// Both DB resources are already live by here; release them (pool first,
		// then embedded PG) before bailing, since the caller may keep the process
		// alive (the service logs the error and stops) rather than exiting.
		pool.Close()
		if epg != nil {
			_ = epg.Stop()
		}
		return nil, fmt.Errorf("initializing media store: %w", err)
	}
	if cfg.MediaBackend == "fs" {
		logx.Info("media store ready (filesystem: %s)", cfg.MediaDir)
	} else {
		logx.Info("media store ready (s3 bucket %q)", cfg.S3Bucket)
	}

	issuer := auth.NewIssuer(cfg.TokenSecret)
	authService := auth.NewService(issuer, pool)
	authHandler := auth.NewHandler(authService)
	usersHandler := users.NewHandler(pool, authService)
	friendsHandler := friends.NewHandler(pool)
	profilesHandler := profiles.NewHandler(pool)
	messagingHandler := messaging.NewHandler(pool)
	syncHandler := sync.NewHandler(pool)
	nodesHandler := nodes.NewHandler(pool)
	nodeshareHandler := nodeshare.NewHandler(pool)
	mediaHandler := media.NewHandler(pool, storage)

	r := chi.NewRouter()
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{cfg.CorsAllowedOrigin},
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowedHeaders: []string{"Authorization", "Content-Type"},
	}))
	r.Get("/health", healthHandler(pool, storage))
	r.Post("/auth/register", usersHandler.Register)
	r.Post("/auth/login", usersHandler.Login)
	r.Post("/auth/refresh", authHandler.Refresh)

	r.Group(func(r chi.Router) {
		r.Use(issuer.RequireAuth)
		r.Get("/me", usersHandler.Me)
		r.Get("/friends", friendsHandler.List)
		r.Get("/friends/code", friendsHandler.MyCode)
		r.Get("/friends/requests", friendsHandler.ListRequests)
		r.Post("/friends/requests", friendsHandler.SendRequest)
		r.Post("/friends/requests/{requesterID}/accept", friendsHandler.AcceptRequest)
		r.Post("/friends/requests/{requesterID}/reject", friendsHandler.RejectRequest)
		r.Delete("/friends/{userID}", friendsHandler.RemoveFriend)
		r.Post("/friends/{userID}/block", friendsHandler.Block)
		r.Post("/friends/{userID}/unblock", friendsHandler.Unblock)
		r.Get("/me/profile", profilesHandler.GetMine)
		r.Put("/me/profile", profilesHandler.UpdateMine)
		r.Get("/profiles/{userID}", profilesHandler.GetForUser)
		r.Get("/me/about", profilesHandler.ListMyAbout)
		r.Get("/me/about/{subjectID}", profilesHandler.GetMyAbout)
		r.Put("/me/about/{subjectID}", profilesHandler.UpsertMyAbout)
		r.Delete("/me/about/{subjectID}", profilesHandler.DeleteMyAbout)
		r.Get("/about/{authorID}", profilesHandler.GetSharedAbout)
		r.Post("/messages", messagingHandler.Send)
		// Static /messages/unread is registered before the /messages/{userID}
		// param route so chi matches it first (it would anyway — static beats
		// wildcard — but keep them together).
		r.Get("/messages/unread", messagingHandler.Unread)
		r.Get("/messages/{userID}", messagingHandler.History)
		r.Post("/messages/{userID}/read", messagingHandler.MarkRead)
		r.Post("/sync/push", syncHandler.Push)
		r.Get("/sync/pull", syncHandler.Pull)
		r.Get("/sync/status", syncHandler.Status)
		// The official node store: operator-published SDK bundles. Any authed
		// user browses + installs; publishing is console-only (no HTTP publish).
		r.Get("/nodes", nodesHandler.List)
		r.Get("/nodes/{id}/bundle", nodesHandler.Download)
		// Friend node-sharing (the clone model): a one-way transfer inbox,
		// friend-gated — separate from the public node-store catalog above.
		r.Post("/nodes/send", nodeshareHandler.Send)
		r.Get("/nodes/received", nodeshareHandler.ListReceived)
		r.Delete("/nodes/received/{id}", nodeshareHandler.Dismiss)
		// Profile media (avatar/banner): raw image bytes in, proxied bytes
		// out. Serving is friendship + profile-visibility gated pass-through.
		r.Put("/me/media/{kind}", mediaHandler.Upload)
		r.Delete("/me/media/{kind}", mediaHandler.Delete)
		r.Get("/media/{userID}/{kind}", mediaHandler.Serve)
	})

	startedAt := time.Now()
	srv := &http.Server{Addr: cfg.Host + ":" + cfg.Port, Handler: r}
	// Log the listen line from the calling goroutine, before the console prints
	// its prompt — otherwise this fires from the serve goroutine a beat later and
	// lands on the "bombers> " line, eating the prompt.
	logx.Info("http listening on %s", srv.Addr)
	// Show the actual URL(s) a client can reach this on — for a LAN bind that's
	// each of the machine's IPs, which is what a self-hoster hands the client.
	for _, u := range console.ReachableURLs(cfg.Host, cfg.Port) {
		logx.Info("reachable at %s", u)
	}
	go func() {
		// A serve error at startup (e.g. port in use) is fatal in both modes;
		// ErrServerClosed is the normal graceful-shutdown exit.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logx.Fatal("http server: %v", err)
		}
	}()

	return &app{srv: srv, pool: pool, storage: storage, epg: epg, startedAt: startedAt, host: cfg.Host, port: cfg.Port}, nil
}

// shutdown performs the graceful teardown in the fixed order the server has
// always used: stop accepting/serving HTTP (bounded), then close the app pool,
// then stop the embedded Postgres (if any) — the pool must close before Postgres
// does so no query races the stop. Same log lines as before.
func (a *app) shutdown() {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.srv.Shutdown(shutdownCtx); err != nil {
		logx.Error("graceful shutdown: %v", err)
	}
	logx.Info("server stopped")

	a.pool.Close()

	if a.epg != nil {
		if serr := a.epg.Stop(); serr != nil {
			logx.Error("stopping embedded Postgres: %v", serr)
		} else {
			logx.Info("embedded Postgres stopped")
		}
	}
}

// runStart runs the HTTP server on the interactive/CLI path. It parses
// --headless, brings up the logger + banner, builds and serves via
// buildAndServe (Fatal on setup error, exactly as before), then waits for the
// console or a signal to stop and tears down. This is the default command, so a
// bare `bombers` (in a terminal) dispatches here.
func runStart(args []string) {
	flags := flag.NewFlagSet("start", flag.ExitOnError)
	headless := flags.Bool("headless", false,
		"serve without the interactive admin console (stop with SIGINT/SIGTERM)")
	_ = flags.Parse(args)

	// Load .env before configuring the logger so LOG_TIME_FORMAT / NO_COLOR from
	// the file take effect; hold any real load error until logx is up to report
	// it in the leveled format.
	envErr := godotenv.Load()
	logx.Init()
	if envErr != nil && !errors.Is(envErr, fs.ErrNotExist) {
		logx.Fatal("loading .env: %v", envErr)
	}

	// The banner is decorative — print it on a real terminal only so piped or
	// redirected logs stay clean.
	if logx.Interactive() {
		printBanner(os.Stdout)
	}

	a, err := buildAndServe()
	if err != nil {
		logx.Fatal("%v", err)
	}

	// DEFAULT: the interactive operator console (Minecraft-style) on stdin.
	// --headless skips it, and so does a non-TTY stdin (service managers,
	// `< /dev/null`) — those wait for SIGINT/SIGTERM like a normal daemon. A
	// console that hits EOF also falls back to signal-waiting rather than
	// exiting or spinning.
	stopped := false
	if !*headless && console.Interactive(os.Stdin) {
		logx.Info(`console ready — type "help", "stop" to quit`)
		stopped = console.New(a.pool, a.storage, a.startedAt, a.host, a.port).Run()
		if !stopped {
			logx.Warn("console input ended; running headless (SIGINT/SIGTERM to stop)")
		}
	}
	if !stopped {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		s := <-sig
		logx.Info("received %s, shutting down", s)
	}

	a.shutdown()
}

// shutdownWait bounds how long the service's Stop waits for the serve goroutine
// to finish its graceful shutdown before returning to the service manager
// anyway. It comfortably covers the 10s HTTP-shutdown budget plus closing the
// pool and stopping the embedded Postgres.
const shutdownWait = 20 * time.Second

// program adapts the server to kardianos/service. The OS service manager calls
// Start (which must return fast) and later Stop; the two channels hand control
// between the manager's goroutine and the serve goroutine.
type program struct {
	stop chan struct{} // closed by Stop to request shutdown
	done chan struct{} // closed by run when shutdown (or a failed start) completes
}

func newProgram() *program {
	return &program{stop: make(chan struct{}), done: make(chan struct{})}
}

// Start returns immediately: the Windows SCM kills a service whose Start blocks,
// and a first-run embedded-Postgres download can take a while. All real work
// happens in the goroutine.
func (p *program) Start(s service.Service) error {
	go p.run(s)
	return nil
}

// run brings up the logger, builds and serves, then blocks until Stop asks it
// to shut down. On a build failure there is nothing to serve, so it reports the
// error and asks the service manager to move us to the stopped state.
func (p *program) run(s service.Service) {
	// A service has no TTY: load .env and initialise the logger here, since
	// buildAndServe logs through logx (it auto-detects the non-interactive stream
	// — no banner, no color).
	_ = godotenv.Load()
	logx.Init()

	a, err := buildAndServe()
	if err != nil {
		logx.Error("startup failed: %v", err)
		close(p.done)
		// Nothing is serving — ask the OS service manager to transition us to
		// stopped (best effort). This drives Stop, whose done-channel guard makes
		// the extra call harmless.
		_ = s.Stop()
		return
	}

	<-p.stop
	a.shutdown()
	close(p.done)
}

// Stop requests shutdown and waits (bounded) for it to actually finish, so the
// service manager doesn't consider us stopped until graceful shutdown ran. If
// run already finished (a failed start bailed out), there is nothing to signal.
func (p *program) Stop(s service.Service) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	close(p.stop)
	select {
	case <-p.done:
	case <-time.After(shutdownWait):
		logx.Warn("graceful shutdown timed out after %s", shutdownWait)
	}
	return nil
}

// runService manages the OS background service: install / start / stop /
// restart / status / uninstall. install and uninstall register/remove a system
// service and need an elevated terminal (Administrator on Windows, root/sudo on
// Linux/macOS); svc.Control surfaces that hint when the manager refuses.
func runService(s service.Service, args []string) {
	logx.Init()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: bombers service <install|start|stop|restart|status|uninstall>")
		os.Exit(2)
	}
	action := args[0]
	switch action {
	case "install", "start", "stop", "restart", "status", "uninstall":
	default:
		fmt.Fprintf(os.Stderr, "unknown service action %q\n", action)
		fmt.Fprintln(os.Stderr, "valid actions: install, start, stop, restart, status, uninstall")
		os.Exit(2)
	}
	if err := svc.Control(s, action); err != nil {
		logx.Fatal("%v", err)
	}
	if action != "status" {
		logx.Info("service %s: ok", action)
	}
}

// runUninstall is the full local teardown — the counterpart to install. It
// removes the OS service if present (best effort: not-installed/permission
// problems are reported, never fatal) and then, after an explicit confirmation,
// deletes the entire data directory (config.json, filesystem media, embedded-
// Postgres data + cached binaries). --yes skips the prompt for scripts. It
// never touches anything outside the data dir.
func runUninstall(s service.Service, args []string) {
	flags := flag.NewFlagSet("uninstall", flag.ExitOnError)
	yes := flags.Bool("yes", false, "skip the confirmation prompt (for scripts)")
	_ = flags.Parse(args)

	logx.Init()

	// 1. Best-effort removal of the OS service. Stop first, then uninstall; a
	// missing service or a privilege problem is reported but must NOT abort the
	// data teardown below.
	if serr := svc.Control(s, "stop"); serr != nil {
		logx.Info("service stop skipped: %v", serr)
	}
	if uerr := svc.Control(s, "uninstall"); uerr != nil {
		logx.Warn("service uninstall skipped: %v", uerr)
	} else {
		logx.Info("OS service removed")
	}

	// 2. Data directory teardown.
	dir, err := setup.DataDir()
	if err != nil {
		logx.Fatal("resolving data dir: %v", err)
	}
	if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
		logx.Info("no data directory at %s — nothing to delete", dir)
		return
	} else if statErr != nil {
		logx.Fatal("checking data dir: %v", statErr)
	}

	fmt.Println()
	fmt.Println("This will permanently delete the Bombers data directory:")
	fmt.Println()
	fmt.Printf("    %s\n", dir)
	fmt.Println()
	fmt.Println("That removes your local config.json, filesystem media, and the embedded")
	fmt.Println("Postgres data + cached binaries — everything under that folder.")
	fmt.Println()

	if !*yes {
		if !console.Interactive(os.Stdin) {
			logx.Fatal("refusing to delete without confirmation — re-run with --yes to proceed non-interactively")
		}
		fmt.Print("Delete it? Type y to confirm [y/N]: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			fmt.Println("Aborted — nothing was deleted.")
			return
		}
	}

	if err := os.RemoveAll(dir); err != nil {
		logx.Fatal("deleting data dir: %v", err)
	}
	logx.Info("deleted %s", dir)
	fmt.Println("Bombers data removed.")
}

// runSetup runs the local self-host wizard unconditionally — the explicit
// reconfigure path. It starts from any existing config so it edits rather than
// resets, mints a TOKEN_SECRET if missing, persists the result, and returns
// WITHOUT serving. `bombers start` then launches with the saved config.
func runSetup(_ []string) {
	_ = godotenv.Load()
	logx.Init()

	dir, err := setup.DataDir()
	if err != nil {
		logx.Fatal("resolving data dir: %v", err)
	}
	fc, _ := setup.Load(dir)
	setup.Wizard(fc, dir)
	setup.EnsureSecret(fc)
	if err := fc.Save(dir); err != nil {
		logx.Fatal("saving local config: %v", err)
	}
	printSetupSummary(fc, dir)
}

// printSetupSummary prints the "Setup complete!" recap after the wizard saves,
// straight to stdout via fmt (not logx — this is a user-facing report, not a log
// line). It restates where each piece landed (config file, database, media),
// the URL(s) the server will answer on, and the next commands to run. Modeled on
// the Hermes setup-complete screen: plain, aligned, skimmable.
func printSetupSummary(fc *setup.FileConfig, dir string) {
	dbLine := "external → " + fc.DatabaseURL
	if fc.DBBackend == "embedded" {
		dbLine = "embedded Postgres → " + filepath.Join(dir, "pg")
	}

	mediaLine := "S3/MinIO → " + fc.S3Endpoint
	if fc.MediaBackend == "fs" {
		mediaDir := fc.MediaDir
		if mediaDir == "" {
			mediaDir = filepath.Join(dir, "media")
		}
		mediaLine = "local files → " + mediaDir
	}

	fmt.Println()
	fmt.Println("✔ Setup complete!")
	fmt.Println()
	fmt.Println("Your files:")
	fmt.Printf("  %-10s%s\n", "Config:", filepath.Join(dir, "config.json"))
	fmt.Printf("  %-10s%s\n", "Database:", dbLine)
	fmt.Printf("  %-10s%s\n", "Media:", mediaLine)
	fmt.Println()
	fmt.Println("Reachable at:")
	for _, u := range console.ReachableURLs(fc.Host, fc.Port) {
		fmt.Printf("  %s\n", u)
	}
	fmt.Println()
	fmt.Println("Next:")
	fmt.Printf("  %-16s%s\n", "bombers start", "run the server")
	fmt.Printf("  %-16s%s\n", "bombers doctor", "check for issues")
	fmt.Printf("  %-16s%s\n", "bombers setup", "re-run this wizard")
	fmt.Println()
}

// Doctor status markers — fixed-width and ASCII-safe so the checklist stays
// aligned in any terminal and survives piping/redirection (no color, no
// Unicode). Every check prints exactly one of these.
const (
	markOK   = "[ ok ]"
	markFAIL = "[FAIL]"
	markWarn = "[warn]"
	markSkip = "[skip]"
)

// runDoctor diagnoses a local self-host install without starting anything. It
// resolves configuration the same way `start` does — layer the saved local
// config UNDER the environment, then config.Load — but NEVER fatals: every step
// is best-effort and every check prints a single status line. It exits 1 when
// any check FAILs (so `bombers doctor && bombers start` gates a launch on a
// clean bill of health); warnings and skips alone never fail.
func runDoctor(_ []string) {
	_ = godotenv.Load()
	logx.Init()

	// Layer the saved local config under the environment exactly like
	// buildAndServe, but tolerate every failure — doctor reports problems, it
	// doesn't die on them. A managed/pure-env host simply has nothing to load.
	dir, dirErr := setup.DataDir()
	if dirErr == nil {
		if fc, ferr := setup.Load(dir); ferr == nil {
			setup.EnsureSecret(fc)
			fc.Apply()
		}
	}

	cfg, cfgErr := config.Load()

	var okCount, failCount int
	mark := func(status, name, msg string) {
		fmt.Printf("%s  %-14s  %s\n", status, name, msg)
		switch status {
		case markOK:
			okCount++
		case markFAIL:
			failCount++
		}
	}

	fmt.Println("Bombers doctor — checking your local setup")
	fmt.Println()

	// 1. Data directory resolves and is writable (create + remove a temp file).
	if dirErr != nil {
		mark(markFAIL, "Data directory", fmt.Sprintf("cannot resolve: %v", dirErr))
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		mark(markFAIL, "Data directory", fmt.Sprintf("cannot create %s: %v", dir, err))
	} else if f, err := os.CreateTemp(dir, ".doctor-*"); err != nil {
		mark(markFAIL, "Data directory", fmt.Sprintf("%s is not writable: %v", dir, err))
	} else {
		tmp := f.Name()
		_ = f.Close()
		_ = os.Remove(tmp)
		mark(markOK, "Data directory", fmt.Sprintf("writable: %s", dir))
	}

	// 2. Configuration loads (all required env/file vars are satisfiable).
	if cfgErr != nil {
		mark(markFAIL, "Configuration", cfgErr.Error())
	} else {
		mark(markOK, "Configuration", "all required settings present")
	}

	// 3. Build architecture vs. the effective DB backend: embedded Postgres has
	// no 32-bit (386) binary upstream, so a 386 build can never run it.
	dbBackend := effectiveDBBackend(cfg)
	switch {
	case dbBackend == "embedded" && runtime.GOARCH == "386":
		mark(markFAIL, "Architecture", "embedded Postgres needs a 64-bit build (this is 32-bit); rebuild with `GOARCH=amd64 go build`, or use external Postgres.")
	case dbBackend == "embedded":
		mark(markOK, "Architecture", fmt.Sprintf("64-bit (%s) — embedded Postgres supported", runtime.GOARCH))
	default:
		mark(markSkip, "Architecture", "external database — no architecture constraint")
	}

	// 4. Port is bindable right now (a rough "is the server already running?"
	// probe): listen, then immediately close on success.
	if cfg == nil {
		mark(markSkip, "Port", "config didn't load")
	} else {
		addr := cfg.Host + ":" + cfg.Port
		if ln, err := net.Listen("tcp", addr); err != nil {
			mark(markFAIL, "Port", fmt.Sprintf("cannot bind %s: %v (already in use?)", addr, err))
		} else {
			_ = ln.Close()
			mark(markOK, "Port", fmt.Sprintf("%s is free", addr))
		}
	}

	// 5. Database reachability. External is dialed here (short ctx, ping, close);
	// embedded is not started by doctor — it comes up with `bombers start`.
	switch {
	case dbBackend == "embedded":
		mark(markSkip, "Database", "embedded — starts with `bombers start`")
	case cfg == nil:
		mark(markSkip, "Database", "config didn't load")
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pool, err := store.NewPool(ctx, cfg.DatabaseURL)
		if err != nil {
			cancel()
			mark(markFAIL, "Database", fmt.Sprintf("cannot connect: %v", err))
		} else {
			perr := pool.Ping(ctx)
			cancel()
			pool.Close()
			if perr != nil {
				mark(markFAIL, "Database", fmt.Sprintf("ping failed: %v", perr))
			} else {
				mark(markOK, "Database", "reachable")
			}
		}
	}

	// 6. Media backend: a filesystem root must be creatable/writable; an S3/MinIO
	// backend must answer a reachability probe.
	switch {
	case cfg == nil:
		mark(markSkip, "Media", "config didn't load")
	case cfg.MediaBackend == "fs":
		mediaDir := cfg.MediaDir
		if mediaDir == "" && dir != "" {
			mediaDir = filepath.Join(dir, "media")
		}
		if mediaDir == "" {
			mark(markSkip, "Media", "no media directory resolved")
		} else if err := os.MkdirAll(mediaDir, 0o755); err != nil {
			mark(markFAIL, "Media", fmt.Sprintf("cannot create %s: %v", mediaDir, err))
		} else if f, err := os.CreateTemp(mediaDir, ".doctor-*"); err != nil {
			mark(markFAIL, "Media", fmt.Sprintf("%s is not writable: %v", mediaDir, err))
		} else {
			tmp := f.Name()
			_ = f.Close()
			_ = os.Remove(tmp)
			mark(markOK, "Media", fmt.Sprintf("filesystem writable: %s", mediaDir))
		}
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		storage, err := media.NewStorage(ctx, cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL)
		if err != nil {
			cancel()
			mark(markFAIL, "Media", fmt.Sprintf("S3/MinIO unreachable: %v", err))
		} else {
			perr := storage.Ping(ctx)
			cancel()
			if perr != nil {
				mark(markFAIL, "Media", fmt.Sprintf("S3/MinIO ping failed: %v", perr))
			} else {
				mark(markOK, "Media", fmt.Sprintf("S3/MinIO reachable: %s", cfg.S3Endpoint))
			}
		}
	}

	fmt.Println()
	fmt.Printf("%d checks OK, %d issue(s)\n", okCount, failCount)
	if failCount > 0 {
		os.Exit(1)
	}
}

// effectiveDBBackend resolves which database backend doctor should test: an
// explicit DB_BACKEND env var wins, then the loaded config's value, then the
// managed default ("external"). Safe on a nil cfg (config.Load failed).
func effectiveDBBackend(cfg *config.Config) string {
	if b := os.Getenv("DB_BACKEND"); b != "" {
		return b
	}
	if cfg != nil && cfg.DBBackend != "" {
		return cfg.DBBackend
	}
	return "external"
}

func healthHandler(pool *pgxpool.Pool, storage media.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		// The DB governs status and the HTTP code (unchanged contract: clients
		// gate login on it). The media field is informational — object storage
		// down degrades only the media endpoints, not the core product.
		mediaState := "up"
		if err := storage.Ping(ctx); err != nil {
			mediaState = "down"
		}

		w.Header().Set("Content-Type", "application/json")
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"degraded","db":"down","media":"` + mediaState + `"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","db":"up","media":"` + mediaState + `"}`))
	}
}
