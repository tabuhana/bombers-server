package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tabuhana/bombers-server/internal/admin"
)

// Operator commands that CHANGE a user: ban, unban, delete.
//
// The console is local-operator-privileged by definition — whoever holds the
// terminal on the machine running the server. That's why these live here and
// not behind an HTTP endpoint: there is no admin role, no admin API to secure,
// and no way to reach them from a client.
//
// Ban is reversible and preferred; delete is not, and takes the user's content
// with it, so it asks first.

// resolveUser finds a user by username OR id, so the operator can type whichever
// they have in front of them. An ambiguous or missing name is an error, never a
// guess — these commands are destructive.
func resolveUser(ctx context.Context, c *Console, needle string) (id, username string, err error) {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return "", "", errors.New("which user? give a username or id")
	}
	row := c.pool.QueryRow(ctx,
		`SELECT id, username FROM users WHERE username = $1 OR id = $1`, needle)
	if err := row.Scan(&id, &username); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", fmt.Errorf("no user %q", needle)
		}
		return "", "", fmt.Errorf("look up user: %w", err)
	}
	return id, username, nil
}

func runBan(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: ban <username|id> [reason]")
		return nil
	}
	id, username, err := resolveUser(ctx, c, args[0])
	if err != nil {
		return err
	}
	reason := strings.TrimSpace(strings.Join(args[1:], " "))

	tag, err := c.pool.Exec(ctx,
		`UPDATE users SET banned_at = NOW(), ban_reason = $2 WHERE id = $1`, id, reason)
	if err != nil {
		return fmt.Errorf("ban user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no user %q", args[0])
	}

	// Kill their refresh tokens so the ban can't be ridden out by a client that
	// already holds one; the access token they may be holding expires within its
	// own short lifetime.
	if _, err := c.pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE user_id = $1`, id); err != nil {
		fmt.Fprintf(c.out, "  (warning: could not clear sessions: %v)\n", err)
	}

	fmt.Fprintf(c.out, "  banned %s\n", username)
	if reason != "" {
		fmt.Fprintf(c.out, "  reason: %s\n", reason)
	}
	fmt.Fprintln(c.out, "  they can't log in or refresh; any live session ends within 15 minutes")
	return nil
}

func runUnban(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: unban <username|id>")
		return nil
	}
	id, username, err := resolveUser(ctx, c, args[0])
	if err != nil {
		return err
	}
	if _, err := c.pool.Exec(ctx,
		`UPDATE users SET banned_at = NULL, ban_reason = '' WHERE id = $1`, id); err != nil {
		return fmt.Errorf("unban user: %w", err)
	}
	fmt.Fprintf(c.out, "  unbanned %s — they can log in again\n", username)
	return nil
}

func runDeleteUser(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: deluser <username|id>")
		fmt.Fprintln(c.out, "  (consider `ban` instead — it's reversible and keeps their content intact)")
		return nil
	}
	id, username, err := resolveUser(ctx, c, args[0])
	if err != nil {
		return err
	}

	// Say plainly what goes with them. Every cross-user table cascades on the
	// user FK, so this is genuinely irreversible.
	fmt.Fprintf(c.out, "  Delete %s (%s)?\n", username, id)
	fmt.Fprintln(c.out, "  This removes their account, profile, media, messages, friendships,")
	fmt.Fprintln(c.out, "  published items and node transfers. It cannot be undone.")
	if !c.confirm("  type the username to confirm: ", username) {
		fmt.Fprintln(c.out, "  cancelled")
		return nil
	}

	tag, err := c.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no user %q", args[0])
	}
	fmt.Fprintf(c.out, "  deleted %s\n", username)
	return nil
}

// runBanned lists who is currently banned and why.
func runBanned(ctx context.Context, c *Console, _ []string) error {
	rows, err := c.pool.Query(ctx,
		`SELECT username, banned_at, ban_reason FROM users WHERE banned_at IS NOT NULL ORDER BY banned_at`)
	if err != nil {
		return fmt.Errorf("list bans: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var username, reason string
		var at time.Time
		if err := rows.Scan(&username, &at, &reason); err != nil {
			return fmt.Errorf("scan ban: %w", err)
		}
		line := fmt.Sprintf("  %-32s %s", username, at.Format("2006-01-02 15:04"))
		if reason != "" {
			line += "  " + reason
		}
		fmt.Fprintln(c.out, line)
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate bans: %w", err)
	}
	if count == 0 {
		fmt.Fprintln(c.out, "  nobody is banned")
	}
	return nil
}

// The ADMIN ROLE commands. The console is the root of trust for admin: it is
// privileged by physical access, so granting the role here needs no auth and
// there is deliberately no HTTP path to self-promotion. (ADMIN_USERNAME at boot
// is the other way in — see internal/admin.)
//
// Admin is what lets operator actions be driven from the CLIENT: an admin can
// publish to the node store over the API instead of copying a bundle onto this
// machine.

func runAdmin(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: admin <username|id>")
		return nil
	}
	username, err := admin.Set(ctx, c.pool, args[0], true)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%s is now an admin\n", username)
	return nil
}

func runUnadmin(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: unadmin <username|id>")
		return nil
	}
	username, err := admin.Set(ctx, c.pool, args[0], false)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%s is no longer an admin\n", username)
	return nil
}

func runAdmins(ctx context.Context, c *Console, _ []string) error {
	rows, err := c.pool.Query(ctx,
		`SELECT username, created_at FROM users WHERE is_admin ORDER BY username`)
	if err != nil {
		return fmt.Errorf("list admins: %w", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var username string
		var created time.Time
		if err := rows.Scan(&username, &created); err != nil {
			return fmt.Errorf("read admin row: %w", err)
		}
		fmt.Fprintf(c.out, "  %-20s  joined %s\n", username, created.Format("2006-01-02"))
		n++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list admins: %w", err)
	}
	if n == 0 {
		fmt.Fprintln(c.out, "  no admins yet — `admin <username>` grants it")
	}
	return nil
}
