package users

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tabuhana/bombers-server/internal/config"
	"github.com/tabuhana/bombers-server/internal/settings"
)

// testDiscordHandler builds one with a nil pool and the settings supplied
// through the ENVIRONMENT, which is what makes this testable without a database:
// Resolve checks env first, so nothing here needs to reach the settings table.
// Start never touches the pool either, which is the security-relevant half.
func testDiscordHandler(t *testing.T, website string) *DiscordHandler {
	t.Setenv("DISCORD_CLIENT_ID", "client-id")
	t.Setenv("DISCORD_CLIENT_SECRET", "client-secret")
	t.Setenv("DISCORD_REDIRECT_URL", "http://192.168.1.110:1337/auth/discord/callback")
	t.Setenv("WEBSITE_URL", website)
	t.Setenv("SIGNUP_MODE", config.SignupList)
	return NewDiscordHandler(nil, nil, settings.New(nil))
}

// testUnconfiguredHandler has no Discord application at all.
func testUnconfiguredHandler(t *testing.T) *DiscordHandler {
	t.Setenv("DISCORD_CLIENT_ID", "")
	t.Setenv("DISCORD_CLIENT_SECRET", "")
	t.Setenv("DISCORD_REDIRECT_URL", "")
	t.Setenv("WEBSITE_URL", "")
	return NewDiscordHandler(nil, nil, settings.New(nil))
}

func get(h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestStartRedirectsToDiscord(t *testing.T) {
	h := testDiscordHandler(t, "https://hanascript.com")
	rec := get(h.Start, "/auth/discord/start?from=app&port=54321")

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location doesn't parse: %v", err)
	}
	if loc.Host != "discord.com" {
		t.Errorf("redirected to %q, want discord.com", loc.Host)
	}
	if loc.Query().Get("state") == "" {
		t.Error("no state in the authorize URL — the callback would have nothing to verify")
	}
}

// The port is a number precisely so that no caller can hand this endpoint a URL.
// An OAuth callback that redirects wherever it's told is an open redirect, and
// it's the classic bug in exactly this position.
func TestStartRejectsAnythingButAPort(t *testing.T) {
	h := testDiscordHandler(t, "https://hanascript.com")
	for _, bad := range []string{
		"",                         // missing
		"notanumber",               //
		"80",                       // privileged
		"0",                        //
		"70000",                    // out of range
		"-1",                       //
		"54321/../evil",            // not a number at all
		"https://evil.example.com", // the attack this shape prevents
	} {
		rec := get(h.Start, "/auth/discord/start?from=app&port="+url.QueryEscape(bad))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("port=%q gave %d, want 400", bad, rec.Code)
		}
	}
}

func TestStartFromWeb(t *testing.T) {
	h := testDiscordHandler(t, "https://hanascript.com")
	rec := get(h.Start, "/auth/discord/start?from=web")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "discord.com") {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
}

func TestStartFromWebNeedsAWebsite(t *testing.T) {
	h := testDiscordHandler(t, "")
	rec := get(h.Start, "/auth/discord/start?from=web")
	// Better to say the server isn't set up than to redirect somebody to "/".
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when WEBSITE_URL is unset", rec.Code)
	}
}

func TestStartRejectsUnknownOrigin(t *testing.T) {
	h := testDiscordHandler(t, "https://hanascript.com")
	for _, from := range []string{"", "desktop", "cli", "APP"} {
		rec := get(h.Start, "/auth/discord/start?from="+from)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("from=%q gave %d, want 400", from, rec.Code)
		}
	}
}

func TestStartWithoutADiscordApp(t *testing.T) {
	h := testUnconfiguredHandler(t)
	rec := get(h.Start, "/auth/discord/start?from=app&port=54321")
	// Nobody can sign in, and the server should say so rather than sending a
	// browser to a Discord URL with no client id in it.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 with no Discord app configured", rec.Code)
	}
}

func TestCallbackWithoutAValidStateGoesNowhere(t *testing.T) {
	h := testDiscordHandler(t, "https://hanascript.com")
	rec := get(h.Callback, "/auth/discord/callback?code=x&state=never-issued")

	// No valid state means no verified place to send anybody, so this is the one
	// branch that answers directly instead of redirecting. It must NOT redirect.
	if rec.Code == http.StatusFound {
		t.Fatalf("an unknown state produced a redirect to %q", rec.Header().Get("Location"))
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestClaimRejectsAnUnknownCode(t *testing.T) {
	h := testDiscordHandler(t, "https://hanascript.com")
	rec := httptest.NewRecorder()
	h.Claim(rec, httptest.NewRequest(http.MethodPost, "/auth/discord/claim",
		strings.NewReader(`{"code":"never-issued"}`)))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// A refused login from the app says one thing and nothing else. The only route
// to the installer is the website terminal, so a client asking on behalf of an
// identity with no account did not come through it.
func TestRefusalWording(t *testing.T) {
	h := testDiscordHandler(t, "https://hanascript.com")

	if got := h.refusalFor(PendingLogin{FromApp: true}); got != errNotAuthorized {
		t.Errorf("app refusal = %q, want %q", got, errNotAuthorized)
	}
	if got := h.refusalFor(PendingLogin{FromApp: false}); got != errNotAllowed {
		t.Errorf("web refusal = %q, want %q", got, errNotAllowed)
	}
}
