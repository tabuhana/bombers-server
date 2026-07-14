// Package console is the server's interactive admin surface: a Minecraft-style
// stdin command loop the binary runs by default (skip it with --headless).
// Whoever is at the local terminal is the operator by definition — there is no
// auth here. The built-ins are read-only introspection, `stop`, and the node
// store's operator-publish commands (`publish`/`unpublish`/`store` — the
// console is the store's ONLY publish path; no HTTP endpoint). Destructive or
// admin-role commands (delete user, promote admin, an is_admin column) are a
// deliberate LATER follow-up, not part of this loop yet.
package console

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// How long any single console command may hold a DB connection.
const commandTimeout = 5 * time.Second

// errStop is the sentinel a command returns to end the loop and shut the
// server down; it keeps the registry uniform (every command is just a run fn).
var errStop = errors.New("stop requested")

// objectStore is the narrow slice of the media storage the console needs: a
// reachability probe for the `status` command. Keeping it an interface leaves
// the console decoupled from the media package (and trivially stubbable).
type objectStore interface {
	Ping(ctx context.Context) error
}

// Console owns the interactive loop: a small registry of commands dispatched
// over lines read from stdin.
type Console struct {
	pool      *pgxpool.Pool
	media     objectStore
	startedAt time.Time
	host      string
	port      string
	in        io.Reader
	out       io.Writer
	commands  []command
}

type command struct {
	name    string
	aliases []string
	help    string
	run     func(ctx context.Context, c *Console, args []string) error
}

func New(pool *pgxpool.Pool, media objectStore, startedAt time.Time, host, port string) *Console {
	return &Console{
		pool:      pool,
		media:     media,
		startedAt: startedAt,
		host:      host,
		port:      port,
		in:        os.Stdin,
		out:       os.Stdout,
		commands:  builtins(),
	}
}

// ReachableURLs lists the base URLs this server answers on, given its bind host
// and port. A wildcard bind ("" or 0.0.0.0 / ::) is expanded to localhost plus
// every non-loopback IPv4 the machine has — so a LAN self-hoster sees the exact
// address to hand a client — while a concrete host is returned as-is. Shared by
// the startup log and the console `address` command.
func ReachableURLs(host, port string) []string {
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{fmt.Sprintf("http://%s:%s", host, port)}
	}
	urls := []string{fmt.Sprintf("http://localhost:%s", port)}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return urls
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			urls = append(urls, fmt.Sprintf("http://%s:%s", ip4, port))
		}
	}
	return urls
}

// Interactive reports whether f looks like an interactive terminal (a
// character device). Piped, redirected, or absent stdin is not — the caller
// should run headless instead of spinning on an instant EOF (e.g. under a
// service manager or `< /dev/null`).
func Interactive(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Run reads and dispatches commands until the operator asks to stop (returns
// true) or stdin ends/fails (returns false — the caller should fall back to
// signal-waiting and keep serving, exactly like --headless).
func (c *Console) Run() bool {
	scanner := bufio.NewScanner(c.in)
	for {
		fmt.Fprint(c.out, "bombers> ")
		if !scanner.Scan() {
			fmt.Fprintln(c.out)
			return false
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		cmd := c.lookup(fields[0])
		if cmd == nil {
			fmt.Fprintf(c.out, "unknown command %q — try \"help\"\n", fields[0])
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		err := cmd.run(ctx, c, fields[1:])
		cancel()
		if errors.Is(err, errStop) {
			return true
		}
		if err != nil {
			fmt.Fprintf(c.out, "error: %v\n", err)
		}
	}
}

func (c *Console) lookup(name string) *command {
	name = strings.ToLower(name)
	for i := range c.commands {
		cmd := &c.commands[i]
		if cmd.name == name {
			return cmd
		}
		for _, alias := range cmd.aliases {
			if alias == name {
				return cmd
			}
		}
	}
	return nil
}
