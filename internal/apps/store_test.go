package apps

import (
	"encoding/json"
	"strings"
	"testing"
)

// AN ASSET KEY IS AN ID AND A PATH, and between them they are the whole of what
// could escape `apps/`. A traversal through a published bundle would be the
// worst kind of bug, so both halves are checked before anything touches storage.
func TestValidAppID(t *testing.T) {
	ok := []string{"weather", "core:weather", "sdk:my-app", "a_b.c", "x"}
	bad := []string{
		"",                // nothing
		"apps/weather",    // a separator is a path
		"core\\weather",   // the other separator
		"..",              // the dot segment itself
		"core:..:weather", // and one buried in a namespace
		".hidden",         // a hidden entry on the filesystem backend
		"core weather",    // a space
		"core%3Aweather",  // still encoded: the handler decodes first
	}
	for _, id := range ok {
		if !ValidAppID(id) {
			t.Errorf("ValidAppID(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if ValidAppID(id) {
			t.Errorf("ValidAppID(%q) = true, want false", id)
		}
	}
	// 128 is the cap, so one past it is refused.
	if ValidAppID(strings.Repeat("a", 129)) {
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
// different apps name one object, which is the one thing an object key must not
// allow — the id is kept safe by `ValidAppID` instead.
func TestAssetKeyKeepsTheNamespace(t *testing.T) {
	if got := AssetKey("core:weather", "art/icon.svg"); got != "apps/core:weather/art/icon.svg" {
		t.Errorf("AssetKey = %q", got)
	}
	// The prefix a publish and an unpublish clear. The trailing slash is what
	// stops one id taking another's bytes (`weather` vs `weather2`).
	if got := AssetKey("weather", ""); got != "apps/weather/" {
		t.Errorf("AssetKey prefix = %q", got)
	}
}

// THE TWO STORES DO NOT SHARE AN OBJECT FOLDER, which is what lets an app be
// published under the id of the node it replaces while both are live — their
// art cannot collide, so unpublishing either leaves the other's bytes alone.
func TestAppAndNodeKeysCannotCollide(t *testing.T) {
	key := AssetKey("core:weather", "art/icon.svg")
	if strings.HasPrefix(key, "nodes/") {
		t.Fatalf("an app's asset landed in the node store's folder: %q", key)
	}
	if !strings.HasPrefix(key, "apps/") {
		t.Fatalf("AssetKey lost its prefix: %q", key)
	}
}

// AN ID IS HALF A KEY, so a publish refuses one that could become a path —
// where the operator is standing, rather than at the upload that follows.
func TestPublishRefusesAnIDThatIsAPath(t *testing.T) {
	if _, err := parseBundle([]byte(`{"manifest":{"id":"a/../b","name":"X"},"files":{"screen.jsx":"1"}}`)); err == nil {
		t.Fatal("a bundle whose id is a path was accepted")
	}
	if _, err := parseBundle([]byte(`{"manifest":{"id":"core:weather","name":"X"},"files":{"screen.jsx":"1"}}`)); err != nil {
		t.Fatalf("a namespaced id was refused: %v", err)
	}
}

// THE SERVER DOES NOT KNOW WHAT AN APP IS, and must not learn. A skin, a widget
// declaration and an icon block are the client's vocabulary; the bundle is
// stored opaquely, so what an app declares can change in a client release and
// never in a deploy. A `parseBundle` that started checking for `skin` would
// make every future manifest field a migration.
func TestPublishDoesNotJudgeTheManifest(t *testing.T) {
	// No skin, no icon, no widget — nothing an app is supposed to have.
	info, err := parseBundle([]byte(`{"manifest":{"id":"bare","name":"Bare"},"files":{"screen.jsx":"x"}}`))
	if err != nil {
		t.Fatalf("a manifest the CLIENT would refuse was refused here too: %v", err)
	}
	if info.ID != "bare" || info.Name != "Bare" {
		t.Errorf("got %+v", info)
	}
}

// The four things a bundle must be, each of which fails without a database.
func TestParseBundleRefusals(t *testing.T) {
	bad := []struct {
		name string
		body string
	}{
		{"empty", ``},
		{"not JSON", `screen.jsx`},
		{"no manifest id", `{"manifest":{"name":"X"},"files":{"a":"b"}}`},
		{"no manifest name", `{"manifest":{"id":"x"},"files":{"a":"b"}}`},
		{"a whitespace name", `{"manifest":{"id":"x","name":"  "},"files":{"a":"b"}}`},
		{"no files", `{"manifest":{"id":"x","name":"X"},"files":{}}`},
		{"files aren't strings", `{"manifest":{"id":"x","name":"X"},"files":{"a":{"b":1}}}`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBundle([]byte(tc.body)); err == nil {
				t.Error("accepted")
			}
		})
	}
	// One past the cap, as bytes rather than as a count of anything.
	over := `{"manifest":{"id":"x","name":"` + strings.Repeat("a", BundleLimit) + `"},"files":{"a":"b"}}`
	if _, err := parseBundle([]byte(over)); err == nil {
		t.Error("a bundle over the cap was accepted")
	}
}

// `assets` IS ADDED BESIDE THE BUNDLE'S OWN KEYS and never wraps them: the body
// of this route IS the bundle, matching the node store the client already
// reads, so `manifest` and `files` stay on the top level and an extra key is
// additive.
func TestWithAssetsIsAdditive(t *testing.T) {
	got := string(withAssets(
		[]byte(`{"manifest":{"id":"w"},"files":{"screen.jsx":"1"}}`),
		[]Asset{{Path: "icon.png", ContentType: "image/png", Size: 12}},
	))
	want := `{"manifest":{"id":"w"},"files":{"screen.jsx":"1"},"assets":[{"path":"icon.png","content_type":"image/png","size_bytes":12}]}`
	if got != want {
		t.Errorf("withAssets =\n  %s\nwant\n  %s", got, want)
	}
	// An app with no art serialises as an empty list, never null: a client
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
	// And whatever it splices stays parseable — the one thing a hand-built JSON
	// writer can get wrong.
	var back map[string]any
	if err := json.Unmarshal(withAssets([]byte(`{"manifest":{"id":"w"}}`), []Asset{{Path: "a.svg"}}), &back); err != nil {
		t.Fatalf("spliced output is not valid JSON: %v", err)
	}
	if _, ok := back["assets"]; !ok {
		t.Error("assets key missing")
	}
}

// AN APP'S ICON IS A BLOCK, and the catalog carries it whole. A node's is a
// lucide name, so the obvious port reads it with `->>` and flattens it — which
// for an app that ships ART yields the empty string and a listing with no icon
// on it. This pins the Go side of that: the field is a raw JSON value, so a
// block survives the trip and is not stringified on the way out.
func TestCatalogIconIsCarriedWhole(t *testing.T) {
	e := CatalogEntry{Icon: json.RawMessage(`{"art":"icon.svg","color":"#101317"}`)}
	out, err := json.Marshal(map[string]any{"icon": e.Icon})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != `{"icon":{"art":"icon.svg","color":"#101317"}}` {
		t.Errorf("icon was not carried whole: %s", out)
	}
}
