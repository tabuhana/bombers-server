package console

import (
	"context"
	"fmt"
	"strings"

	"github.com/tabuhana/bombers-server/internal/settings"
)

// Configuration an operator changes without restarting anything.
//
// The Discord application in particular had nowhere to live: env vars mean
// editing a file and restarting, and a wizard-configured install has no file to
// edit — config.json only carries what the wizard knows to ask. Now it's a table
// the console writes and every sign-in reads.
//
// Environment variables still win. When one does, these commands SAY so rather
// than appearing to work and changing nothing.

func runConfig(ctx context.Context, c *Console, _ []string) error {
	set := settings.New(c.pool)
	for _, r := range set.All(ctx) {
		value := r.Value
		if r.Secret {
			value = settings.Mask(value)
		}
		if value == "" {
			value = "—"
		}
		note := ""
		switch r.Source {
		case settings.FromEnv:
			note = fmt.Sprintf("(from %s — the console can't change it)", r.Env)
		case settings.FromDefault:
			if r.Default != "" {
				note = "(default)"
			} else {
				note = "(not set)"
			}
		}
		fmt.Fprintf(c.out, "  %-24s %-28s %s\n", r.Key, value, note)
	}
	fmt.Fprintln(c.out, "  change one with `set <key> <value>`, clear it with `set <key>`")
	return nil
}

func runSet(ctx context.Context, c *Console, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(c.out, "  usage: set <key> [value]        (no value clears it)")
		fmt.Fprintln(c.out, "  keys:")
		for _, s := range settings.Known {
			fmt.Fprintf(c.out, "    %-24s %s\n", s.Key, s.Help)
		}
		return nil
	}

	key := strings.TrimSpace(args[0])
	setting, ok := settings.Lookup(key)
	if !ok {
		return fmt.Errorf("no setting called %q — `config` lists them", key)
	}
	value := strings.TrimSpace(strings.Join(args[1:], " "))

	// signup.mode is the one with a closed set of answers, and a typo there
	// silently means "not list", which is the dangerous direction.
	if key == settings.SignupMode && value != "" && value != "list" && value != "open" {
		return fmt.Errorf("signup.mode is `list` or `open`, not %q", value)
	}

	set := settings.New(c.pool)
	if err := set.Set(ctx, key, value); err != nil {
		return err
	}

	shown := value
	if setting.Secret {
		shown = settings.Mask(value)
	}
	if value == "" {
		fmt.Fprintf(c.out, "  cleared %s\n", key)
	} else {
		fmt.Fprintf(c.out, "  %s = %s\n", key, shown)
	}

	// Saved, but not in force — worth saying immediately rather than letting the
	// operator wonder why nothing changed.
	if r := set.Resolve(ctx, key); r.Source == settings.FromEnv {
		fmt.Fprintf(c.out, "  ! %s is set in the environment and overrides this\n", setting.Env)
		return nil
	}

	fmt.Fprintln(c.out, "  in effect now — the next sign-in reads it")
	return nil
}

