package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A STORE THAT SERVES ASSETS MUST ALSO ACCEPT THEM.
//
// This test is here because the node store served `GET /nodes/{id}/assets/*`
// for weeks with no `PUT` beside it (2026-11-10). `nodes.Handler.PublishAsset`
// was written, `node_assets` was migrated, the client uploaded one file per
// node — and every upload came back `405 Method Not Allowed`, because chi
// answers an unregistered verb on a REGISTERED path that way rather than with a
// 404. Nothing was broken enough to look broken: the bundle published fine, the
// catalogue listed the node, and the store simply served a manifest naming
// `icon.svg` with no such file behind it, so every installed copy drew a blank
// face.
//
// It reads the SOURCE rather than the router, because building the router needs
// a database pool and this has to run in `go test ./...` on a laptop with no
// Postgres. That is a real limit: it checks that the line is written, not that
// chi accepted it. The route's own behaviour is `internal/nodes`' to test.
func TestEveryAssetRouteCanBePublishedTo(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)

	// The two halves of the same path, as they are actually written. The store's
	// name is the capture, so a store added later is picked up with no edit here.
	serves := routeStores(text, `r\.Get\("/([a-z-]+)/\{id\}/assets/\*"`)
	accepts := routeStores(text, `r\.Put\("/([a-z-]+)/\{id\}/assets/\*"`)

	if len(serves) == 0 {
		t.Fatal("no asset routes found at all — this test has stopped matching the source")
	}

	for store := range serves {
		if !accepts[store] {
			t.Errorf(
				"/%s/{id}/assets/* is served but has no PUT — an upload to it gets 405, "+
					"and the store will serve a manifest naming art it does not hold",
				store,
			)
		}
	}
	// And the reverse, which would be a route an operator can write to and
	// nobody can read: bytes accepted into storage that nothing can serve back.
	for store := range accepts {
		if !contains(serves, store) {
			t.Errorf("/%s/{id}/assets/* accepts a PUT but is never served", store)
		}
	}
}

func routeStores(text, pattern string) map[string]bool {
	found := map[string]bool{}
	for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(text, -1) {
		found[m[1]] = true
	}
	return found
}

func contains(set map[string]bool, name string) bool { return set[name] }

// The stores the test above found, named out loud, so that a store silently
// losing BOTH of its asset routes — which the paired check cannot see — fails
// here instead of passing quietly.
func TestAssetRoutesCoverEveryStoreThatHasArt(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	got := sortedKeys(routeStores(string(source), `r\.Get\("/([a-z-]+)/\{id\}/assets/\*"`))
	want := []string{"activities", "nodes", "packs"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stores serving assets = %v, want %v", got, want)
	}
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
