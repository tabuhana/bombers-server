package rooms

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The answer contract: every host request gets exactly one room:ok or room:error,
// both carrying the request's type and its ref when it gave one; a bad ref is
// refused and nothing is acted on; a ref is never relayed.

type answerRig struct {
	t    *testing.T
	h    *Handler
	room *Room
	host *fakeConn
	bob  *fakeConn
}

func newAnswerRig(t *testing.T) *answerRig {
	h := NewHandler(nil, nil, nil, nil)
	room := h.hub.Create("room1", "amber-lantern", "host", time.Now())
	g := &answerRig{t: t, h: h, room: room, host: &fakeConn{}, bob: &fakeConn{}}
	if _, err := room.Join("host", "host", g.host); err != nil {
		t.Fatal(err)
	}
	if _, err := room.Join("bob", "bob", g.bob); err != nil {
		t.Fatal(err)
	}
	g.host.frames, g.bob.frames = nil, nil
	return g
}

// send pushes one raw frame through the real per-frame handler as `as`.
func (g *answerRig) send(as string, c *fakeConn, raw string) {
	g.h.handleFrame(g.room, as, []byte(raw), c)
}

// frame is one decoded frame: its type and its payload keys.
type frame struct {
	typ  string
	data map[string]any
}

func framesOf(t *testing.T, c *fakeConn) []frame {
	t.Helper()
	var out []frame
	for _, raw := range c.frames {
		var e envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if len(e.Data) > 0 {
			if err := json.Unmarshal(e.Data, &data); err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, frame{e.Type, data})
	}
	return out
}

// only returns every frame of the given types, in order.
func only(fs []frame, types ...string) []frame {
	var out []frame
	for _, f := range fs {
		for _, ty := range types {
			if f.typ == ty {
				out = append(out, f)
			}
		}
	}
	return out
}

