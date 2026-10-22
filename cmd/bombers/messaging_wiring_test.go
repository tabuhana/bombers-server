package main

import (
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
