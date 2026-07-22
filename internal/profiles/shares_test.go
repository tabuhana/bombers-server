package profiles

import (
	"encoding/json"
	"strings"
	"testing"
)

// The sharing rules that don't need a database: what a viewer is allowed to see
// once the grants are known, and the guards on a publish. The DB paths
// (replaceShares' friend filter, the grant queries) are exercised against a real
// server; these cover the logic that decides what leaves the building.

func TestRedactUnsharedHidesEverythingByDefault(t *testing.T) {
	bd := "1998-04-02"
	age := 28
	resp := profileResponse{
		DisplayName: "Sam",
		Bio:         "hello",
		Birthday:    &bd,
		Age:         &age,
		Country:     "CA",
		Timezone:    "America/Toronto",
		City:        "Montreal",
		Nickname:    "Sammy",
		Notes:       json.RawMessage(`{"favorites":{"name":"Favorites","items":["tea"]}}`),
	}

	redactUnshared(&resp, map[string]bool{}) // granted nothing

	if resp.Birthday != nil || resp.Age != nil {
		t.Errorf("birthday leaked: %v / %v", resp.Birthday, resp.Age)
	}
	if resp.Country != "" || resp.Timezone != "" || resp.City != "" {
		t.Errorf("location leaked: %q %q %q", resp.Country, resp.Timezone, resp.City)
	}
	if resp.Nickname != "" {
		t.Errorf("nickname leaked: %q", resp.Nickname)
	}
	if string(resp.Notes) != "{}" {
		t.Errorf("notes leaked: %s", resp.Notes)
	}
	// The base card is NOT governed by sharing - a friend still sees who you are.
	if resp.DisplayName != "Sam" || resp.Bio != "hello" {
		t.Errorf("base card was redacted: %q / %q", resp.DisplayName, resp.Bio)
	}
}

func TestRedactUnsharedKeepsGrantedFields(t *testing.T) {
	bd := "1998-04-02"
	age := 28
	resp := profileResponse{
		Birthday: &bd,
		Age:      &age,
		Country:  "CA",
		City:     "Montreal",
		Nickname: "Sammy",
		Notes: json.RawMessage(
			`{"favorites":{"name":"Favorites","items":["tea"]},"secrets":{"name":"Secrets","items":["shh"]}}`,
		),
	}

	// Granted the birthday and ONE note category, but not location, not
	// nickname, and not the other category.
	redactUnshared(&resp, map[string]bool{FieldBirthday: true, NotePrefix + "favorites": true})

	if resp.Birthday == nil || *resp.Birthday != bd {
		t.Errorf("granted birthday was redacted: %v", resp.Birthday)
	}
	if resp.Age == nil || *resp.Age != age {
		t.Errorf("age should ride along with the birthday grant: %v", resp.Age)
	}
	if !strings.Contains(string(resp.Notes), "favorites") {
		t.Errorf("the granted note category was redacted: %s", resp.Notes)
	}
	if strings.Contains(string(resp.Notes), "secrets") {
		t.Errorf("an UNgranted note category leaked: %s", resp.Notes)
	}
	if resp.Country != "" || resp.City != "" {
		t.Errorf("ungranted location survived: %q %q", resp.Country, resp.City)
	}
	if resp.Nickname != "" {
		t.Errorf("ungranted nickname survived: %q", resp.Nickname)
	}
}

// A field the client sends that the server doesn't know must be rejected, not
// silently stored - a grant nobody can read looks exactly like sharing that
// quietly doesn't work.
func TestKnownFields(t *testing.T) {
	// The fixed facts, plus any per-category note key.
	for _, f := range []string{
		FieldBirthday, FieldLocation, FieldNickname,
		NotePrefix + "favorites", NotePrefix + "a1b2-c3d4_e5",
	} {
		if !isKnownField(f) {
			t.Errorf("%q should be a known share field", f)
		}
	}
	// Anything else, including a category key that could smuggle punctuation
	// into a response, or an empty/oversized id.
	for _, f := range []string{
		"photos", "bio", "", "Birthday", "notes", "note:", "note:has space",
		"note:../etc", `note:{"x":1}`, NotePrefix + strings.Repeat("a", maxCategoryIDLen+1),
	} {
		if isKnownField(f) {
			t.Errorf("%q should NOT be a known share field", f)
		}
	}
}

func TestCountGrants(t *testing.T) {
	got := countGrants(map[string][]string{
		FieldBirthday:            {"a", "b", "c"},
		NotePrefix + "favorites": {"a"},
		FieldNickname:            {},
	})
	if got != 4 {
		t.Errorf("countGrants = %d, want 4", got)
	}
	if countGrants(map[string][]string{}) != 0 {
		t.Error("empty publish should count zero grants")
	}
}

func TestNotesOrEmptyNormalises(t *testing.T) {
	if string(notesOrEmpty(nil)) != "{}" {
		t.Error("nil notes should normalise to an empty object")
	}
	if string(notesOrEmpty([]byte{})) != "{}" {
		t.Error("empty notes should normalise to an empty object")
	}
	in := []byte(`{"favorites":{"items":["x"]}}`)
	if string(notesOrEmpty(in)) != string(in) {
		t.Error("real notes should pass through untouched")
	}
}
