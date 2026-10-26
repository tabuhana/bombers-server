package presence

import "testing"

// A stored status is a WISH; what a friend sees is that wish plus whether an app
// of theirs is connected. Everything else in this package is one query, so this
// function is the whole of the domain's thinking.
func TestEffectiveCombinesTheWishAndTheSocket(t *testing.T) {
	cases := []struct {
		name      string
		state     Status
		connected bool
		want      Status
	}{
		{"online and here", Online, true, Online},
		{"away and here", Away, true, Away},
		{"dnd and here", DND, true, DND},

		// The case that matters: a status stored by a machine that has since been
		// shut. Without it, closing the app leaves you permanently green.
		{"online but nothing connected", Online, false, Offline},
		{"away but nothing connected", Away, false, Offline},
		{"dnd but nothing connected", DND, false, Offline},

		// Offline is honoured exactly — it means invisible AND disconnected, so a
		// live socket does not override it. Otherwise "appear offline" would leak
		// the moment the app connected.
		{"offline despite a live socket", Offline, true, Offline},
		{"offline and gone", Offline, false, Offline},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Effective(tc.state, tc.connected); got != tc.want {
				t.Errorf("Effective(%q, %v) = %q, want %q", tc.state, tc.connected, got, tc.want)
			}
		})
	}
}

// A closed set, checked at the door: an unrecognised status stored here would
// read as "not offline" to every lookup that isn't testing for a specific one.
func TestValidIsAClosedSet(t *testing.T) {
	for _, ok := range []string{"online", "away", "dnd", "offline", " online "} {
		if !Valid(ok) {
			t.Errorf("Valid(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "ONLINE", "busy", "invisible", "idle", "offline; DROP TABLE"} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true, want false", bad)
		}
	}
}

// The invisible rule, which is the one thing in the fan-out that is a promise
// rather than a convenience: a frame saying "X's presence moved" says X is at
// their computer, so an invisible user's socket arriving or going tells nobody.
// Their CHOOSING invisible is still announced — that is the change, and all it
// reveals is the grey dot they asked for.
func TestAnnounceSaysNothingForAnInvisibleUser(t *testing.T) {
	told := [][]string{}
	h := &Handler{nudge: func(userIDs []string, fromID string) { told = append(told, userIDs) }}

	h.Announce(nil, "u1", Offline)
	if len(told) != 0 {
		t.Fatalf("an invisible user's presence was announced to %v", told)
	}
	// Without a pool there is nothing to look friends up in, so the other
	// statuses cannot be driven this far here; the gate above runs before any
	// query, which is what this pins.
}
