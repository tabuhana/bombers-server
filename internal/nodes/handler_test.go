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

// AN ASSET KEY IS AN ID AND A PATH, and between them they are the whole of what
// could escape `nodes/`. A traversal through a published bundle would be the
// worst kind of bug, so both halves are checked before anything touches storage.
func TestValidNodeID(t *testing.T) {
	ok := []string{"weather", "core:weather", "sdk:my-node", "a_b.c", "x"}
	bad := []string{
		"",                // nothing
		"nodes/weather",   // a separator is a path
		"core\\weather",   // the other separator
		"..",              // the dot segment itself
		"core:..:weather", // and one buried in a namespace
		".hidden",         // a hidden entry on the filesystem backend
		"core weather",    // a space
		"core%3Aweather",  // still encoded: the handler decodes first
	}
	for _, id := range ok {
		if !ValidNodeID(id) {
			t.Errorf("ValidNodeID(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if ValidNodeID(id) {
			t.Errorf("ValidNodeID(%q) = true, want false", id)
		}
	}
	// 128 is the cap, so one past it is refused.
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'a'
	}
	if ValidNodeID(string(long)) {
		t.Error("a 129-character id was accepted")
	}
}

func TestValidAssetPath(t *testing.T) {
	for _, p := range []string{"icon.png", "art/icon.svg", "a/b/c.json"} {
		if !ValidAssetPath(p) {
			t.Errorf("ValidAssetPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", "/icon.png", "..", "art/../../etc/passwd", "art//icon.png", "art\\icon.png", "./icon.png"} {
		if ValidAssetPath(p) {
			t.Errorf("ValidAssetPath(%q) = true, want false", p)
		}
	}
}

// THE KEY TAKES THE ID VERBATIM. Sanitising a namespaced id would let two
// different nodes name one object, which is the one thing an object key must not
// allow — the id is kept safe by `ValidNodeID` instead.
func TestAssetKeyKeepsTheNamespace(t *testing.T) {
	if got := AssetKey("core:weather", "art/icon.svg"); got != "nodes/core:weather/art/icon.svg" {
		t.Errorf("AssetKey = %q", got)
	}
	// The prefix a publish and an unpublish clear. The trailing slash is what
	// stops one id taking another's bytes (`weather` vs `weather2`).
	if got := AssetKey("weather", ""); got != "nodes/weather/" {
		t.Errorf("AssetKey prefix = %q", got)
	}
}

// AN ID IS HALF A KEY, so a publish refuses one that could become a path —
// where the operator is standing, rather than at the upload that follows.
func TestPublishRefusesAnIDThatIsAPath(t *testing.T) {
	_, err := parseBundle([]byte(`{"manifest":{"id":"a/../b","name":"X"},"files":{"i.js":"1"}}`))
	if err == nil {
		t.Fatal("a bundle whose id is a path was accepted")
	}
	if _, err := parseBundle([]byte(`{"manifest":{"id":"core:weather","name":"X"},"files":{"i.js":"1"}}`)); err != nil {
		t.Fatalf("a namespaced id was refused: %v", err)
	}
}

// `assets` IS ADDED BESIDE THE BUNDLE'S OWN KEYS and never wraps them: this
// route's body has always BEEN the bundle, so an envelope would break every
// client already installed. An old client ignores the extra key.
func TestWithAssetsIsAdditive(t *testing.T) {
	got := string(withAssets(
		[]byte(`{"manifest":{"id":"w"},"files":{"i.js":"1"}}`),
		[]Asset{{Path: "icon.png", ContentType: "image/png", Size: 12}},
	))
	want := `{"manifest":{"id":"w"},"files":{"i.js":"1"},"assets":[{"path":"icon.png","content_type":"image/png","size_bytes":12}]}`
	if got != want {
		t.Errorf("withAssets =\n  %s\nwant\n  %s", got, want)
	}
	// A node with no art serialises as an empty list, never null: a client
	// should not have to tell "none" from "absent".
	if got := string(withAssets([]byte(`{"files":{}}`), []Asset{})); got != `{"files":{},"assets":[]}` {
		t.Errorf("empty assets = %s", got)
	}
	// An empty object has nothing to put a comma after.
	if got := string(withAssets([]byte(`{}`), []Asset{})); got != `{"assets":[]}` {
		t.Errorf("empty bundle = %s", got)
	}
	// AND A BUNDLE THAT IS NOT AN OBJECT COMES BACK UNTOUCHED. `parseBundle`
	// means that cannot happen on the way in; a reader that invented a shape for
	// it would be the one place a store read could lie about what was published.
	if got := string(withAssets([]byte(`[1,2]`), []Asset{})); got != `[1,2]` {
		t.Errorf("non-object bundle = %s", got)
	}
}
