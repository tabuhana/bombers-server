package activities

import (
	"encoding/json"
	"strings"
	"testing"
)

// listed runs a stored bundle through the same step the catalogue uses and
// returns the entry as it would go out on the wire.
func listed(t *testing.T, bundle string) (catalogEntry, map[string]json.RawMessage) {
	t.Helper()
	entry := catalogEntry{ID: "g", Name: "G", Version: "1.0.0"}
	applyManifest(&entry, []byte(bundle), map[string]bool{})
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return entry, keys
}

func TestListingCarriesRequiresEngine(t *testing.T) {
	_, keys := listed(t, `{"manifest":{"requiresEngine":{"id":"core:engine-2d","version":"1.2.0"}},"files":{"a":"b"}}`)
	got, ok := keys["requires_engine"]
	if !ok {
		t.Fatal("requires_engine is missing from the listing")
	}
	if string(got) != `{"id":"core:engine-2d","version":"1.2.0"}` {
		t.Errorf("requires_engine = %s", got)
	}
}

func TestListingOmitsRequiresEngineWhenNotDeclared(t *testing.T) {
	_, keys := listed(t, `{"manifest":{"description":"d"},"files":{"a":"b"}}`)
	if _, ok := keys["requires_engine"]; ok {
		t.Error("a game with no requiresEngine listed a requires_engine key")
	}
}

// A malformed requiresEngine drops that one key and nothing else: the rest of
// the manifest must still list.
func TestMalformedRequiresEngineDropsOnlyItself(t *testing.T) {
	cases := map[string]string{
		"a string":                `"core:engine-2d"`,
		"a number":                `3`,
		"an array":                `["core:engine-2d"]`,
		"null":                    `null`,
		"an object, no id":        `{"version":"1.0.0"}`,
		"an object, blank":        `{"id":"   ","version":"1.0.0"}`,
		"an id of the wrong type": `{"id":7}`,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			bundle := `{"manifest":{"description":"still here","players":{"min":2,"max":4},"requiresEngine":` + value + `},"files":{"a":"b"}}`
			entry, keys := listed(t, bundle)
			if _, ok := keys["requires_engine"]; ok {
				t.Errorf("requires_engine was listed for %s", value)
			}
			if entry.Description != "still here" {
				t.Errorf("description = %q, lost to a bad requiresEngine", entry.Description)
			}
			if entry.Players == nil || entry.Players.Min != 2 || entry.Players.Max != 4 {
				t.Errorf("players = %+v, lost to a bad requiresEngine", entry.Players)
			}
		})
	}
}

// The id is trimmed before it is judged and before it is passed on.
func TestRequiresEngineIDIsTrimmed(t *testing.T) {
	entry, _ := listed(t, `{"manifest":{"requiresEngine":{"id":"  e  ","version":"2"}},"files":{"a":"b"}}`)
	if entry.RequiresEngine == nil || entry.RequiresEngine.ID != "e" || entry.RequiresEngine.Version != "2" {
		t.Errorf("RequiresEngine = %+v", entry.RequiresEngine)
	}
}

// The server never checks the engine exists and never compares versions: a
// version it cannot parse passes through untouched.
func TestRequiresEngineIsPassedThroughUnjudged(t *testing.T) {
	entry, _ := listed(t, `{"manifest":{"requiresEngine":{"id":"nobody-published-this","version":"not a version"}},"files":{"a":"b"}}`)
	if entry.RequiresEngine == nil || !strings.Contains(entry.RequiresEngine.Version, "not a version") {
		t.Errorf("RequiresEngine = %+v", entry.RequiresEngine)
	}
}

// A game's art is still only advertised when it was really shipped.
func TestApplyManifestStillGatesArtOnWhatShipped(t *testing.T) {
	entry := catalogEntry{}
	applyManifest(&entry, []byte(`{"manifest":{"icon":"i.png","cover":"c.png"}}`), map[string]bool{"i.png": true})
	if entry.Icon != "i.png" || entry.Cover != "" {
		t.Errorf("icon=%q cover=%q", entry.Icon, entry.Cover)
	}
}
