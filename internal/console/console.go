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

// Console owns the interactive loop: a small registry of commands dispatched
// over lines read from stdin.
type Console struct {
	pool      *pgxpool.Pool
	startedAt time.Time
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

func New(pool *pgxpool.Pool, startedAt time.Time) *Console {
	return &Console{
		pool:      pool,
		startedAt: startedAt,
		in:        os.Stdin,
		out:       os.Stdout,
		commands:  builtins(),
	}
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
	fmt.Fprintln(c.out, `Bombers server console — "help" for commands, "stop" to shut down.`)
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
