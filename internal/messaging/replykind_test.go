package messaging

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
)

// A reply's preview says WHAT it quotes: the original's kind, from the closed set a
// message's kind is from, always present, and kept by a tombstone. These tests drive a
// reply to each sort of original down every path that returns a reply_to and look at the
// raw JSON a client would read.

// quoted is an original to reply to, and what its preview must say.
type quoted struct {
	name    string
	make    func(t *testing.T, e *env) string // returns the original's id
	delete  bool
	kind    string
	body    string
	deleted bool
}

func quotedOriginals() []quoted {
	words := func(t *testing.T, e *env) string { return e.db.seed("orig", "u1", "u2", "the words").ID }
	captionedPic := func(t *testing.T, e *env) string { return captioned(t, e, "a caption") }
	barePic := func(t *testing.T, e *env) string { return sentImage(t, e, testPNG(t, 8, 8)) }
	card := func(t *testing.T, e *env) string {
		if err := e.h.MintInvite(context.Background(), "inv1", "room1", "umber", "u1", "u2"); err != nil {
			t.Fatal(err)
		}
		return e.db.msgs[len(e.db.msgs)-1].ID
	}
	return []quoted{
		{"words", words, false, "text", "the words", false},
		{"a picture with a caption", captionedPic, false, "image", "a caption", false},
		{"a picture with no words", barePic, false, "image", "", false},
		{"an invite card", card, false, "invite", "", false},
		{"deleted words", words, true, "text", "", true},
		{"a deleted captioned picture", captionedPic, true, "image", "", true},
		{"a deleted bare picture", barePic, true, "image", "", true},
		{"a deleted invite card", card, true, "invite", "", true},
	}
}

// replyObject is the reply_to of a message as JSON, checked to be the whole of what the
// preview is: exactly these five keys, and no payload.
func replyObject(t *testing.T, where string, msg map[string]any) map[string]any {
	t.Helper()
	rt, ok := msg["reply_to"].(map[string]any)
	if !ok {
		t.Fatalf("%s: reply_to = %v, want an object", where, msg["reply_to"])
	}
	keys := make([]string, 0, len(rt))
	for k := range rt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"body", "deleted", "id", "kind", "sender_id"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("%s: reply_to has keys %v, want %v", where, keys, want)
	}
	return rt
}

func messageNamed(t *testing.T, where string, list any, id string) map[string]any {
	t.Helper()
	items, _ := list.([]any)
	for _, it := range items {
		if m, _ := it.(map[string]any); m != nil && m["id"] == id {
			return m
		}
	}
	t.Fatalf("%s: message %s is not in %v", where, id, list)
	return nil
}

// Every path that returns a message returns the reply's kind with it.
func TestAReplyPreviewSaysWhatItQuotesOnEveryPath(t *testing.T) {
	for _, q := range quotedOriginals() {
		t.Run(q.name, func(t *testing.T) {
			e := imageEnv()
			origID := q.make(t, e)
			if q.delete {
				if rec := e.do("u1", http.MethodDelete, "/messages/"+origID, ""); rec.Code != http.StatusNoContent {
					t.Fatalf("delete = %d", rec.Code)
				}
			}

			check := func(where string, msg map[string]any) {
				t.Helper()
				rt := replyObject(t, where, msg)
				if rt["id"] != origID || rt["kind"] != q.kind || rt["body"] != q.body || rt["deleted"] != q.deleted {
					t.Errorf("%s: reply_to = %v, want kind %q body %q deleted %v", where, rt, q.kind, q.body, q.deleted)
				}
			}

			// The send: its answer, and the frame each of the two people is pushed.
			e.ln.pushes = nil
			rec := sendAs(t, e, "u2", "u1", "findable reply", map[string]any{"reply_to_id": origID})
			if rec.Code != http.StatusCreated {
				t.Fatalf("reply = %d %s", rec.Code, rec.Body)
			}
			replyID := decode[sendResponse](t, rec).ID
			check("send response", asJSONBody(t, rec))
			for who, frame := range e.ln.frames(t) {
				check("new frame to "+who, frame["message"].(map[string]any))
			}

			// Reads, as the other person and as the replier.
			for _, who := range []string{"u1", "u2"} {
				peer := map[string]string{"u1": "u2", "u2": "u1"}[who]
				hist := asJSONBody(t, e.do(who, http.MethodGet, "/messages/"+peer, ""))
				check(who+" history", messageNamed(t, who+" history", hist["messages"], replyID))

				around := asJSONBody(t, e.do(who, http.MethodGet, "/messages/"+peer+"?around="+replyID, ""))
				check(who+" around", messageNamed(t, who+" around", around["messages"], replyID))

				feed := asJSONBody(t, e.do(who, http.MethodGet, "/messages?since=0", ""))
				check(who+" catch-up", messageNamed(t, who+" catch-up", feed["messages"], replyID))

				found := asJSONBody(t, e.search(who, "findable", ""))
				check(who+" search", messageNamed(t, who+" search", found["messages"], replyID))

				list := asJSONBody(t, e.do(who, http.MethodGet, "/conversations", ""))
				last := list["conversations"].([]any)[0].(map[string]any)["last_message"].(map[string]any)
				if last["id"] != replyID {
					t.Fatalf("%s conversations: last message is %v, want the reply", who, last["id"])
				}
				check(who+" conversation list", last)
			}

			// A pin: the pin's answer, its frames and the pin list.
			e.ln.pushes = nil
			if rec := e.pin("u1", replyID); rec.Code != http.StatusOK {
				t.Fatalf("pin = %d %s", rec.Code, rec.Body)
			} else {
				check("pin response", asJSONBody(t, rec))
			}
			for who, frame := range e.ln.frames(t) {
				check("pin frame to "+who, frame["message"].(map[string]any))
			}
			pins := asJSONBody(t, e.do("u1", http.MethodGet, "/messages/u2/pins", ""))
			check("pin list", messageNamed(t, "pin list", pins["messages"], replyID))

			// An edit of the reply, and a reaction to it.
			e.ln.pushes = nil
			rec = patch(t, e, "u2", replyID, "findable reply, edited")
			if rec.Code != http.StatusOK {
				t.Fatalf("edit = %d %s", rec.Code, rec.Body)
			}
			check("edit response", asJSONBody(t, rec))
			for who, frame := range e.ln.frames(t) {
				check("edit frame to "+who, frame["message"].(map[string]any))
			}
			e.ln.pushes = nil
			if rec := reactAs(t, e, "u1", replyID, "👍"); rec.Code != http.StatusOK {
				t.Fatalf("react = %d %s", rec.Code, rec.Body)
			} else {
				check("react response", asJSONBody(t, rec))
			}
			for who, frame := range e.ln.frames(t) {
				check("react frame to "+who, frame["message"].(map[string]any))
			}
		})
	}
}

