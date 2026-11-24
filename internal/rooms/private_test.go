package rooms

import (
	"encoding/json"
	"testing"
	"time"
)

// A relayed frame may be addressed to ONE member: the server delivers it to that
// member only and stamps `to` on the delivered frame.

type privateRig struct {
	*answerRig
	carol *fakeConn
}

func newPrivateRig(t *testing.T) *privateRig {
	g := newAnswerRig(t)
	p := &privateRig{answerRig: g, carol: &fakeConn{}}
	if _, err := g.room.Join("carol", "carol", p.carol); err != nil {
		t.Fatal(err)
	}
	g.host.frames, g.bob.frames, p.carol.frames = nil, nil, nil
	return p
}

// raws decodes every frame a connection received into a plain map, so a test can
// assert a key is absent rather than empty.
func raws(t *testing.T, c *fakeConn) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range c.frames {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func wantNothing(t *testing.T, name string, c *fakeConn) {
	t.Helper()
	if len(c.frames) != 0 {
		t.Fatalf("%s should have received nothing, got %v", name, c.types())
	}
}

func wantNoneDelivered(t *testing.T, p *privateRig) {
	t.Helper()
	wantNothing(t, "carol", p.carol)
	if len(only(framesOf(t, p.bob), "deal")) != 0 || len(only(framesOf(t, p.host), "deal")) != 0 {
		t.Fatal("the frame must not have been delivered to anyone")
	}
}

func privateFrameJSON(to string) string {
	return `{"t":"deal","d":{"hand":[1,2]},"to":` + to + `}`
}

func TestPrivateFrameReachesOnlyTheAddressee(t *testing.T) {
	p := newPrivateRig(t)
	p.send("host", p.host, privateFrameJSON(`"bob"`))

	got := raws(t, p.bob)
	if len(got) != 1 {
		t.Fatalf("bob should get exactly one frame, got %d", len(got))
	}
	f := got[0]
	if f["t"] != "deal" || f["from"] != "host" || f["to"] != "bob" {
		t.Fatalf("delivered frame = %v", f)
	}
	d, ok := f["d"].(map[string]any)
	if !ok || d["hand"] == nil {
		t.Fatalf("payload lost: %v", f["d"])
	}
	if hasKey(f, "ref") {
		t.Fatalf("ref must never be forwarded: %v", f)
	}
	wantNothing(t, "carol", p.carol)
	wantNothing(t, "host", p.host)
}

func TestGuestCanSendPrivatelyToTheHost(t *testing.T) {
	p := newPrivateRig(t)
	p.send("bob", p.bob, privateFrameJSON(`"host"`))

	got := raws(t, p.host)
	if len(got) != 1 || got[0]["from"] != "bob" || got[0]["to"] != "host" {
		t.Fatalf("host got %v", got)
	}
	wantNothing(t, "carol", p.carol)
	wantNothing(t, "bob", p.bob)
}

func TestBroadcastStillHasNoToKey(t *testing.T) {
	p := newPrivateRig(t)
	p.send("host", p.host, `{"t":"move","d":{"x":1},"ref":"r1"}`)

	for name, c := range map[string]*fakeConn{"bob": p.bob, "carol": p.carol} {
		got := raws(t, c)
		if len(got) != 1 {
			t.Fatalf("%s should get one frame, got %d", name, len(got))
		}
		if hasKey(got[0], "to") || hasKey(got[0], "ref") {
			t.Fatalf("%s: a broadcast carries no to and no ref: %v", name, got[0])
		}
		if got[0]["from"] != "host" {
			t.Fatalf("%s: from = %v", name, got[0]["from"])
		}
	}
	wantNothing(t, "host", p.host)
}

func TestNullAndEmptyToAreABroadcast(t *testing.T) {
	for _, to := range []string{`null`, `""`} {
		p := newPrivateRig(t)
		p.send("host", p.host, privateFrameJSON(to))
		for name, c := range map[string]*fakeConn{"bob": p.bob, "carol": p.carol} {
			got := raws(t, c)
			if len(got) != 1 || hasKey(got[0], "to") {
				t.Fatalf("to=%s: %s got %v", to, name, got)
			}
		}
		wantNothing(t, "host", p.host)
	}
}

func TestNonStringToIsInvalidTo(t *testing.T) {
	for _, to := range []string{`7`, `{"a":1}`, `["bob"]`, `true`} {
		for _, ref := range []string{"", `"r-9"`} {
			p := newPrivateRig(t)
			body := `{"t":"deal","d":{},"to":` + to
			wantRef := ""
			if ref != "" {
				body += `,"ref":` + ref
				wantRef = "r-9"
			}
			p.send("host", p.host, body+`}`)
			wantErr(t, p.host, errInvalidTo, "deal", wantRef)
			wantNoneDelivered(t, p)
		}
	}
}

func TestToNamingSelfIsInvalidTo(t *testing.T) {
	p := newPrivateRig(t)
	p.send("host", p.host, `{"t":"deal","to":"host","ref":"s1"}`)
	wantErr(t, p.host, errInvalidTo, "deal", "s1")
	wantNoneDelivered(t, p)
}

func TestToSomeoneNotInTheRoomIsNotInRoom(t *testing.T) {
	p := newPrivateRig(t)
	p.send("host", p.host, `{"t":"deal","d":{},"to":"zed","ref":"n1"}`)
	wantErr(t, p.host, errNotInRoom, "deal", "n1")
	wantNoneDelivered(t, p)

	p.host.frames = nil
	p.send("host", p.host, `{"t":"deal","d":{},"to":"zed"}`)
	wantErr(t, p.host, errNotInRoom, "deal", "")
}

func TestToSomeoneWhoLeftIsNotInRoom(t *testing.T) {
	p := newPrivateRig(t)
	p.room.Leave("carol", p.carol, time.Now())
	p.host.frames, p.bob.frames, p.carol.frames = nil, nil, nil

	p.send("host", p.host, `{"t":"deal","d":{},"to":"carol","ref":"l1"}`)
	wantErr(t, p.host, errNotInRoom, "deal", "l1")
	wantNoneDelivered(t, p)
}

func TestHostRequestWithToIsRefusedAndNotRun(t *testing.T) {
	p := newPrivateRig(t)
	p.send("host", p.host, `{"t":"host:rename","d":{"name":"nope"},"to":"bob","ref":"h1"}`)
	wantErr(t, p.host, errInvalidTo, CtlRename, "h1")
	if got := p.room.Name(); got != "amber-lantern" {
		t.Fatalf("the rename must not have happened, name = %q", got)
	}
	wantNothing(t, "bob", p.bob)
	wantNothing(t, "carol", p.carol)
}

func TestReservedTypeWithToIsStillReserved(t *testing.T) {
	p := newPrivateRig(t)
	p.send("bob", p.bob, `{"t":"room:join","to":"carol"}`)
	wantErr(t, p.bob, "reserved_type", "room:join", "")
	wantNothing(t, "carol", p.carol)
	wantNothing(t, "host", p.host)
}

func TestRefIsNeverForwardedOnAPrivateFrame(t *testing.T) {
	p := newPrivateRig(t)
	p.send("host", p.host, `{"t":"deal","d":{},"to":"bob","ref":"keep-me"}`)
	got := raws(t, p.bob)
	if len(got) != 1 || hasKey(got[0], "ref") || got[0]["to"] != "bob" {
		t.Fatalf("bob got %v", got)
	}
	wantNothing(t, "host", p.host)
}

func TestRefusedPrivateFramesSpendBudget(t *testing.T) {
	p := newPrivateRig(t)
	limited := false
	for range 400 {
		p.host.frames = nil
		p.send("host", p.host, `{"t":"deal","to":"zed"}`)
		a := wantOneAnswer(t, p.host)
		if a.data["code"] == "rate_limited" {
			limited = true
			break
		}
		if a.data["code"] != errNotInRoom {
			t.Fatalf("unexpected answer %v", a.data)
		}
	}
	if !limited {
		t.Fatal("a burst of refused private frames must eventually be rate limited")
	}
}

func TestValidPrivateFramesAreRateLimited(t *testing.T) {
	p := newPrivateRig(t)
	limited := false
	for range 400 {
		p.host.frames = nil
		p.send("host", p.host, `{"t":"deal","to":"bob"}`)
		if len(p.host.frames) > 0 {
			wantErr(t, p.host, "rate_limited", "deal", "")
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("a burst of private frames must eventually be rate limited")
	}
	if n := len(p.bob.frames); n == 0 || n >= 400 {
		t.Fatalf("bob received %d frames, want some but not all", n)
	}
}
