package nodes

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tags never fail a listing and never come out nil, and a `null` inside the
// array is not a string — decoded straight into []string it would have shown
// on the shelf as an empty tag.
func TestDecodeTags(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"absent coalesces to empty array", `[]`, []string{}},
		{"null", `null`, []string{}},
		{"not an array", `{"a":1}`, []string{}},
		{"a number in the array", `["a", 1]`, []string{}},
		{"a null element is not a string", `[null, "a"]`, []string{}},
		{"normal", `["weather", "glance"]`, []string{"weather", "glance"}},
	} {
		got := decodeTags(json.RawMessage(tc.raw))
		if got == nil {
			t.Errorf("%s: got nil, want a non-nil slice", tc.name)
			continue
		}
		if len(got) != len(tc.want) || strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