// wantOneAnswer asserts the connection got exactly one answer (ok or error), and
// returns it.
func wantOneAnswer(t *testing.T, c *fakeConn) frame {
	t.Helper()
	got := only(framesOf(t, c), TypeOK, TypeError)
	if len(got) != 1 {
		t.Fatalf("want exactly one answer, got %d: %v", len(got), c.types())
	}
	return got[0]
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func wantOK(t *testing.T, c *fakeConn, typ, ref string) {
	t.Helper()
	a := wantOneAnswer(t, c)
	if a.typ != TypeOK {
		t.Fatalf("want room:ok, got %s %v", a.typ, a.data)
	}
	if a.data["type"] != typ {
		t.Fatalf("ok type = %v, want %q", a.data["type"], typ)
	}
	if ref == "" {
		if hasKey(a.data, "ref") {
			t.Fatalf("ok must carry no ref key, got %v", a.data)
		}
	} else if a.data["ref"] != ref {
		t.Fatalf("ok ref = %v, want %q", a.data["ref"], ref)
	}
	if hasKey(a.data, "code") {
		t.Fatalf("ok must carry no code, got %v", a.data)
	}
}

func wantErr(t *testing.T, c *fakeConn, code, typ, ref string) {
	t.Helper()
	a := wantOneAnswer(t, c)
	if a.typ != TypeError {
		t.Fatalf("want room:error, got %s %v", a.typ, a.data)
	}
	if a.data["code"] != code {
		t.Fatalf("code = %v, want %q", a.data["code"], code)
	}
	if typ == "" {
		if hasKey(a.data, "type") {
			t.Fatalf("error must carry no type key, got %v", a.data)
		}
	} else if a.data["type"] != typ {
		t.Fatalf("error type = %v, want %q", a.data["type"], typ)
	}
	if ref == "" {
		if hasKey(a.data, "ref") {
			t.Fatalf("error must carry no ref key, got %v", a.data)
		}
	} else if a.data["ref"] != ref {
		t.Fatalf("error ref = %v, want %q", a.data["ref"], ref)
	}
}

// hostRequests are one successful request of each kind. Kick is last of the
// non-ending ones and the end request is separate because it closes the room.
var hostRequests = []struct{ typ, body string }{
	{CtlRename, `{"name":"game night"}`},
	{CtlGame, `{"activity_id":"chess"}`},
	{CtlKick, `{"user_id":"bob"}`},
	{CtlInvites, `{"allow":true}`},
	{CtlEnd, `{}`},
}

func frameJSON(typ, body, refJSON string) string {
	s := `{"t":"` + typ + `"`
	if body != "" {
		s += `,"d":` + body
	}
	if refJSON != "" {
		s += `,"ref":` + refJSON
	}
	return s + `}`
}

func TestEveryHostRequestIsAnsweredOnceWithOk(t *testing.T) {
	for _, withRef := range []bool{true, false} {
		for _, req := range hostRequests {
			g := newAnswerRig(t)
			refJSON, ref := "", ""
			if withRef {
				refJSON, ref = `"r-1"`, "r-1"
			}
			g.send("host", g.host, frameJSON(req.typ, req.body, refJSON))
			wantOK(t, g.host, req.typ, ref)
		}
	}
}

func TestOkFollowsTheRoomUpdate(t *testing.T) {
	g := newAnswerRig(t)
	g.send("host", g.host, frameJSON(CtlRename, `{"name":"game night"}`, `"x"`))
	types := g.host.types()
	if len(types) != 2 || types[0] != TypeUpdate || types[1] != TypeOK {
		t.Fatalf("host should see the update then the ok, got %v", types)
	}
}

func TestHostEndSendsOkBeforeClosed(t *testing.T) {
	g := newAnswerRig(t)
	g.send("host", g.host, frameJSON(CtlEnd, `{}`, `"bye"`))
	types := g.host.types()
	if len(types) != 2 || types[0] != TypeOK || types[1] != TypeClosed {
		t.Fatalf("want room:ok then room:closed, got %v", types)
	}
	if !g.host.closed {
		t.Fatal("the host's connection should have been closed")
	}
	if _, err := g.h.hub.Get("room1"); err == nil {
		t.Fatal("the room should have been removed")
	}
}

func TestHostRefusalsCarryTypeAndRefAndNoOk(t *testing.T) {
	cases := []struct {
		name, as, typ, body, code string
	}{
		{"not_host", "bob", CtlRename, `{"name":"nope"}`, "not_host"},
		{"not_host end", "bob", CtlEnd, `{}`, "not_host"},
		{"bad json body", "host", CtlRename, `"not an object"`, errInvalidBody},
		{"invites with no allow", "host", CtlInvites, `{}`, errInvalidBody},
		{"unknown host type", "host", "host:xyz", `{}`, "refused"},
		{"kick a stranger", "host", CtlKick, `{"user_id":"nobody"}`, "refused"},
	}
	for _, c := range cases {
		for _, ref := range []string{"abc", ""} {
			g := newAnswerRig(t)
			conn := g.host
			if c.as == "bob" {
				conn = g.bob
			}
			refJSON := ""
			if ref != "" {
				refJSON = `"` + ref + `"`
			}
			g.send(c.as, conn, frameJSON(c.typ, c.body, refJSON))
			t.Run(c.name, func(t *testing.T) { wantErr(t, conn, c.code, c.typ, ref) })
			if n := len(only(framesOf(t, conn), TypeOK)); n != 0 {
				t.Fatalf("%s: a refusal must not also send room:ok", c.name)
			}
		}
	}
}

func TestHostRequestOnAClosedRoomIsRoomClosed(t *testing.T) {
	g := newAnswerRig(t)
	g.room.Close()
	g.host.frames = nil
	g.send("host", g.host, frameJSON(CtlRename, `{"name":"late"}`, `"r"`))
	wantErr(t, g.host, "room_closed", CtlRename, "r")
}

func TestReservedTypeCarriesTypeAndRef(t *testing.T) {
	g := newAnswerRig(t)
	g.send("bob", g.bob, frameJSON("room:join", `{}`, `"r9"`))
	wantErr(t, g.bob, "reserved_type", "room:join", "r9")
	if len(g.host.frames) != 0 {
		t.Fatal("a reserved frame must not be relayed")
	}
	g.bob.frames = nil
	g.send("bob", g.bob, frameJSON("room:join", `{}`, ""))
	wantErr(t, g.bob, "reserved_type", "room:join", "")
}

func TestRateLimitedCarriesTypeAndRef(t *testing.T) {
	g := newAnswerRig(t)
	raw := frameJSON("game:pos", `{"x":1}`, `"spam"`)
	for range 400 {
		g.send("bob", g.bob, raw)
	}
	errs := only(framesOf(t, g.bob), TypeError)
	if len(errs) == 0 {
		t.Fatal("400 frames in a tight loop should trip the rate cap")
	}
	for _, e := range errs {
		if e.data["code"] != "rate_limited" || e.data["type"] != "game:pos" || e.data["ref"] != "spam" {
			t.Fatalf("rate_limited should carry type and ref, got %v", e.data)
		}
	}
}

func TestInvalidRefIsRefusedAndNothingIsActedOn(t *testing.T) {
	bad := map[string]string{
		"number": `42`,
		"object": `{"a":1}`,
		"array":  `["a"]`,
		"bool":   `true`,
		"long":   `"` + strings.Repeat("a", maxRefBytes+1) + `"`,
	}
	for name, refJSON := range bad {
		// A host request is not executed.
		g := newAnswerRig(t)
		g.send("host", g.host, frameJSON(CtlRename, `{"name":"changed"}`, refJSON))
		wantErr(t, g.host, "invalid_ref", CtlRename, "")
		if g.room.Name() != "amber-lantern" {
			t.Fatalf("%s: a rename with a bad ref must not run, name = %q", name, g.room.Name())
		}
		// A relayed frame is not relayed.
		g = newAnswerRig(t)
		g.send("bob", g.bob, frameJSON("game:move", `{"m":1}`, refJSON))
		wantErr(t, g.bob, "invalid_ref", "game:move", "")
		if len(g.host.frames) != 0 {
			t.Fatalf("%s: a relayed frame with a bad ref must not be relayed", name)
		}
		// A reserved type with a bad ref is still invalid_ref, checked first.
		g = newAnswerRig(t)
		g.send("bob", g.bob, frameJSON("room:join", `{}`, refJSON))
		wantErr(t, g.bob, "invalid_ref", "room:join", "")
	}
}

func TestSixtyFourByteRefIsAccepted(t *testing.T) {
	g := newAnswerRig(t)
	ref := strings.Repeat("a", maxRefBytes)
	g.send("host", g.host, frameJSON(CtlRename, `{"name":"changed"}`, `"`+ref+`"`))
	wantOK(t, g.host, CtlRename, ref)
	if g.room.Name() != "changed" {
		t.Fatal("the rename should have run")
	}
}

func TestNullAndEmptyRefBehaveAsAbsent(t *testing.T) {
	for _, refJSON := range []string{`null`, `""`} {
		g := newAnswerRig(t)
		g.send("host", g.host, frameJSON(CtlRename, `{"name":"changed"}`, refJSON))
		wantOK(t, g.host, CtlRename, "")

		g = newAnswerRig(t)
		g.send("host", g.host, frameJSON(CtlRename, `"bad body"`, refJSON))
		wantErr(t, g.host, errInvalidBody, CtlRename, "")
	}
}

func TestInvalidFrameCarriesNeitherTypeNorRef(t *testing.T) {
	for _, raw := range []string{`not json`, `{"d":{}}`, `{"t":"","ref":"r"}`, `{"t":5,"ref":"r"}`} {
		g := newAnswerRig(t)
		g.send("bob", g.bob, raw)
		wantErr(t, g.bob, "invalid_frame", "", "")
	}
}

func TestARelayedFramesRefIsNeverForwarded(t *testing.T) {
	g := newAnswerRig(t)
	g.send("bob", g.bob, frameJSON("game:move", `{"m":1}`, `"secret-ref"`))
	if len(g.host.frames) != 1 {
		t.Fatalf("host should get the relayed frame, got %v", g.host.types())
	}
	raw := g.host.frames[0]
	if strings.Contains(string(raw), "ref") || strings.Contains(string(raw), "secret-ref") {
		t.Fatalf("the relayed frame must carry no ref: %s", raw)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["ref"]; ok {
		t.Fatal("ref key present on the relayed frame")
	}
	if string(m["from"]) != `"bob"` || string(m["t"]) != `"game:move"` {
		t.Fatalf("relayed frame lost its type or from: %s", raw)
	}
	// And the sender is told nothing: a relayed frame is not a request.
	if len(g.bob.frames) != 0 {
		t.Fatalf("a relayed frame gets no answer, sender saw %v", g.bob.types())
	}
}
