package main

import (
	"os"
	"strings"
	"testing"

	"github.com/tabuhana/bombers-server/internal/messaging"
	"github.com/tabuhana/bombers-server/internal/notify"
	"github.com/tabuhana/bombers-server/internal/rooms"
)

// messaging may not import notify, so it names its own kinds and main hands it the
// hub's fan-out. If the strings ever drift, every DM, typing indicator or read
// receipt is delivered under a kind no client listens for — silently, since a
// push never errors.
func TestMessagingPushesUnderTheKindsTheClientListensFor(t *testing.T) {
	for _, k := range []struct{ name, messaging, notify string }{
		{"dm", messaging.KindDM, notify.KindDM},
		{"typing", messaging.KindTyping, notify.KindTyping},
		{"read", messaging.KindRead, notify.KindRead},
	} {
		if k.messaging != k.notify {
			t.Errorf("%s: messaging.Kind = %q, notify.Kind = %q", k.name, k.messaging, k.notify)
		}
	}
}

// rooms may not import notify either, so it names the invite kind itself. A drift
// here delivers every invite and every answer under a kind no client listens for,
// and a push never errors to say so.
func TestRoomsPushesInvitesUnderTheKindTheClientListensFor(t *testing.T) {
	if rooms.KindInvite != notify.KindInvite {
		t.Errorf("rooms.KindInvite = %q, notify.KindInvite = %q", rooms.KindInvite, notify.KindInvite)
	}
}

// rooms holds the invite and messaging writes the message that carries it; main is
// the only place that joins them. The fit is checked by the compiler here, and that
// main actually passes it is checked as text, because a rooms handler built with no
// minter is valid Go that makes invites with no card in the thread - silently, since
// nil means "no messaging wired" to every test.
var _ rooms.MintInviteMessage = (*messaging.Handler)(nil).MintInvite

func TestMainHandsRoomsTheInviteMessageWriter(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "rooms.NewHandler(") {
			calls = append(calls, line)
		}
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "messagingHandler.MintInvite") {
		t.Errorf("rooms.NewHandler is built as %q; it must be given messagingHandler.MintInvite", calls)
	}
}

// The route behind a card in a thread. Registered in main like every route, so a
// missing line is a 404 for every card and nothing else fails to say so.
func TestMainRegistersTheInviteRead(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `r.Get("/invites/{inviteID}", roomsHandler.GetInvite)`) {
		t.Error("GET /invites/{inviteID} is not registered to roomsHandler.GetInvite")
	}
}
