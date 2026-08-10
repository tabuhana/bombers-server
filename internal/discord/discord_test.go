package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newTestClient points a Client at a stub server standing in for Discord.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "http://192.168.1.110:1337/auth/discord/callback",
		HTTP:         srv.Client(),
		APIBase:      srv.URL,
	}, srv
}

func TestConfigured(t *testing.T) {
	full := Client{ClientID: "a", ClientSecret: "b", RedirectURL: "c"}
	if !full.Configured() {
		t.Error("a fully configured client should report configured")
	}
	// Each one missing on its own has to be enough to report unconfigured — the
	// whole point is that a half-configured server says so instead of failing
	// mysteriously at the callback.
	for _, c := range []Client{
		{ClientSecret: "b", RedirectURL: "c"},
		{ClientID: "a", RedirectURL: "c"},
		{ClientID: "a", ClientSecret: "b"},
		{},
	} {
		if c.Configured() {
			t.Errorf("%+v should not report configured", c)
		}
	}
}

func TestAuthorizeURL(t *testing.T) {
	c := Client{
		ClientID:    "1536263879573901312",
		RedirectURL: "http://192.168.1.110:1337/auth/discord/callback",
	}
	got, err := url.Parse(c.AuthorizeURL("state-token"))
	if err != nil {
		t.Fatalf("the authorize URL doesn't parse: %v", err)
	}
	q := got.Query()

	checks := map[string]string{
		"client_id":     "1536263879573901312",
		"response_type": "code",
		"redirect_uri":  "http://192.168.1.110:1337/auth/discord/callback",
		"state":         "state-token",
		// Space-separated, not comma — Discord rejects the wrong separator.
		"scope": "identify connections",
	}
	for key, want := range checks {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, q.Get(key), want)
		}
	}

	// Asking for anything beyond identity would be a scope creep nobody noticed.
	for _, scope := range strings.Fields(q.Get("scope")) {
		if scope != "identify" && scope != "connections" {
			t.Errorf("unexpected scope requested: %q", scope)
		}
	}
}

func TestExchange(t *testing.T) {
	var gotForm url.Values
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","scope":"identify connections"}`))
	})
	token, err := c.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if token != "tok" {
		t.Errorf("token = %q, want %q", token, "tok")
	}
	// redirect_uri must be re-sent and must match, or Discord rejects it — a
	// mistake that only shows up against the real service.
	if gotForm.Get("redirect_uri") != c.RedirectURL {
		t.Errorf("redirect_uri = %q, want %q", gotForm.Get("redirect_uri"), c.RedirectURL)
	}
	if gotForm.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", gotForm.Get("grant_type"))
	}
	if gotForm.Get("code") != "the-code" {
		t.Errorf("code = %q", gotForm.Get("code"))
	}
}

func TestExchangeRefused(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	if _, err := c.Exchange(context.Background(), "stale"); err == nil {
		t.Fatal("a refused exchange should be an error")
	} else if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the error should carry Discord's reason, got: %v", err)
	}
}

func TestDisplayName(t *testing.T) {
	if got := (User{Username: "handle", GlobalName: "Display"}).DisplayName(); got != "Display" {
		t.Errorf("DisplayName = %q, want the global name", got)
	}
	// Accounts that never set a display name fall back to the handle rather than
	// showing nothing.
	if got := (User{Username: "handle"}).DisplayName(); got != "handle" {
		t.Errorf("DisplayName = %q, want the username", got)
	}
}

func TestAvatarURL(t *testing.T) {
	// No avatar set is not an error — it's a person with the default picture,
	// and the caller shows their own placeholder.
	if got := (User{ID: "1"}).AvatarURL(256); got != "" {
		t.Errorf("AvatarURL with no avatar = %q, want empty", got)
	}
	got := User{ID: "42", Avatar: "abc"}.AvatarURL(128)
	if !strings.Contains(got, "/avatars/42/abc.png") || !strings.Contains(got, "size=128") {
		t.Errorf("AvatarURL = %q", got)
	}
	// Animated avatars are gifs, and asking for the png renders a still frame.
	if got := (User{ID: "42", Avatar: "a_xyz"}).AvatarURL(0); !strings.Contains(got, ".gif") {
		t.Errorf("animated AvatarURL = %q, want a gif", got)
	}
}

func TestConnectionPublic(t *testing.T) {
	if !(Connection{Visibility: 1}).Public() {
		t.Error("visibility 1 is shown on their profile")
	}
	// Granted to us by the scope, but hidden on their Discord profile — so it
	// shouldn't surface on a person card either.
	if (Connection{Visibility: 0}).Public() {
		t.Error("visibility 0 is hidden and must not count as public")
	}
}
