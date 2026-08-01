package apitokens

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tabuhana/bombers-server/internal/auth"
)

// `auth` carries its own copy of the prefix so the dependency points one way.
// That is only safe while the copies agree: if they drift, every API token gets
// handed to the JWT parser and every agent is silently locked out.
func TestPrefixMatchesAuth(t *testing.T) {
	if auth.APITokenPrefix != Prefix {
		t.Fatalf("auth says %q, apitokens says %q", auth.APITokenPrefix, Prefix)
	}
}

// The secret must be recognisable as a Bombers token before anything hashes it,
// and unguessable once it is.
func TestSecretShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		secret, hash, err := generate()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !strings.HasPrefix(secret, Prefix) {
			t.Fatalf("secret %q has no %q prefix", secret, Prefix)
		}
		if len(secret) < 40 {
			t.Fatalf("secret is only %d chars — too short to be unguessable", len(secret))
		}
		if strings.Contains(hash, secret[len(Prefix):]) {
			t.Fatal("the stored hash contains the secret")
		}
		if seen[secret] {
			t.Fatal("generate() repeated a secret")
		}
		seen[secret] = true
	}
}

// The hash is what's stored, so the same secret must always resolve and a
// different one must never collide.
func TestHashIsStableAndDistinct(t *testing.T) {
	a, _, _ := generate()
	b, _, _ := generate()
	if hashSecret(a) != hashSecret(a) {
		t.Fatal("hashing is not stable")
	}
	if hashSecret(a) == hashSecret(b) {
		t.Fatal("two secrets hashed the same")
	}
}

// A token always knows who it belongs to; everything else must be granted.
func TestScopeSetDefaults(t *testing.T) {
	empty := NewSet(nil)
	if !empty.Has(ProfileRead) {
		t.Error("every token should be able to read /me")
	}
	for _, s := range All {
		if empty.Has(s) {
			t.Errorf("an ungranted token has %s", s)
		}
	}

	granted := NewSet([]string{string(NotesRead), string(StoreRead)})
	if !granted.Has(NotesRead) || !granted.Has(StoreRead) {
		t.Error("a granted scope is missing")
	}
	if granted.Has(NotesWrite) {
		t.Error("notes:read leaked into notes:write — read must not imply write")
	}
	if granted.Has(MessagesRead) {
		t.Error("an unrelated scope came along")
	}
}

func TestValidRejectsInvented(t *testing.T) {
	for _, s := range All {
		if !Valid(string(s)) {
			t.Errorf("%s should be valid", s)
		}
	}
	for _, bad := range []string{"", "notes", "notes:*", "*", "admin", "notes:read ", "NOTES:READ"} {
		if Valid(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

// The gate is the whole feature. A session is unscoped (it IS the person); a
// token may only do what it was granted; and a scope it lacks is 403, never 401
// — the credential is fine, the request isn't.
func TestRequireScope(t *testing.T) {
	tests := []struct {
		name    string
		ctx     func(context.Context) context.Context
		want    int
		reaches bool
	}{
		{
			name:    "a session passes — it can do whatever its owner can",
			ctx:     func(c context.Context) context.Context { return c },
			want:    http.StatusOK,
			reaches: true,
		},
		{
			name: "a token holding the scope passes",
			ctx: func(c context.Context) context.Context {
				return WithHolder(c, Holder{UserID: "u1", Scopes: NewSet([]string{string(NotesRead)})})
			},
			want:    http.StatusOK,
			reaches: true,
		},
		{
			name: "a token without it is refused",
			ctx: func(c context.Context) context.Context {
				return WithHolder(c, Holder{UserID: "u1", Scopes: NewSet([]string{string(StoreRead)})})
			},
			want: http.StatusForbidden,
		},
		{
			name: "a token with NO scopes is refused",
			ctx: func(c context.Context) context.Context {
				return WithHolder(c, Holder{UserID: "u1", Scopes: NewSet(nil)})
			},
			want: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			h := RequireScope(NotesRead)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/sync/pull", nil)
			req = req.WithContext(tc.ctx(req.Context()))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if reached != tc.reaches {
				t.Errorf("reached the handler = %v, want %v", reached, tc.reaches)
			}
			if tc.want == http.StatusForbidden {
				var body struct {
					Error string `json:"error"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				if body.Error != errForbiddenScope {
					t.Errorf("error = %q, want %q", body.Error, errForbiddenScope)
				}
			}
		})
	}
}

// Privilege that can extend itself isn't a scope system. A token must never be
// able to mint another token or revoke one, whatever it holds — otherwise a
// narrow credential issues itself a wide one, and a leaked one locks its owner
// out of the only lever they have.
func TestSessionOnlyRefusesEveryToken(t *testing.T) {
	everything := make([]string, 0, len(All))
	for _, s := range All {
		everything = append(everything, string(s))
	}

	for _, holder := range []Holder{
		{UserID: "u1", Scopes: NewSet(nil)},
		{UserID: "u1", Scopes: NewSet(everything)},
	} {
		reached := false
		h := SessionOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
		}))
		req := httptest.NewRequest(http.MethodPost, "/me/tokens", nil)
		req = req.WithContext(WithHolder(req.Context(), holder))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if reached {
			t.Fatal("a token reached the token-management routes")
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	}

	// …and a session gets through.
	reached := false
	h := SessionOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/me/tokens", nil))
	if !reached {
		t.Error("a session was refused its own tokens")
	}
}

// Resolve must reject anything that isn't shaped like one of ours BEFORE it
// reaches the database — a session JWT presented here should never become a
// query, let alone a match.
func TestResolveRejectsForeignSecrets(t *testing.T) {
	for _, s := range []string{"", "eyJhbGciOiJIUzI1NiJ9.abc.def", "bmb", "xbmb_abc"} {
		if _, err := Resolve(context.Background(), nil, s); err != ErrNotFound {
			t.Errorf("Resolve(%q) = %v, want ErrNotFound (and no db call)", s, err)
		}
	}
}
