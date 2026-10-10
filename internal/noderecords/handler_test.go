package noderecords

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/tabuhana/bombers-server/internal/auth"
)

// The guest list a client submits is not the one that gets stored: it is
// trimmed, de-duplicated, and has the owner taken out of it. Naming yourself is
// a no-op rather than an error, because you always see your own record, and a
// duplicate id would mean nudging the same person twice and showing them twice
// in a pile.
func TestNormalizeAudience(t *testing.T) {
	cases := []struct {
		name  string
		ids   []string
		owner string
		want  []string
	}{
		{"a plain list is kept, in order", []string{"b", "a"}, "me", []string{"b", "a"}},
		{"duplicates collapse", []string{"a", "a", "b", "a"}, "me", []string{"a", "b"}},
		{"the owner is dropped", []string{"a", "me", "b"}, "me", []string{"a", "b"}},
		{"whitespace is trimmed", []string{" a ", "\tb"}, "me", []string{"a", "b"}},
		{"empty entries are dropped", []string{"", "  ", "a"}, "me", []string{"a"}},
		{"addressed to nobody is allowed", []string{}, "me", []string{}},
		{"only yourself is the same as nobody", []string{"me", "me"}, "me", []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, tooMany := normalizeAudience(tc.ids, tc.owner)
			if tooMany {
				t.Fatalf("normalizeAudience(%v) reported too many", tc.ids)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("normalizeAudience(%v) = %v, want %v", tc.ids, got, tc.want)
			}
		})
	}
}

// The cap exists so a bug can't write an unbounded array, and it is counted
// BEFORE de-duplication: a list of ten thousand copies of one id is still a
// client doing something wrong.
func TestNormalizeAudienceCap(t *testing.T) {
	ids := make([]string, maxAudience+1)
	for i := range ids {
		ids[i] = "same"
	}
	if _, tooMany := normalizeAudience(ids, "me"); !tooMany {
		t.Errorf("an audience of %d was accepted; the cap is %d", len(ids), maxAudience)
	}

	ok := make([]string, maxAudience)
	for i := range ok {
		ok[i] = string(rune('a' + i%26))
	}
	if _, tooMany := normalizeAudience(ok, "me"); tooMany {
		t.Errorf("an audience of exactly %d was refused", maxAudience)
	}
}

// Editing a record has to tell the people who are NO LONGER on the list, or
// their copy sits there until something else makes them look. That is the union
// of the new audience and the old one.
func TestUnionTellsBothLists(t *testing.T) {
	got := union([]string{"a", "b"}, []string{"b", "c", ""})
	want := []string{"a", "b", "c"}
	if !slices.Equal(got, want) {
		t.Errorf("union = %v, want %v", got, want)
	}
}

// Nudging everyone about an answer must not quietly rewrite the audience the
// header holds — the header is read again for the owner id straight afterwards.
func TestAudienceWithoutDoesNotMutate(t *testing.T) {
	head := &header{OwnerID: "owner", Audience: []string{"a", "b", "c"}}
	got := audienceWithout(head, "b")
	if want := []string{"a", "c"}; !slices.Equal(got, want) {
		t.Errorf("audienceWithout = %v, want %v", got, want)
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(head.Audience, want) {
		t.Errorf("the header's audience was modified: %v", head.Audience)
	}
}

// A body is stored as an opaque blob, so the one thing worth checking is that
// it is an OBJECT — a shape a node can add fields to in a later version without
// everything that reads it having to change.
func TestIsJSONObject(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{"title":"dinner"}`, true},
		{"  \n\t{}", true},
		{`[1,2,3]`, false},
		{`"a string"`, false},
		{`null`, false},
		{`42`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := isJSONObject(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("isJSONObject(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// Reading is always on behalf of ONE node. A missing `?node=` is a client bug,
// and answering it with every node's records would be the server deciding to
// hand over more than it was asked for — so it is refused before the pool is
// ever touched (which is also why a nil pool is safe here).
func TestListNeedsANodeID(t *testing.T) {
	h := NewHandler(nil, nil)
	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"no node at all", "", http.StatusBadRequest},
		{"an empty node", "?node=", http.StatusBadRequest},
		{"whitespace only", "?node=%20%20", http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/node-records/received"+tc.query, nil)
			r = r.WithContext(auth.WithUserID(r.Context(), "user-1"))
			w := httptest.NewRecorder()

			h.ListReceived(w, r)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

// Every read and write needs a signed-in caller; without one it is a 401 and
// nothing is looked up.
func TestListNeedsAuth(t *testing.T) {
	h := NewHandler(nil, nil)
	r := httptest.NewRequest(http.MethodGet, "/node-records/received?node=events", nil)
	w := httptest.NewRecorder()

	h.ListReceived(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// The nudge says "something changed", to the people it concerns, and never to
// the person who just changed it — their own device already knows, and its
// other windows re-read on their own.
func TestNudgeSkipsSelfAndDuplicates(t *testing.T) {
	var gotIDs []string
	var gotNode, gotFrom string
	calls := 0
	h := NewHandler(nil, func(userIDs []string, nodeID, fromID string) {
		calls++
		gotIDs, gotNode, gotFrom = userIDs, nodeID, fromID
	})

	h.nudge([]string{"a", "me", "b", "a", ""}, "events", "me")
	if calls != 1 {
		t.Fatalf("notify called %d times, want 1", calls)
	}
	if want := []string{"a", "b"}; !slices.Equal(gotIDs, want) {
		t.Errorf("nudged %v, want %v", gotIDs, want)
	}
	if gotNode != "events" || gotFrom != "me" {
		t.Errorf("nudge carried node=%q from=%q", gotNode, gotFrom)
	}

	// Nobody left to tell is silence, not an empty send.
	calls = 0
	h.nudge([]string{"me"}, "events", "me")
	if calls != 0 {
		t.Errorf("notify called %d times for an audience of just the writer", calls)
	}
}

// A handler built with no notifier (which is how these tests build one, and how
// the server runs with nobody listening) must not panic when it writes.
func TestNudgeWithoutANotifier(t *testing.T) {
	NewHandler(nil, nil).nudge([]string{"a"}, "events", "me")
}
