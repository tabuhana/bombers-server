package engines

import (
	"encoding/json"
	"strings"
	"testing"
)

// AN ASSET KEY IS AN ID AND A PATH, and between them they are the whole of what
// could escape `engines/`. A traversal through a published bundle would be the
// worst kind of bug, so both halves are checked before anything touches storage.
func TestValidEngineID(t *testing.T) {
	ok := []string{"physics", "core:physics", "sdk:my-engine", "a_b.c", "x"}
	bad := []string{
		"",                // nothing
		"engines/physics", // a separator is a path
		"core\\physics",   // the other separator
		"..",              // the dot segment itself
		"core:..:physics", // and one buried in a namespace
		".hidden",         // a hidden entry on the filesystem backend
		"core physics",    // a space
		"core%3Aphysics",  // still encoded: the handler decodes first
	}
	for _, id := range ok {
		if !ValidEngineID(id) {
			t.Errorf("ValidEngineID(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if ValidEngineID(id) {
			t.Errorf("ValidEngineID(%q) = true, want false", id)
		}
	}
	// 128 is the cap, so one past it is refused.
	if ValidEngineID(strings.Repeat("a", 129)) {
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
// different engines name one object, which is the one thing an object key must not
// allow — the id is kept safe by `ValidEngineID` instead.
func TestAssetKeyKeepsTheNamespace(t *testing.T) {
	if got := AssetKey("core:physics", "art/icon.svg"); got != "engines/core:physics/art/icon.svg" {
		t.Errorf("AssetKey = %q", got)
	}
	// The prefix a publish and an unpublish clear. The trailing slash is what
	// stops one id taking another's bytes (`physics` vs `physics2`).
	if got := AssetKey("physics", ""); got != "engines/physics/" {
		t.Errorf("AssetKey prefix = %q", got)
	}
}

// THE STORES DO NOT SHARE AN OBJECT FOLDER, which is what lets an engine carry
// the same id as an app, node or game — their bytes cannot collide, so
// unpublishing either leaves the other's alone. The prefix is `engines/<id>/`,
// never `apps/`.
func TestEngineKeysCannotCollide(t *testing.T) {
	key := AssetKey("core:engine-2d", "art/icon.svg")
	for _, other := range []string{"apps/", "nodes/", "activities/", "packs/"} {
		if strings.HasPrefix(key, other) {
			t.Fatalf("an engine's asset landed in the %s folder: %q", other, key)
		}
	}
	if !strings.HasPrefix(key, "engines/core:engine-2d/") {
		t.Fatalf("AssetKey lost its prefix: %q", key)
	}
	if got := AssetKey("2d", ""); got != "engines/2d/" {
		t.Errorf("AssetKey prefix = %q, want engines/2d/", got)
	}
}

// AN ID IS HALF A KEY, so a publish refuses one that could become a path —
// where the operator is standing, rather than at the upload that follows.
func TestPublishRefusesAnIDThatIsAPath(t *testing.T) {
	if _, err := parseBundle([]byte(`{"manifest":{"id":"a/../b","name":"X"},"files":{"screen.jsx":"1"}}`)); err == nil {
		t.Fatal("a bundle whose id is a path was accepted")
	}
	if _, err := parseBundle([]byte(`{"manifest":{"id":"core:physics","name":"X"},"files":{"screen.jsx":"1"}}`)); err != nil {
		t.Fatalf("a namespaced id was refused: %v", err)
	}
}

// THE SERVER DOES NOT KNOW WHAT AN ENGINE IS, and must not learn. The engine's
// type is the client's vocabulary; the bundle is stored opaquely, so a new type
// is a client release and never a deploy. A `parseBundle` that started checking
// `engine` would make every future engine type a migration.
func TestPublishDoesNotJudgeTheManifest(t *testing.T) {
	// No `engine` at all — nothing an engine is supposed to have.
	info, err := parseBundle([]byte(`{"manifest":{"id":"bare","name":"Bare"},"files":{"main.js":"x"}}`))
	if err != nil {
		t.Fatalf("a manifest the CLIENT would refuse was refused here too: %v", err)
	}
	if info.ID != "bare" || info.Name != "Bare" {
		t.Errorf("got %+v", info)
	}
	// And a type nobody has invented yet, or one of the wrong JSON type, is
	// still the client's to judge.
	for _, engine := range []string{`"3d"`, `"a type from next year"`, `7`, `{"x":1}`, `null`} {
		body := `{"manifest":{"id":"e","name":"E","engine":` + engine + `},"files":{"main.js":"x"}}`
		if _, err := parseBundle([]byte(body)); err != nil {
			t.Errorf("engine %s was judged by the server: %v", engine, err)
		}
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
// of this route IS the bundle, matching the app store the client already
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
	// An engine with no art serialises as an empty list, never null: a client
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

// THE CATALOGUE QUERY LIFTS `engine` AND `description` AS TEXT and coalesces
// both to "" so a minimal manifest still lists. It carries no icon and no tags:
// an engine is fetched because a game needs it, never browsed on a shelf. The
// query needs a database to run, so this pins its text, as the shape that
// matters here.
func TestCatalogSQLLiftsEngineAndDescription(t *testing.T) {
	for _, want := range []string{
		`COALESCE(bundle->'manifest'->>'engine', '')`,
		`COALESCE(bundle->'manifest'->>'description', '')`,
		`COALESCE(bundle->'manifest'->>'requiresApp', '')`,
		`FROM engines`,
		`ORDER BY name`,
	} {
		if !strings.Contains(catalogSQL, want) {
			t.Errorf("catalogSQL is missing %q", want)
		}
	}
	for _, unwanted := range []string{"icon", "tags"} {
		if strings.Contains(catalogSQL, unwanted) {
			t.Errorf("catalogSQL reads %q, which an engine listing does not carry", unwanted)
		}
	}
}

// THE CATALOGUE WIRE SHAPE is exactly these keys: no icon, no tags. requires_app
// is omitted when empty, as the app catalogue's is.
func TestCatalogResponseKeys(t *testing.T) {
	full, err := json.Marshal(catalogResponse{ID: "e", Engine: "2d", RequiresApp: "1.2.0"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(full, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"id", "name", "version", "engine", "description", "updated_at", "requires_app"} {
		if _, ok := m[k]; !ok {
			t.Errorf("catalogue entry has no %q key: %s", k, full)
		}
	}
	if len(m) != 7 {
		t.Errorf("catalogue entry has %d keys, want 7: %s", len(m), full)
	}
	bare, _ := json.Marshal(catalogResponse{ID: "e"})
	if strings.Contains(string(bare), "requires_app") {
		t.Errorf("an empty requires_app was not omitted: %s", bare)
	}
}
