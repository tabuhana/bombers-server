package messaging

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A message has a kind: text, invite or image. These tests are about the one rule
// that makes the kinds worth having - a client cannot declare one - and about what
// the kind looks like on the wire.

// POST /messages is text-only, exactly as it was. A client that names any other
// kind is refused, and so is one that brings a payload: both are the shape of a
// client trying to put a claim into a message that the server did not mint. If
// this were allowed, an invite would be "a room id in a message" again, which is
// the shortcut that was deleted.
func TestSendRefusesAClientDeclaredKindOrPayload(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
	}{
		{"invite", map[string]any{"kind": "invite"}},
		{"invite with a room in the payload", map[string]any{"kind": "invite", "payload": map[string]any{"invite_id": "i1", "room": "r1", "room_name": "x"}}},
		{"image", map[string]any{"kind": "image"}},
		{"image with a payload", map[string]any{"kind": "image", "payload": map[string]any{"media_id": "m1"}}},
		{"an unknown word", map[string]any{"kind": "sticker"}},
		{"text in capitals", map[string]any{"kind": "TEXT"}},
		{"an empty kind", map[string]any{"kind": ""}},
		{"a payload on a text message", map[string]any{"kind": "text", "payload": map[string]any{"room": "r1"}}},
		{"a payload with no kind at all", map[string]any{"payload": map[string]any{"room": "r1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv("u1", "u2")
			e.db.befriend("u1", "u2")

			fields := map[string]any{"recipient_id": "u2", "body": "hi"}
			for k, v := range tc.fields {
				fields[k] = v
			}
			rec := e.do("u1", http.MethodPost, "/messages", jsonBody(t, fields))
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_body" {
				t.Errorf("got %d %s, want 400 invalid_body", rec.Code, rec.Body)
			}
			if len(e.db.msgs) != 0 || len(e.ln.pushes) != 0 {
				t.Error("a refused send stored or pushed something")
			}
		})
	}
}

// Saying "text" out loud is the same as not saying it: the default is the only
// thing a client can ask for, so asking for it is not an error. And a payload that
// is JSON null is no payload.
func TestSendAcceptsTextSaidOutLoud(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"kind text":         {"kind": "text"},
		"payload null":      {"payload": nil},
		"kind and no field": {},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv("u1", "u2")
			e.db.befriend("u1", "u2")
			fields := map[string]any{"recipient_id": "u2", "body": "hi"}
			for k, v := range extra {
				fields[k] = v
			}
			rec := e.do("u1", http.MethodPost, "/messages", jsonBody(t, fields))
			if rec.Code != http.StatusCreated {
				t.Fatalf("got %d %s, want 201", rec.Code, rec.Body)
			}
			if len(e.db.msgs) != 1 || e.db.msgs[0].Kind != "text" || e.db.msgs[0].Payload != nil {
				t.Errorf("stored = %+v, want one text message with no payload", e.db.msgs)
			}
		})
	}
}

// A text message says what kind it is, and says nothing of a payload - it is left
// out, not null - so a client that predates kinds sees exactly the shape it knew
// plus one field. This holds on every endpoint a message comes back from.
func TestATextMessageReadsBackAsTextWithNoPayload(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.seed("m1", "u1", "u2", "hello")

	check := func(where string, raw map[string]any) {
		t.Helper()
		if raw["kind"] != "text" {
			t.Errorf("%s: kind = %v, want text", where, raw["kind"])
		}
		if _, has := raw["payload"]; has {
			t.Errorf("%s: a text message carries a payload key: %v", where, raw["payload"])
		}
	}

	hist := asJSONBody(t, e.do("u2", http.MethodGet, "/messages/u1", ""))
	check("history", hist["messages"].([]any)[0].(map[string]any))
	feed := asJSONBody(t, e.do("u2", http.MethodGet, "/messages?since=0", ""))
	check("catch-up", feed["messages"].([]any)[0].(map[string]any))
	sent := asJSONBody(t, e.do("u1", http.MethodPost, "/messages", jsonBody(t, map[string]any{"recipient_id": "u2", "body": "again"})))
	check("send", sent)
	_, frame := e.ln.frame(t)
	check("dm frame", frame["message"].(map[string]any))
}

// asJSONBody decodes a recorded response into a generic map.
func asJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %q (%v)", rec.Body.String(), err)
	}
	return out
}
