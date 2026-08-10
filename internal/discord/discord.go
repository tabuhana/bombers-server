// Package discord is the client for Discord's OAuth2 endpoints — the only way
// anyone signs into Bombers. There are no passwords in the product.
//
// It is a leaf: net/http and encoding/json, no domain imports, no database. It
// answers one question — "who is this person on Discord" — and everything about
// accounts, allowlists and sessions happens above it.
//
// **No Discord token is ever stored.** The exchange happens during the callback,
// we read the identity and the connections we were granted, and the token is
// thrown away (and revoked, if Discord will take it). That's a deliberate trade:
// connections refresh whenever someone signs in rather than continuously, and in
// return a stolen database contains no keys to anybody's Discord account.
package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultAPIBase is Discord's API. Version-pinned: Discord ships breaking
	// changes behind the version number, so an unpinned call is one that breaks
	// on their schedule rather than ours.
	DefaultAPIBase = "https://discord.com/api/v10"

	authorizeURL = "https://discord.com/oauth2/authorize"
	cdnBase      = "https://cdn.discordapp.com"
)

// Scopes we ask for, and no more.
//
//   - identify   the account itself: id, username, avatar
//   - connections what they've linked to Discord (Steam, GitHub, League…), which
//     fills in the linked-accounts facts on a person card
//
// Notably absent: email (we send none, so holding one is a liability), guilds,
// and everything in the rpc.* family, which needs Discord's approval and reads
// their running desktop app.
var Scopes = []string{"identify", "connections"}

// Client talks to Discord on behalf of one registered application.
type Client struct {
	ClientID     string
	ClientSecret string
	// RedirectURL must match a redirect registered on the Discord application
	// EXACTLY — Discord compares strings, it doesn't pattern-match — and the same
	// value has to be sent again at the exchange or it's rejected.
	RedirectURL string

	// HTTP is the client used for every call. Nil means a sane default; tests
	// substitute one that answers without a network.
	HTTP *http.Client

	// APIBase overrides where the API lives. Empty means Discord. It exists so
	// the tests can point at a stub, which is worth one field to avoid the
	// alternative: package-level variables that every test mutates in turn.
	APIBase string
}

// api builds an endpoint URL under the configured base.
func (c *Client) api(path string) string {
	base := c.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	return strings.TrimRight(base, "/") + path
}

// Configured reports whether sign-in can work at all. A server missing these
// still boots and still serves everyone already signed in — it just can't let
// anybody new in, and it should say so rather than failing at startup.
func (c *Client) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// AuthorizeURL is where the browser gets sent to start a login.
//
// `state` is ours: Discord hands it back untouched at the callback, and the
// caller uses it both to prove the callback belongs to a login it started and to
// remember which waiting client to send the result to.
func (c *Client) AuthorizeURL(state string) string {
	q := url.Values{
		"client_id":     {c.ClientID},
		"response_type": {"code"},
		"redirect_uri":  {c.RedirectURL},
		"scope":         {strings.Join(Scopes, " ")},
		"state":         {state},
		// Ask every time rather than silently reusing a previous authorization.
		// Signing in should be a thing you can see happening.
		"prompt": {"consent"},
	}
	return authorizeURL + "?" + q.Encode()
}

// User is the part of a Discord account Bombers cares about.
type User struct {
	ID string `json:"id"`
	// Username is the handle (no discriminator since Discord's 2023 change).
	Username string `json:"username"`
	// GlobalName is the display name, which is what people actually call each
	// other. Empty for accounts that never set one.
	GlobalName string `json:"global_name"`
	// Avatar is a hash, not a URL. AvatarURL turns it into one.
	Avatar string `json:"avatar"`
}

// DisplayName is what to show: their chosen display name, falling back to the
// handle.
func (u User) DisplayName() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// AvatarURL is where their picture lives, or "" if they've never set one.
//
// We don't copy it into our own media storage — a Discord avatar is theirs and
// changes when they change it, and a stale copy is worse than a link. A user who
// wants a Bombers-specific picture uploads one, which takes precedence.
func (u User) AvatarURL(size int) string {
	if u.Avatar == "" {
		return ""
	}
	if size <= 0 {
		size = 256
	}
	// Animated avatars start with a_ and are gifs; anything else renders as png.
	ext := "png"
	if strings.HasPrefix(u.Avatar, "a_") {
		ext = "gif"
	}
	return fmt.Sprintf("%s/avatars/%s/%s.%s?size=%d", cdnBase, u.ID, u.Avatar, ext, size)
}

// Connection is one third-party account someone has linked to Discord.
type Connection struct {
	// Type is the service: "steam", "github", "leagueoflegends", "twitch"…
	Type string `json:"type"`
	// ID is their identifier on that service, Name their handle there.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Verified is Discord's word that they proved they own it.
	Verified bool `json:"verified"`
	// Visibility is 1 when shown on their Discord profile, 0 when hidden.
	Visibility int `json:"visibility"`
}

// Public reports whether a connection is one they show on their own Discord
// profile. Hidden ones were granted to us by the scope but were never meant to
// be on display, so they shouldn't appear on a person card either.
func (c Connection) Public() bool { return c.Visibility == 1 }

// Exchange trades the callback's code for an access token.
func (c *Client) Exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		// Sent again, and it has to match the one from the authorize step.
		"redirect_uri": {c.RedirectURL},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api("/oauth2/token"), strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building the token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching Discord: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discord refused the code exchange: %s", describe(resp.StatusCode, body))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("discord's token response made no sense: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("discord returned no access token")
	}
	return out.AccessToken, nil
}

// Me reads the account the token belongs to.
func (c *Client) Me(ctx context.Context, token string) (User, error) {
	var user User
	if err := c.get(ctx, c.api("/users/@me"), token, &user); err != nil {
		return User{}, err
	}
	if user.ID == "" {
		return User{}, fmt.Errorf("discord returned an account with no id")
	}
	return user, nil
}

// Connections reads their linked accounts.
//
// A failure here is NOT a failed login — connections are decoration on a person
// card, and refusing to sign somebody in because their Steam link couldn't be
// read would be absurd. Callers should log and carry on.
func (c *Client) Connections(ctx context.Context, token string) ([]Connection, error) {
	var conns []Connection
	if err := c.get(ctx, c.api("/users/@me/connections"), token, &conns); err != nil {
		return nil, err
	}
	return conns, nil
}

// Revoke hands the token back. Best effort: we're done with it either way, and
// nothing about the login depends on Discord accepting this.
func (c *Client) Revoke(ctx context.Context, token string) {
	form := url.Values{
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"token":         {token},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api("/oauth2/token/revoke"), strings.NewReader(form.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// get performs an authorized GET and decodes the response into out.
func (c *Client) get(ctx context.Context, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("reaching Discord: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discord refused the request: %s", describe(resp.StatusCode, body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("discord's response made no sense: %w", err)
	}
	return nil
}

// describe turns a failed response into something worth putting in a log, using
// Discord's own error text when it sent any and never running long.
func describe(status int, body []byte) string {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:297] + "..."
	}
	if msg == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, msg)
}
