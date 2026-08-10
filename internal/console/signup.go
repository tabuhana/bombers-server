package console

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tabuhana/bombers-server/internal/users"
)

// Who may sign up, who tried and couldn't, and who is refused outright.
//
// Discord is the only way into Bombers and the installer is only reachable
// after finishing on the website, so these commands are the whole of "who gets
// in". They live at the console because that's where the operator is, and
// because there is deliberately no HTTP route that widens access.

// discordIDArg takes the first argument as a Discord id. Discord ids are
// numeric snowflakes, and rejecting anything else here catches the common
// mistake — pasting a username, which would sit on the allowlist matching
// nobody, forever.
func discordIDArg(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("which Discord account? give the numeric user id")
	}
	id := strings.TrimSpace(args[0])
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", fmt.Errorf("%q isn't a Discord user id — it's the long number, "+
			"from Discord's Settings → Advanced → Developer Mode, then right-click a user → Copy User ID", id)
	}
	return id, nil
}

func runAllow(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: allow <discord-id> [who they are]")
		return nil
	}
	id, err := discordIDArg(args)
	if err != nil {
		return err
	}
	note := strings.TrimSpace(strings.Join(args[1:], " "))

	added, err := users.AddToAllowlist(ctx, c.pool, id, note)
	if err != nil {
		return err
	}
	if added {
		fmt.Fprintf(c.out, "  %s can now sign up\n", describeID(id, note))
	} else {
		fmt.Fprintf(c.out, "  %s was already allowed (note updated)\n", describeID(id, note))
	}

	// Being on the allowlist means nothing while blocked, and somebody just
	// typing `allow` deserves to know that rather than wondering later.
	if blocked, berr := users.IsBlocked(ctx, c.pool, id); berr == nil && blocked {
		fmt.Fprintln(c.out, "  ! they are BLOCKED, so this has no effect — `unblock` them first")
	}
	return nil
}

func runUnallow(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: unallow <discord-id>")
		return nil
	}
	id, err := discordIDArg(args)
	if err != nil {
		return err
	}
	removed, err := users.RemoveFromAllowlist(ctx, c.pool, id)
	if err != nil {
		return err
	}
	if !removed {
		fmt.Fprintf(c.out, "  %s wasn't on the allowlist\n", id)
		return nil
	}
	fmt.Fprintf(c.out, "  %s can no longer sign up\n", id)
	// The distinction matters: this is about creating an account, not about one
	// that exists. Somebody who's already in stays in.
	fmt.Fprintln(c.out, "  (an account they already have is untouched — use `ban` for that)")
	return nil
}

func runAllowed(ctx context.Context, c *Console, _ []string) error {
	list, err := users.ListAllowlist(ctx, c.pool)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(c.out, "  the allowlist is empty — nobody can sign up")
		fmt.Fprintln(c.out, "  add someone with `allow <discord-id> [who they are]`")
		return nil
	}
	for _, e := range list {
		fmt.Fprintf(c.out, "  %-20s %-24s %s\n", e.DiscordID, e.Note, e.AddedAt.Format(time.DateOnly))
	}
	fmt.Fprintf(c.out, "  %d allowed\n", len(list))
	return nil
}

func runAttempts(ctx context.Context, c *Console, args []string) error {
	limit := 25
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			limit = n
		}
	}
	list, err := users.ListSigninAttempts(ctx, c.pool, limit)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(c.out, "  nobody without an account has tried to sign in")
		return nil
	}

	fmt.Fprintln(c.out, "  who tried to sign in with no account:")
	for _, a := range list {
		mark := " "
		if a.Blocked() {
			mark = "B"
		}
		fmt.Fprintf(c.out, "  %s %-20s %-20s x%-4d last %s\n",
			mark, a.DiscordID, a.DiscordUsername, a.Attempts, a.LastAt.Format(time.RFC822))
		if a.BlockReason != "" {
			fmt.Fprintf(c.out, "      blocked: %s\n", a.BlockReason)
		}
	}
	fmt.Fprintln(c.out, "  B = blocked. Getting the app requires an account, so anyone here didn't")
	fmt.Fprintln(c.out, "  come through the website. `allow` them if you meant to, `block` if not.")
	return nil
}

func runBlock(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: block <discord-id> [reason]")
		return nil
	}
	id, err := discordIDArg(args)
	if err != nil {
		return err
	}
	reason := strings.TrimSpace(strings.Join(args[1:], " "))

	if err := users.BlockIdentity(ctx, c.pool, id, reason); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "  %s is blocked from signing in\n", id)
	if reason != "" {
		fmt.Fprintf(c.out, "  reason: %s\n", reason)
	}
	fmt.Fprintln(c.out, "  checked before the allowlist, so this wins over being allowed")

	// If they already have an account, blocking the identity is not the whole
	// job, and silently doing half of it would be the worse outcome.
	if u, uerr := users.GetUserByDiscordID(ctx, c.pool, id); uerr == nil {
		fmt.Fprintf(c.out, "  ! they already have an account (%s). Blocking stops new sign-ins;\n", u.Username)
		fmt.Fprintf(c.out, "    `ban %s` ends their sessions and refuses their refreshes\n", u.Username)
	}
	return nil
}

func runUnblock(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: unblock <discord-id>")
		return nil
	}
	id, err := discordIDArg(args)
	if err != nil {
		return err
	}
	lifted, err := users.UnblockIdentity(ctx, c.pool, id)
	if err != nil {
		return err
	}
	if !lifted {
		fmt.Fprintf(c.out, "  %s wasn't blocked\n", id)
		return nil
	}
	fmt.Fprintf(c.out, "  %s is no longer blocked\n", id)
	fmt.Fprintln(c.out, "  they still need to be on the allowlist to sign up")
	return nil
}

func runLink(ctx context.Context, c *Console, args []string) error {
	if len(args) < 2 {
		fmt.Fprintln(c.out, "  usage: link <username|id> <discord-id>")
		fmt.Fprintln(c.out, "  attaches a Discord account to an account that already exists")
		return nil
	}
	userID, username, err := resolveUser(ctx, c, args[0])
	if err != nil {
		return err
	}
	discordID, err := discordIDArg(args[1:])
	if err != nil {
		return err
	}

	// Only the id is set here. Their handle, avatar and connections fill in at
	// their next sign-in, which is the only moment the server holds a token to
	// read them.
	err = users.LinkDiscord(ctx, c.pool, userID, users.DiscordProfile{ID: discordID})
	switch {
	case err == nil:
	case errors.Is(err, users.ErrDiscordAlreadyLinked):
		return fmt.Errorf("that Discord account is already linked to a different user")
	default:
		return err
	}

	fmt.Fprintf(c.out, "  %s can now sign in with Discord %s\n", username, discordID)
	fmt.Fprintln(c.out, "  their name and picture fill in when they next sign in")
	return nil
}

func runUnlink(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: unlink <username|id>")
		return nil
	}
	userID, username, err := resolveUser(ctx, c, args[0])
	if err != nil {
		return err
	}
	if err := users.UnlinkDiscord(ctx, c.pool, userID); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "  %s is no longer linked to a Discord account\n", username)
	// Worth saying plainly, because it's the point of unlinking and also the
	// thing that surprises people who did it to "clean up".
	fmt.Fprintln(c.out, "  they cannot sign in until you `link` them to one")
	return nil
}

// describeID renders an id with its note when there is one.
func describeID(id, note string) string {
	if note == "" {
		return id
	}
	return fmt.Sprintf("%s (%s)", id, note)
}
