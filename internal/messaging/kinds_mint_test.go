package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

// ---- minting an invite message ---------------------------------------------

func mintInviteBetween(t *testing.T, e *env) error {
	t.Helper()
	return e.h.MintInvite(context.Background(), "inv1", "room1", "umber-delta", "u1", "u2")
}

// Making an invite writes ONE message: from the inviter to the invitee, kind
// invite, carrying the invite's id, the room and the room's name as it was. Both
// people are told, as for any new message. The words are empty - the card is the
// payload.
func TestMintInviteWritesOneInviteMessageInTheConversation(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")

	if err := mintInviteBetween(t, e); err != nil {
		t.Fatal(err)
	}
	if len(e.db.msgs) != 1 {
		t.Fatalf("messages = %d, want exactly 1", len(e.db.msgs))
	}
	m := e.db.msgs[0]
	if m.Kind != kindInvite || m.SenderID != "u1" || m.RecipientID != "u2" || m.Body != "" {
		t.Errorf("stored = %+v, want an invite from u1 to u2 with no words", m)
	}
	var payload map[string]string
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		t.Fatalf("payload %q: %v", m.Payload, err)
	}
	if !reflect.DeepEqual(payload, map[string]string{"invite_id": "inv1", "room": "room1", "room_name": "umber-delta"}) {
		t.Errorf("payload = %v", payload)
	}

	// Told to both people, as one frame each, carrying kind and payload.
	for who, frame := range e.ln.frames(t) {
		msg := frame["message"].(map[string]any)
		pl, _ := msg["payload"].(map[string]any)
		if frame["op"] != "new" || msg["kind"] != "invite" || pl["room_name"] != "umber-delta" || pl["invite_id"] != "inv1" {
			t.Errorf("%s was told %v", who, frame)
		}
	}

	// And it is in the thread both people read, as an invite with its payload.
	for _, tc := range []struct{ who, peer string }{{"u2", "u1"}, {"u1", "u2"}} {
		hist := asJSONBody(t, e.do(tc.who, http.MethodGet, "/messages/"+tc.peer, ""))
		got := hist["messages"].([]any)
		if len(got) != 1 {
			t.Fatalf("%s: history has %d messages, want 1", tc.who, len(got))
		}
		msg := got[0].(map[string]any)
		if msg["kind"] != "invite" || msg["payload"].(map[string]any)["room"] != "room1" {
			t.Errorf("%s reads %v", tc.who, msg)
		}
	}
}

// The mint is friend-gated like every other write of a message: no friendship, no
// message, and the caller is told so that the invite can be voided.
func TestMintInviteRefusedWritesNothing(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(e *env) error{
		"not friends":  func(e *env) error { return e.h.MintInvite(ctx, "inv1", "room1", "x", "u1", "u3") },
		"no such user": func(e *env) error { return e.h.MintInvite(ctx, "inv1", "room1", "x", "u1", "ghost") },
		"to yourself":  func(e *env) error { return e.h.MintInvite(ctx, "inv1", "room1", "x", "u1", "u1") },
		"no invite id": func(e *env) error { return e.h.MintInvite(ctx, "", "room1", "x", "u1", "u2") },
		"no room":      func(e *env) error { return e.h.MintInvite(ctx, "inv1", "", "x", "u1", "u2") },
		"no room name": func(e *env) error { return e.h.MintInvite(ctx, "inv1", "room1", "", "u1", "u2") },
	}
	for name, do := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv("u1", "u2", "u3")
			e.db.befriend("u1", "u2")
			if err := do(e); err == nil {
				t.Error("want an error so the invite is voided")
			}
			if len(e.db.msgs) != 0 || len(e.ln.pushes) != 0 {
				t.Errorf("a refused mint wrote or pushed something: %+v %+v", e.db.msgs, e.ln.pushes)
			}
		})
	}
}

