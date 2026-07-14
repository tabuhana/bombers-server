// Package embeddedpg runs a real Postgres as a supervised child process for
// local self-host mode (LOCAL_MODE.md §6) via fergusstrange/embedded-postgres.
// No Docker, no system install: the library downloads a pinned Postgres binary
// once, caches it under the data dir, and boots it bound to localhost only. The
// managed/cloud path never imports this — it connects to an external
// DATABASE_URL instead, and so downloads nothing.
//
// Like logx it is a leaf-ish package: it may import logx but no domain package,
// so anything can pull it in without import cycles.
package embeddedpg

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/tabuhana/bombers-server/internal/logx"
)

const (
	// port is a Bombers-specific default that avoids clashing with a system
	// Postgres or the docker-compose Postgres, both of which use 5432.
	port = 5433

	// Fixed local credentials. The database is bound to loopback only, so these
	// never leave the machine — a stable known trio keeps the connection string
	// reproducible.
	user     = "bombers"
	password = "bombers"
	database = "bombers"
)

// Instance is an opaque handle to a running embedded Postgres. Callers hold it
// only to Stop the subprocess on shutdown; they never touch the library type.
type Instance struct {
	pg *embeddedpostgres.EmbeddedPostgres
}

// ConnString is the single source of truth for the embedded Postgres URL: the
// fixed loopback host, the Bombers-specific port, and the fixed local
// credentials, all from this package's consts. Start returns it, and the
// standalone `bombers console` uses it to reach a running server's embedded
// database without re-deriving the string (both must agree — hence one func).
func ConnString() string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", user, password, port, database)
}

// stopStalePostgres clears a leftover embedded Postgres left running by a
// previous run that didn't shut down cleanly (e.g. the terminal was closed,
// which orphans the child process and leaves it holding the port). It acts ONLY
// when our port is actually taken — so a stale postmaster.pid whose process is
// long gone is ignored and Start just proceeds. When the port IS held, it reads
// the postmaster.pid in our own data dir (so the PID is almost certainly our
// leftover Postgres), signals it to stop, and waits a moment for the port to free
// up. Best-effort throughout: on any doubt it does nothing and lets Start's own
// port check report the problem as before.
func stopStalePostgres(base string) {
	// Nothing to clean unless something is actually holding our port.
	if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
		_ = ln.Close()
		return
	}
	raw, err := os.ReadFile(filepath.Join(base, "data", "postmaster.pid"))
	if err != nil {
		return // no pid file to consult — leave it to Start's port check
	}
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)[0])
	pid, err := strconv.Atoi(first)
	if err != nil || pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	logx.Warn("a previous embedded Postgres (pid %d) is still holding port %d from an unclean shutdown — stopping it", pid, port)
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		_ = proc.Kill() // SIGTERM not deliverable (e.g. a stale/reused PID, or Windows) — force it
	}
	time.Sleep(1500 * time.Millisecond) // give Postgres a moment to release the port
}

// Start configures and boots a local Postgres under <dataDir>/pg, returning the
// running instance and the connection string the app pool + migrations use.
// Everything it needs is contained under that one directory:
//
//	<dataDir>/pg/data      the Postgres data directory (initialised once, then reused)
//	<dataDir>/pg/runtime   the extracted binary/runtime (re-extracted each start)
//	<dataDir>/pg/cache     the downloaded binary archive (fetched ONCE, then reused)
//
// The pinned major version keeps the on-disk data format reproducible across
// upgrades of this server. The very first Start downloads a one-time Postgres
// binary (the library verifies its SHA256 before use); every later start finds
// it cached under <dataDir>/pg/cache and boots straight away with no network.
func Start(dataDir string) (*Instance, string, error) {
	// Embedded Postgres downloads a prebuilt binary that must match this build's
	// CPU architecture, and no 32-bit (386) Postgres is published upstream — so a
	// 386 build can never start it. Fail fast with clear, actionable guidance
	// instead of the library's cryptic "no version found matching …" download error.
	if runtime.GOARCH == "386" {
		return nil, "", fmt.Errorf("embedded Postgres needs a 64-bit build (this binary is 32-bit / GOARCH=386) — " +
			"rebuild with `GOARCH=amd64 go build`, or choose \"use my own Postgres\" in `bombers setup`")
	}

	base := filepath.Join(dataDir, "pg")

	// Self-heal: clear a leftover Postgres from a previous run that didn't shut
	// down cleanly, so `bombers start` recovers instead of failing on "port
	// already in use".
	stopStalePostgres(base)

	cfg := embeddedpostgres.DefaultConfig().
		// Pin a specific major (Postgres 16) so the data directory format is
		// reproducible and later server upgrades don't silently move it.
		Version(embeddedpostgres.V16).
		Port(port).
		Username(user).
		Password(password).
		Database(database).
		DataPath(filepath.Join(base, "data")).
		RuntimePath(filepath.Join(base, "runtime")).
		// CachePath is what makes the binary download happen only ONCE: the
		// fetched archive lives here and every later Start reuses it.
		CachePath(filepath.Join(base, "cache")).
		// Route the subprocess's own output into logx so it shares the server's
		// single log stream instead of writing raw to stdout.
		Logger(logWriter{}).
		// Bind to loopback only — the embedded DB must never be reachable off
		// the box even when the API itself is exposed on the LAN
		// (LOCAL_MODE.md §10). initdb already defaults listen_addresses to
		// localhost; set it explicitly so a future library/default change can't
		// widen it, and match the value the library's own health check dials.
		StartParameters(map[string]string{"listen_addresses": "localhost"})

	pg := embeddedpostgres.NewDatabase(cfg)

	logx.Info("starting embedded Postgres 16 (first run downloads a one-time ~30-60 MB binary, then it is cached)...")
	if err := pg.Start(); err != nil {
		return nil, "", fmt.Errorf("start embedded postgres: %w", err)
	}
	connStr := ConnString()
	logx.Info("embedded Postgres listening on 127.0.0.1:%d", port)

	return &Instance{pg: pg}, connStr, nil
}

// Stop shuts the embedded Postgres down gracefully. It is safe on a nil or
// never-started instance (returns nil). The caller stops it AFTER closing the
// app's connection pool so no query races the shutdown.
func (i *Instance) Stop() error {
	if i == nil || i.pg == nil {
		return nil
	}
	return i.pg.Stop()
}

// logWriter is an io.Writer that forwards the Postgres subprocess's output to
// logx one line at a time, so PG's logs stay in the server's leveled, colored
// stream rather than printing raw (the rest of the server avoids the stdlib log
// package for the same reason).
type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	for line := range strings.SplitSeq(strings.TrimRight(string(p), "\n"), "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		// Postgres is chatty at startup (LOG: checkpoints, autovacuum, "ready to
		// accept connections", …). That routine noise clutters the server's log for
		// no benefit — bombers logs its own "listening" line — so forward only the
		// lines that flag a real problem and drop the rest.
		if strings.Contains(s, "WARNING") || strings.Contains(s, "ERROR") ||
			strings.Contains(s, "FATAL") || strings.Contains(s, "PANIC") {
			logx.Warn("postgres: %s", s)
		}
	}
	return len(p), nil
}