// A message that answers nothing still has reply_to null: the kind lives inside a
// preview, and there is no preview to put it in.
func TestAMessageThatAnswersNothingHasNoPreviewAtAll(t *testing.T) {
	e := imageEnv()
	rec := sendAs(t, e, "u1", "u2", "plain", nil)
	if got := asJSONBody(t, rec); got["reply_to"] != nil {
		t.Errorf("reply_to = %v, want null", got["reply_to"])
	}
}

// The preview is the original's id, sender, words, kind and whether it is deleted, and
// nothing about how to draw it. A payload here would be the start of a second copy of
// the message, and a client that wants one fetches the original.
func TestAReplyPreviewCarriesNoPayload(t *testing.T) {
	e := imageEnv()
	orig := captioned(t, e, "a caption")
	rec := sendAs(t, e, "u2", "u1", "re", map[string]any{"reply_to_id": orig})
	rt := replyObject(t, "send response", asJSONBody(t, rec))
	if _, has := rt["payload"]; has {
		t.Errorf("reply_to carries a payload: %v", rt)
	}
	for _, k := range []string{"width", "height", "mime", "size"} {
		if _, has := rt[k]; has {
			t.Errorf("reply_to carries %s: %v", k, rt)
		}
	}
}

// The kind is never absent and never null, even for the zero value of the struct: a
// client does not have to tell "absent" from "null" from a word.
func TestAReplyPreviewAlwaysHasAKindKey(t *testing.T) {
	raw, err := json.Marshal(replyPreview{})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if k, has := got["kind"]; !has || k == nil {
		t.Errorf("an empty preview marshals as %s: kind must be present and not null", raw)
	}
}

// toReplyPreview passes the original's kind through for each of them, and a tombstone
// loses its words and keeps its kind.
func TestToReplyPreviewKeepsTheKindOfEveryOriginal(t *testing.T) {
	for _, kind := range []string{kindText, kindInvite, kindImage} {
		live := toReplyPreview(replyRecord{OriginalID: "m", SenderID: "u", Body: "words", Kind: kind})
		if live.Kind != kind || live.Body != "words" || live.Deleted {
			t.Errorf("live %s: %+v", kind, live)
		}
		gone := toReplyPreview(replyRecord{OriginalID: "m", SenderID: "u", Body: "stale words", Kind: kind, Deleted: true})
		if gone.Kind != kind || gone.Body != "" || !gone.Deleted {
			t.Errorf("deleted %s: %+v, want its kind kept and its words gone", kind, gone)
		}
	}
}

// There is still one place a preview is resolved, and every path goes through it: a
// reply to a picture read after the picture's SQL-level failure is a failure and not a
// preview with a missing kind.
func TestEveryPathResolvesPreviewsThroughTheOneStatement(t *testing.T) {
	e := imageEnv()
	orig := captioned(t, e, "a caption")
	sendAs(t, e, "u2", "u1", "re", map[string]any{"reply_to_id": orig})
	e.db.failOnQuery = replyPreviewsSQL
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"history": e.do("u1", http.MethodGet, "/messages/u2", ""),
		"feed":    e.do("u1", http.MethodGet, "/messages?since=0", ""),
		"list":    e.do("u1", http.MethodGet, "/conversations", ""),
	} {
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: %d, want 500 when the preview statement fails", name, rec.Code)
		}
	}
}
