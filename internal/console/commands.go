package console

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The starter command set: read-only introspection plus stop. Anything that
// mutates (delete user, promote admin) waits for the admin-role follow-up.
func builtins() []command {
	return []command{
		{name: "help", help: "list available commands", run: runHelp},
		{name: "users", help: "list registered users (username, id, created)", run: runUsers},
		{name: "status", help: "uptime, DB ping, and row counts", run: runStatus},
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

func runStatus(ctx context.Context, c *Console, _ []string) error {
	fmt.Fprintf(c.out, "  uptime:  %s\n", time.Since(c.startedAt).Round(time.Second))
	if err := c.pool.Ping(ctx); err != nil {
		fmt.Fprintf(c.out, "  db:      down (%v)\n", err)
		return nil
	}
	fmt.Fprintln(c.out, "  db:      up")

	// Row counts are best-effort color — one failing table (e.g. a migration
	// not yet applied) shouldn't kill the rest of status.
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

func runStop(_ context.Context, _ *Console, _ []string) error {
	return errStop
}