// A failed write is an error the caller can act on, and nothing was pushed.
func TestMintInviteReportsAWriteThatFailed(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	e.db.failOnQuery = insertMessageSQL

	if err := mintInviteBetween(t, e); err == nil {
		t.Fatal("a message that could not be written must be an error")
	}
	if len(e.db.msgs) != 0 || len(e.ln.pushes) != 0 {
		t.Error("a failed mint stored or pushed something")
	}
}

// A room name from a host is text a person typed: NUL would be refused by jsonb and
// be a 500 for the inviter, so it is cleaned before it is quoted.
func TestMintInviteCleansTheRoomName(t *testing.T) {
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	if err := e.h.MintInvite(context.Background(), "inv1", "room1", "ab\x00c", "u1", "u2"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(e.db.msgs[0].Payload), `"room_name":"abc"`) {
		t.Errorf("payload = %s", e.db.msgs[0].Payload)
	}
}

// ---- what a kind may carry --------------------------------------------------

func TestCheckKindPayload(t *testing.T) {
	good := `{"invite_id":"i","room":"r","room_name":"n"}`
	cases := []struct {
		name    string
		kind    string
		payload string
		want    error
	}{
		{"text, no payload", kindText, "", nil},
		{"text with a payload", kindText, `{}`, errBadPayload},
		{"invite", kindInvite, good, nil},
		{"invite with nothing", kindInvite, "", errBadPayload},
		{"invite that is not JSON", kindInvite, "room", errBadPayload},
		{"invite missing its room", kindInvite, `{"invite_id":"i","room_name":"n"}`, errBadPayload},
		{"invite missing its id", kindInvite, `{"room":"r","room_name":"n"}`, errBadPayload},
		{"invite missing its name", kindInvite, `{"invite_id":"i","room":"r"}`, errBadPayload},
		{"invite with a field of its own invention", kindInvite, `{"invite_id":"i","room":"r","room_name":"n","admin":true}`, errBadPayload},
		{"invite with a second document after it", kindInvite, good + good, errBadPayload},
		{"image, which nothing writes yet", kindImage, `{}`, errKindNotMintable},
		{"a word nothing renders", "sticker", `{}`, errUnknownKind},
		{"no kind at all", "", "", errUnknownKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkKindPayload(tc.kind, []byte(tc.payload))
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("checkKindPayload(%q, %q) = %v, want %v", tc.kind, tc.payload, err, tc.want)
			}
		})
	}
}

// Nothing reaches the table with a kind it should not have, even from inside this
// package: the insert refuses first, so the CHECK on the column is the last line
// and not the only one.
func TestInsertRefusesAnUnknownKindOrAWrongPayload(t *testing.T) {
	for name, m := range map[string]*messageRecord{
		"unknown kind":      {ID: "m1", SenderID: "a", RecipientID: "b", Kind: "sticker"},
		"no kind":           {ID: "m1", SenderID: "a", RecipientID: "b"},
		"text with payload": {ID: "m1", SenderID: "a", RecipientID: "b", Kind: kindText, Payload: []byte(`{"room":"r"}`)},
		"invite, no body":   {ID: "m1", SenderID: "a", RecipientID: "b", Kind: kindInvite},
	} {
		t.Run(name, func(t *testing.T) {
			db := newFakeDB("a", "b")
			if _, err := insertMessage(context.Background(), db, m); err == nil {
				t.Error("insertMessage accepted it")
			}
			if len(db.msgs) != 0 || len(db.callsTo(insertMessageSQL)) != 0 {
				t.Error("the statement ran")
			}
		})
	}
}

// ---- an invite message is not text ------------------------------------------

func mintedInvite(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv("u1", "u2")
	e.db.befriend("u1", "u2")
	if err := mintInviteBetween(t, e); err != nil {
		t.Fatal(err)
	}
	e.ln.pushes = nil
	return e, e.db.msgs[0].ID
}

