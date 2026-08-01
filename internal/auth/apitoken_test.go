package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tabuhana/bombers-server/internal/auth"
)

type stubResolver struct {
	userID string
	err    error
	called string
}

func (s *stubResolver) ResolveAPIToken(_ context.Context, secret string) (string, func(*http.Request) *http.Request, error) {
	s.called = secret
	if s.err != nil {
		return "", nil, s.err
	}
	return s.userID, func(r *http.Request) *http.Request { return r }, nil
}

// One header, two kinds of credential. The prefix decides, and it must decide
// BEFORE either verifier runs: a JWT handed to the token resolver would be
// hashed and looked up (a pointless query that can only miss), and a token
// handed to the JWT parser would be rejected as a malformed session.
func TestRequireAuthRoutesByPrefix(t *testing.T) {
	issuer := auth.NewIssuer("test-secret")
	access, _, err := issuer.IssueAccessToken("user-jwt")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	tests := []struct {
		name        string
		header      string
		wantUser    string
		wantStatus  int
		wantResolve string // what the token resolver was handed, "" for untouched
	}{
		{
			name:       "a session JWT never reaches the token resolver",
			header:     "Bearer " + access,
			wantUser:   "user-jwt",
			wantStatus: http.StatusOK,
		},
		{
			name:        "an API token never reaches the JWT parser",
			header:      "Bearer bmb_abcdefghijklmnop",
			wantUser:    "user-token",
			wantStatus:  http.StatusOK,
			wantResolve: "bmb_abcdefghijklmnop",
		},
		{
			name:       "no header is 401",
			header:     "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "a made-up JWT is 401",
			header:     "Bearer not.a.token",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &stubResolver{userID: "user-token"}
			issuer.SetTokenResolver(resolver)

			var gotUser string
			h := issuer.RequireAuth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				gotUser, _ = auth.UserIDFromContext(r.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/me", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if tc.wantStatus == http.StatusOK && rec.Code != 200 {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if tc.wantStatus != http.StatusOK && rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if gotUser != tc.wantUser {
				t.Errorf("user = %q, want %q", gotUser, tc.wantUser)
			}
			if resolver.called != tc.wantResolve {
				t.Errorf("token resolver saw %q, want %q", resolver.called, tc.wantResolve)
			}
		})
	}
}

// A server wired without the tokens domain must still work — and must not treat
// a `bmb_` string as anything but a bad session.
func TestNoResolverMeansSessionsOnly(t *testing.T) {
	issuer := auth.NewIssuer("test-secret")
	h := issuer.RequireAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a token was accepted with no resolver wired")
	}))
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer bmb_whatever")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
