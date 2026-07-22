package profiles

import (
	"encoding/json"
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
		Notes:       json.RawMessage(`[{"text":"likes tea"}]`),
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
	if string(resp.Notes) != "[]" {
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
		Notes:    json.RawMessage(`[{"text":"likes tea"}]`),
	}

	// Granted the birthday and the notes, but NOT location or nickname.
	redactUnshared(&resp, map[string]bool{FieldBirthday: true, FieldNotes: true})

	if resp.Birthday == nil || *resp.Birthday != bd {
		t.Errorf("granted birthday was redacted: %v", resp.Birthday)
	}
	if resp.Age == nil || *resp.Age != age {
		t.Errorf("age should ride along with the birthday grant: %v", resp.Age)
	}
	if string(resp.Notes) == "[]" {
		t.Error("granted notes were redacted")
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
func TestKnownFieldsIsClosed(t *testing.T) {
	for _, f := range []string{FieldBirthday, FieldLocation, FieldNickname, FieldNotes} {
		if !knownFields[f] {
			t.Errorf("%q should be a known share field", f)
		}
	}
	for _, f := range []string{"photos", "bio", "", "Birthday", "notes "} {
		if knownFields[f] {
			t.Errorf("%q should NOT be a known share field", f)
		}
	}
}

func TestCountGrants(t *testing.T) {
	got := countGrants(map[string][]string{
		FieldBirthday: {"a", "b", "c"},
		FieldNotes:    {"a"},
		FieldNickname: {},
	})
	if got != 4 {
		t.Errorf("countGrants = %d, want 4", got)
	}
	if countGrants(map[string][]string{}) != 0 {
		t.Error("empty publish should count zero grants")
	}
}

func TestNotesOrEmptyNormalises(t *testing.T) {
	if string(notesOrEmpty(nil)) != "[]" {
		t.Error("nil notes should normalise to an empty array")
	}
	if string(notesOrEmpty([]byte{})) != "[]" {
		t.Error("empty notes should normalise to an empty array")
	}
	in := []byte(`[{"text":"x"}]`)
	if string(notesOrEmpty(in)) != string(in) {
		t.Error("real notes should pass through untouched")
	}
}
