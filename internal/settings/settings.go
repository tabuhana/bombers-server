// Package settings is the configuration an operator changes at runtime, as
// opposed to the configuration a deployment decides.
//
// The split matters. A port, a database URL, where media lives — those belong to
// whoever deployed the server, and env vars are exactly right for them. Who may
// sign up, and which Discord application to sign them in with, belong to whoever
// OPERATES it, and env vars are exactly wrong: changing one meant editing a file
// and restarting, and on a wizard-configured install there was no file to edit,
// because config.json can only carry what the wizard knows to ask.
//
// So these live in a table the console writes and every request reads.
//
// **The environment still wins.** Resolve checks env first, so a container deploy
// passing DISCORD_CLIENT_ID behaves exactly as it did, and an operator who can't
// reach the console can always override from outside. That also means a value set
// in the environment can't be changed from the console, and Resolve reports which
// source answered so the console can say so instead of appearing to do nothing.
package settings

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The keys. Names are dotted rather than SCREAMING_CASE because they're rows,
// not environment variables — the env name each maps to is on the Setting below.
const (
	DiscordClientID     = "discord.client_id"
	DiscordClientSecret = "discord.client_secret"
	DiscordRedirectURL  = "discord.redirect_url"
	WebsiteURL          = "website.url"
	SignupMode          = "signup.mode"
)

// Source says where a resolved value came from — so the console can explain that
// an environment variable is overriding what it was just asked to change.
type Source string

const (
	FromEnv     Source = "env"
	FromDB      Source = "database"
	FromDefault Source = "default"
)

// Setting describes one operator-changeable value.
type Setting struct {
	Key string
	// Env is the variable that overrides it.
	Env string
	// Default when neither env nor the database has one.
	Default string
	// Secret hides the value in listings. It still goes in the database as
	// written — this is about not printing it into a terminal someone might be
	// sharing, not about storage.
	Secret bool
	// Help is one line, for the console.
	Help string
}

// Known is every setting, in the order the console lists them.
var Known = []Setting{
	{Key: DiscordClientID, Env: "DISCORD_CLIENT_ID", Help: "Discord application's client ID"},
	{Key: DiscordClientSecret, Env: "DISCORD_CLIENT_SECRET", Secret: true, Help: "Discord application's client secret"},
	{Key: DiscordRedirectURL, Env: "DISCORD_REDIRECT_URL", Help: "must match a redirect registered on the Discord app EXACTLY"},
	{Key: WebsiteURL, Env: "WEBSITE_URL", Help: "where a website sign-in returns (only the website half needs it)"},
	{Key: SignupMode, Env: "SIGNUP_MODE", Default: "list", Help: "list (allowlist only) or open (anyone with Discord)"},
}

// Lookup finds a setting by key.
func Lookup(key string) (Setting, bool) {
	for _, s := range Known {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

// ErrUnknownKey is returned for a key that isn't in Known. Settings are a closed
// set on purpose: a typo shouldn't silently become a row nothing reads.
var ErrUnknownKey = errors.New("unknown setting")

// Resolved is one setting's effective value and where it came from.
type Resolved struct {
	Setting
	Value  string
	Source Source
}

// Store reads and writes the table.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Resolve returns the effective value of one setting: environment, then the
// database, then the default. A database error resolves to the default rather
// than failing — a sign-in shouldn't break because a settings read hiccuped, and
// the default for every one of these is the safe answer.
func (s *Store) Resolve(ctx context.Context, key string) Resolved {
	setting, ok := Lookup(key)
	if !ok {
		return Resolved{Value: "", Source: FromDefault}
	}
	if v := strings.TrimSpace(os.Getenv(setting.Env)); v != "" {
		return Resolved{Setting: setting, Value: v, Source: FromEnv}
	}
	if s != nil && s.pool != nil {
		var v string
		err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&v)
		if err == nil && strings.TrimSpace(v) != "" {
			return Resolved{Setting: setting, Value: strings.TrimSpace(v), Source: FromDB}
		}
	}
	return Resolved{Setting: setting, Value: setting.Default, Source: FromDefault}
}

// Get is Resolve without the provenance, for callers that just want the value.
func (s *Store) Get(ctx context.Context, key string) string {
	return s.Resolve(ctx, key).Value
}

// All resolves every known setting, for the console's listing.
func (s *Store) All(ctx context.Context) []Resolved {
	out := make([]Resolved, 0, len(Known))
	for _, setting := range Known {
		out = append(out, s.Resolve(ctx, setting.Key))
	}
	return out
}

// Set writes a value. An empty value deletes the row, which is how you go back
// to the default — storing "" would be a value that reads as unset everywhere
// and looks deliberate in the table.
func (s *Store) Set(ctx context.Context, key, value string) error {
	if _, ok := Lookup(key); !ok {
		return fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		_, err := s.pool.Exec(ctx, `DELETE FROM settings WHERE key = $1`, key)
		if err != nil {
			return fmt.Errorf("clearing %s: %w", key, err)
		}
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, value)
	if err != nil {
		return fmt.Errorf("saving %s: %w", key, err)
	}
	return nil
}

// StoredValue reads what's in the TABLE, ignoring env and defaults — so the
// console can tell "there is nothing stored" apart from "something is stored but
// an environment variable is winning".
func (s *Store) StoredValue(ctx context.Context, key string) (string, bool) {
	var v string
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) || err != nil {
		return "", false
	}
	return v, true
}

// Mask renders a secret for display. It shows enough to recognise which value is
// there without printing one into a terminal somebody might be sharing.
func Mask(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 6 {
		return strings.Repeat("•", len(value))
	}
	return value[:3] + strings.Repeat("•", 8) + value[len(value)-3:]
}
