package main

import (
	"testing"

	"github.com/tabuhana/bombers-server/internal/messaging"
	"github.com/tabuhana/bombers-server/internal/notify"
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
