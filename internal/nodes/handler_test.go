package nodes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A node id has a namespace, so it has a colon: `core:weather`. A client
// percent-encodes that into the path, and chi routes on the RAW path whenever
// it differs from the decoded one — handing back `core%3Aweather`, which
// matches no row.
//
// The symptom was misleading: the catalogue worked (no id in the URL) while
// installing failed with "that node is no longer published", so it read as a
// publishing problem rather than a reading one.
func TestNodeIDIsDecoded(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"an encoded colon", "core%3Aweather", "core:weather"},
		{"an unencoded colon", "core:weather", "core:weather"},
		{"a plain id", "weather", "weather"},
		{"an installed snapshot id", "sdk%3Amynode", "sdk:mynode"},
		// A broken escape is a lookup that should MISS, not a request that
		// should fall over.
		{"a malformed escape survives", "core%zzweather", "core%zzweather"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", tc.raw)
			r := httptest.NewRequest(http.MethodGet, "/nodes/x/bundle", nil)
			r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

			if got := nodeID(r); got != tc.want {
				t.Errorf("nodeID(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
