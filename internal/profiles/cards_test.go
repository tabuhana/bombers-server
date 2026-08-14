package profiles

import (
	"encoding/json"
	"strings"
	"testing"
)

// The card is opaque, and "opaque" has to mean byte-for-byte. The client is the
// only half that knows what a card means — which categories exist, which notes
// departed from their category's audience — so anything this server changes on
// the way through is a change nobody asked for and nobody can see until a
// friend's card renders wrong.
func TestCardResponsePassesContentThroughUnchanged(t *testing.T) {
	// Deliberately awkward: key order a Go map would not preserve, a number Go
	// would reformat, a unicode escape, and a shape the server models nowhere.
	content := json.RawMessage(`{"zzz":1,"aaa":2,"n":1.50,"u":"é","odd":[{"deep":{"deeper":null}}]}`)

	out := cardResponse("01JABC", content)

	if !strings.Contains(string(out), string(content)) {
		t.Fatalf("content was rewritten on the way out:\n got %s\nwant it to contain %s", out, content)
	}
	// Still one valid document, and the owner is where a reader expects it.
	var parsed struct {
		OwnerID string          `json:"owner_id"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v (%s)", err, out)
	}
	if parsed.OwnerID != "01JABC" {
		t.Errorf("owner_id = %q, want 01JABC", parsed.OwnerID)
	}
}

// An empty stored card must not produce `"content":}`. It shouldn't happen —
// the column is NOT NULL and a publish always writes a body — but the failure
// mode is a response no client can parse, so it costs nothing to be sure.
func TestCardResponseSurvivesEmptyContent(t *testing.T) {
	out := cardResponse("01JABC", nil)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("empty content produced invalid JSON: %v (%s)", err, out)
	}
	if parsed["content"] != nil {
		t.Errorf("content = %v, want null", parsed["content"])
	}
}

// The owner id goes through json.Marshal rather than string concatenation.
// It arrives from a URL parameter, so a quote in it would otherwise end the
// string early and let the rest of the path write its own JSON keys.
func TestCardResponseEscapesTheOwnerID(t *testing.T) {
	out := cardResponse(`","content":"stolen`, json.RawMessage(`{"ok":true}`))
	var parsed struct {
		OwnerID string          `json:"owner_id"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("a quoted owner id broke the response: %v (%s)", err, out)
	}
	if string(parsed.Content) != `{"ok":true}` {
		t.Errorf("content was displaced by the owner id: %s", parsed.Content)
	}
}