func runDiscord(ctx context.Context, c *Console, args []string) error {
	set := settings.New(c.pool)

	if len(args) == 0 || args[0] == "show" {
		id := set.Resolve(ctx, settings.DiscordClientID)
		secret := set.Resolve(ctx, settings.DiscordClientSecret)
		redirect := set.Resolve(ctx, settings.DiscordRedirectURL)

		if id.Value == "" || secret.Value == "" || redirect.Value == "" {
			fmt.Fprintln(c.out, "  Discord sign-in is NOT configured — nobody can sign in.")
		} else {
			fmt.Fprintln(c.out, "  Discord sign-in is ready.")
		}
		fmt.Fprintf(c.out, "    client id     %s\n", orDash(id.Value))
		fmt.Fprintf(c.out, "    client secret %s\n", orDash(settings.Mask(secret.Value)))
		fmt.Fprintf(c.out, "    redirect      %s\n", orDash(redirect.Value))
		fmt.Fprintln(c.out, "  set it with: discord set <client-id> <client-secret> <redirect-url>")
		return nil
	}

	if args[0] == "clear" {
		return clearDiscord(ctx, c, set)
	}

	if args[0] != "set" {
		fmt.Fprintln(c.out, "  usage: discord            show the current application")
		fmt.Fprintln(c.out, "         discord set <client-id> <client-secret> <redirect-url>")
		fmt.Fprintln(c.out, "         discord clear")
		return nil
	}

	if len(args) < 4 {
		fmt.Fprintln(c.out, "  usage: discord set <client-id> <client-secret> <redirect-url>")
		fmt.Fprintln(c.out, "  all three at once, because two of the three configured is the same")
		fmt.Fprintln(c.out, "  as none of them — sign-in needs the whole set.")
		return nil
	}

	for key, value := range map[string]string{
		settings.DiscordClientID:     args[1],
		settings.DiscordClientSecret: args[2],
		settings.DiscordRedirectURL:  args[3],
	} {
		if err := set.Set(ctx, key, value); err != nil {
			return err
		}
	}

	fmt.Fprintln(c.out, "  Discord application saved. Sign-in works from the next attempt.")
	fmt.Fprintf(c.out, "    redirect  %s\n", args[3])
	fmt.Fprintln(c.out, "  ! that redirect must be registered on the Discord application EXACTLY,")
	fmt.Fprintln(c.out, "    at discord.com/developers → your app → OAuth2 → Redirects.")

	for _, key := range []string{settings.DiscordClientID, settings.DiscordClientSecret, settings.DiscordRedirectURL} {
		if r := set.Resolve(ctx, key); r.Source == settings.FromEnv {
			fmt.Fprintf(c.out, "  ! %s is set in the environment and overrides what you just saved\n", r.Env)
		}
	}
	return nil
}

func clearDiscord(ctx context.Context, c *Console, set *settings.Store) error {
	for _, key := range []string{settings.DiscordClientID, settings.DiscordClientSecret, settings.DiscordRedirectURL} {
		if err := set.Set(ctx, key, ""); err != nil {
			return err
		}
	}
	fmt.Fprintln(c.out, "  Discord application cleared — nobody can sign in until it's set again")
	return nil
}

func runSignups(ctx context.Context, c *Console, args []string) error {
	set := settings.New(c.pool)

	if len(args) == 0 {
		r := set.Resolve(ctx, settings.SignupMode)
		switch r.Value {
		case "open":
			fmt.Fprintln(c.out, "  signups: OPEN — anyone who signs in with Discord gets an account")
		default:
			fmt.Fprintln(c.out, "  signups: LIST — only Discord ids on the allowlist (`allowed`)")
		}
		if r.Source == settings.FromEnv {
			fmt.Fprintf(c.out, "  (from %s — the console can't change it)\n", r.Env)
		}
		fmt.Fprintln(c.out, "  change with `signups open` or `signups list`")
		return nil
	}

	mode := strings.ToLower(strings.TrimSpace(args[0]))
	if mode != "list" && mode != "open" {
		return fmt.Errorf("`signups list` or `signups open`, not %q", mode)
	}
	if err := set.Set(ctx, settings.SignupMode, mode); err != nil {
		return err
	}
	if mode == "open" {
		fmt.Fprintln(c.out, "  signups are OPEN — anybody with a Discord account can make one")
		fmt.Fprintln(c.out, "  the allowlist is ignored while this is on, and `block` still applies")
	} else {
		fmt.Fprintln(c.out, "  signups are LIST — only ids you `allow` can make an account")
	}
	if r := set.Resolve(ctx, settings.SignupMode); r.Source == settings.FromEnv {
		fmt.Fprintf(c.out, "  ! %s is set in the environment and overrides this\n", r.Env)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