// There are no words to edit on a card: an edit would put text on something the
// server made. It is the same opaque 404 as any message that cannot be edited, and
// it changes and pushes nothing.
func TestAnInviteMessageCannotBeEdited(t *testing.T) {
	e, id := mintedInvite(t)

	rec := e.do("u1", http.MethodPatch, "/messages/"+id, jsonBody(t, map[string]any{"body": "gotcha"}))
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "message_not_found" {
		t.Errorf("got %d %s, want 404 message_not_found", rec.Code, rec.Body)
	}
	if m := e.db.msgs[0]; m.Body != "" || m.EditedAt != nil || len(e.ln.pushes) != 0 {
		t.Errorf("the edit changed or pushed something: %+v %+v", m, e.ln.pushes)
	}
}

// The sender can still take it back. A tombstone says nothing, so the payload goes
// with the body; the kind stays, and the payload key is left out like it is for text.
func TestDeletingAnInviteMessageClearsItsPayload(t *testing.T) {
	e, id := mintedInvite(t)

	rec := e.do("u1", http.MethodDelete, "/messages/"+id, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	m := e.db.msgs[0]
	if m.DeletedAt == nil || m.Payload != nil || m.Kind != kindInvite {
		t.Errorf("stored = %+v, want a tombstone that is still an invite and carries nothing", m)
	}
	_, frame := e.ln.frame(t)
	msg := frame["message"].(map[string]any)
	if _, has := msg["payload"]; has || msg["kind"] != "invite" {
		t.Errorf("the delete frame says %v", msg)
	}
}

// ---- the SQL and the migration, pinned as text ------------------------------

func TestSQLKindAndPayloadAreInTheColumnsAndTheWritesThatMatter(t *testing.T) {
	mustContain(t, "messageColumns", messageColumns, "kind, payload")
	mustContain(t, "insertMessageSQL", insertMessageSQL, "kind, payload) VALUES", "$7::text, $8::jsonb")
	// Only text can be edited, in the lookup and in the statement itself.
	mustContain(t, "ownMessageRecipientSQL", ownMessageRecipientSQL, "AND kind = 'text'")
	mustContain(t, "editMessageSQL", editMessageSQL, "AND kind = 'text'")
	// Nothing but the insert writes a kind, and only the delete clears a payload.
	for name, sql := range map[string]string{
		"editMessageSQL": editMessageSQL, "pinMessageSQL": pinMessageSQL, "unpinMessageSQL": unpinMessageSQL, "touchMessageSQL": touchMessageSQL,
	} {
		set, _, _ := strings.Cut(sql, "WHERE") // what it writes, not what it matches
		mustNotContain(t, name, set, "kind =", "payload =")
	}
	mustContain(t, "deleteMessageSQL", deleteMessageSQL, "payload = NULL")
	mustNotContain(t, "deleteMessageSQL", deleteMessageSQL, "kind =")
}

// The migration only ADDS, defaults every existing row to text, and its CHECK
// names exactly the kinds the code knows.
func TestTheMigrationAddsTheKindColumnsAndOnlyThat(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/20261022120000_message_kinds.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(raw), "-- +goose Down")
	var code []string
	for _, line := range strings.Split(up, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code = append(code, line)
		}
	}
	norm := strings.Join(strings.Fields(strings.Join(code, "\n")), " ")
	mustContain(t, "migration", norm,
		"ADD COLUMN kind TEXT NOT NULL DEFAULT 'text'",
		"ADD COLUMN payload JSONB;", // nullable, no default: text has none
		"CHECK (kind IN ('text', 'invite', 'image'))",
	)
	mustNotContain(t, "migration up", strings.ToUpper(norm), "UPDATE ", "DELETE ", "DROP ", "INSERT ")
	for _, k := range []string{kindText, kindInvite, kindImage} {
		if !strings.Contains(norm, "'"+k+"'") {
			t.Errorf("the CHECK does not name %q", k)
		}
	}
}
