package presence

import (
	"testing"
	"time"
)

// A stored status is a WISH; what a friend sees is that wish plus whether
// anybody has heard from you. Everything else in this package is one query, so
// this function is the whole of the domain's thinking.
func TestEffectiveCombinesTheWishAndTheHeartbeat(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-30 * time.Second)
	stale := now.Add(-StaleAfter - time.Second)

	cases := []struct {
		name  string
		state Status
		seen  time.Time
		want  Status
	}{
		{"online and here", Online, fresh, Online},
		{"away and here", Away, fresh, Away},
		{"dnd and here", DND, fresh, DND},

		// An `online` from a machine that stopped talking is a claim nobody is
		// backing. This is the case that matters: without it, closing the app
		// leaves you permanently green.
		{"online but long gone", Online, stale, Offline},
		{"away but long gone", Away, stale, Offline},
		{"dnd but long gone", DND, stale, Offline},

		// Offline is honoured exactly — it means invisible AND disconnected, so
		// a heartbeat arriving a second ago does not override it. Otherwise
		// "appear offline" would leak the moment the app checked in.
		{"offline despite a fresh heartbeat", Offline, fresh, Offline},
		{"offline and gone", Offline, stale, Offline},

		// Never seen at all: the zero time is what the friends query fills in
		// for somebody with no presence row.
		{"never seen", Online, time.Time{}, Offline},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Effective(tc.state, tc.seen, now); got != tc.want {
				t.Errorf("Effective(%q, %v) = %q, want %q", tc.state, tc.seen, got, tc.want)
			}
		})
	}
}

// The boundary itself, since "3 minutes" is a number somebody will want to
// change and the comparison is easy to get backwards.
func TestStaleIsExclusive(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	if got := Effective(Online, now.Add(-StaleAfter), now); got != Online {
		t.Errorf("exactly at the limit = %q, want online — a heartbeat that just aged out is still a heartbeat", got)
	}
	if got := Effective(Online, now.Add(-StaleAfter-time.Nanosecond), now); got != Offline {
		t.Errorf("a nanosecond past the limit = %q, want offline", got)
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
