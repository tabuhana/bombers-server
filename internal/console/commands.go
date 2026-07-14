package console

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/nodes"
)

// The command set: read-only introspection, stop, and the NODE STORE's
// operator-publish surface (publish/unpublish/store) — the console IS the
// store's admin path, a deliberate step past read-only (there is no HTTP
// publish endpoint and no admin role yet). Anything else that mutates
// (delete user, promote admin) still waits for the admin-role follow-up.
func builtins() []command {
	return []command{
		{name: "help", help: "list available commands", run: runHelp},
		{name: "users", help: "list registered users (username, id, created)", run: runUsers},
		{name: "status", help: "uptime, DB + media ping, and row counts", run: runStatus},
		{name: "address", aliases: []string{"addr", "url"}, help: "the URL(s) this server is reachable at (hand one to a client)", run: runAddress},
		{name: "logtime", help: "show or set the log timestamp format (time|datetime|iso)", run: runLogtime},
		{name: "store", help: "list store-published nodes (id, name, version)", run: runStore},
		{name: "publish", help: "publish <path> — put a {manifest, files} JSON bundle in the node store", run: runPublish},
		{name: "unpublish", help: "unpublish <id> — remove a node from the store", run: runUnpublish},
		{name: "stop", aliases: []string{"quit", "exit"}, help: "gracefully shut the server down", run: runStop},
	}
}

func runHelp(_ context.Context, c *Console, _ []string) error {
	for _, cmd := range c.commands {
		name := cmd.name
		if len(cmd.aliases) > 0 {
			name += " (" + strings.Join(cmd.aliases, ", ") + ")"
		}
		fmt.Fprintf(c.out, "  %-20s %s\n", name, cmd.help)
	}
	return nil
}

func runUsers(ctx context.Context, c *Console, _ []string) error {
	rows, err := c.pool.Query(ctx, `SELECT username, id, created_at FROM users ORDER BY created_at`)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var username, id string
		var created time.Time
		if err := rows.Scan(&username, &id, &created); err != nil {
			return fmt.Errorf("scan user: %w", err)
		}
		fmt.Fprintf(c.out, "  %-32s %s  %s\n", username, id, created.Format("2006-01-02 15:04"))
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate users: %w", err)
	}
	fmt.Fprintf(c.out, "%d user(s)\n", count)
	return nil
}

// runAddress prints the base URL(s) this server is reachable at — the thing a LAN
// self-hoster needs to point a client at it (the machine's IP, not just the bind).
func runAddress(_ context.Context, c *Console, _ []string) error {
	for _, u := range ReachableURLs(c.host, c.port) {
		fmt.Fprintf(c.out, "  %s\n", u)
	}
	return nil
}

func runStatus(ctx context.Context, c *Console, _ []string) error {
	fmt.Fprintf(c.out, "  uptime:  %s\n", time.Since(c.startedAt).Round(time.Second))
	fmt.Fprintf(c.out, "  address: %s\n", strings.Join(ReachableURLs(c.host, c.port), "  "))

	dbErr := c.pool.Ping(ctx)
	if dbErr != nil {
		fmt.Fprintf(c.out, "  db:      down (%v)\n", dbErr)
	} else {
		fmt.Fprintln(c.out, "  db:      up")
	}

	// Object storage — informational, mirrors the /health "media" signal. It's
	// independent of the DB, so it reports even when the DB is down.
	if c.media == nil {
		fmt.Fprintln(c.out, "  media:   n/a")
	} else if err := c.media.Ping(ctx); err != nil {
		fmt.Fprintf(c.out, "  media:   down (%v)\n", err)
	} else {
		fmt.Fprintln(c.out, "  media:   up")
	}

	// Row counts need the DB; skip just them (not the whole command) when it's
	// down. Best-effort otherwise — one failing table (e.g. a migration not yet
	// applied) shouldn't kill the rest of status.
	if dbErr != nil {
		return nil
	}
	for _, q := range []struct{ label, sql string }{
		{"users", `SELECT COUNT(*) FROM users`},
		{"friendships (accepted)", `SELECT COUNT(*) FROM friendships WHERE state = 'accepted'`},
		{"messages", `SELECT COUNT(*) FROM messages`},
		{"node transfers", `SELECT COUNT(*) FROM node_transfers`},
	} {
		var n int64
		if err := c.pool.QueryRow(ctx, q.sql).Scan(&n); err != nil {
			fmt.Fprintf(c.out, "  %-22s ? (%v)\n", q.label+":", err)
			continue
		}
		fmt.Fprintf(c.out, "  %-22s %d\n", q.label+":", n)
	}
	return nil
}

// runLogtime shows or switches the live log timestamp format. With no argument
// it prints the choices and the current one; with an argument it sets it (and
// the change is announced through the logger itself so you see the new format).
func runLogtime(_ context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  time · datetime · iso")
		fmt.Fprintf(c.out, "  current: %s\n", logx.TimeFormat())
		return nil
	}
	name := args[0]
	if err := logx.SetTimeFormat(name); err != nil {
		fmt.Fprintf(c.out, "unknown format %q — use time|datetime|iso\n", name)
		return nil
	}
	logx.Info("log time format → %s", name)
	return nil
}

func runStore(ctx context.Context, c *Console, _ []string) error {
	entries, err := nodes.Catalog(ctx, c.pool)
	if err != nil {
		return err
	}
	for _, e := range entries {
		version := e.Version
		if version == "" {
			version = "-"
		}
		fmt.Fprintf(c.out, "  %-28s %-32s v%s\n", e.ID, e.Name, version)
	}
	fmt.Fprintf(c.out, "%d node(s) published\n", len(entries))
	return nil
}

func runPublish(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: publish <path-to-bundle.json>  (a {manifest, files} JSON file)")
	}
	// Join so a path with spaces works without quoting.
	path := strings.Join(args, " ")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read bundle: %w", err)
	}
	info, err := nodes.PublishBundle(ctx, c.pool, raw)
	if err != nil {
		return err
	}
	version := info.Version
	if version == "" {
		version = "-"
	}
	fmt.Fprintf(c.out, "published %s (%s) v%s\n", info.ID, info.Name, version)
	return nil
}

func runUnpublish(ctx context.Context, c *Console, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: unpublish <node-id>")
	}
	removed, err := nodes.Unpublish(ctx, c.pool, args[0])
	if err != nil {
		return err
	}
	if !removed {
		fmt.Fprintf(c.out, "no node %q in the store\n", args[0])
		return nil
	}
	fmt.Fprintf(c.out, "unpublished %s\n", args[0])
	return nil
}

func runStop(_ context.Context, _ *Console, _ []string) error {
	return errStop
}
