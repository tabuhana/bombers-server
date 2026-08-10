package users

import (
	"testing"
	"time"
)

// at pins the store's clock so expiry can be tested without sleeping.
func at(s *PendingStore, t time.Time) { s.now = func() time.Time { return t } }

func TestClaimLoginIsSingleUse(t *testing.T) {
	s := NewPendingStore()
	state, err := s.StartLogin("http://localhost:5000", true)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	got, ok := s.ClaimLogin(state)
	if !ok {
		t.Fatal("the first claim should succeed")
	}
	if got.ReturnTo != "http://localhost:5000" || !got.FromApp {
		t.Errorf("claimed %+v, not what was stored", got)
	}

	// The whole protection against a replayed callback minting a second
	// session is that a state works exactly once.
	if _, ok := s.ClaimLogin(state); ok {
		t.Error("the second claim must fail")
	}
}

func TestClaimLoginRejectsUnknownState(t *testing.T) {
	s := NewPendingStore()
	// A callback carrying a state this server never issued is either a forgery
	// or a very old tab. Neither should produce a session.
	if _, ok := s.ClaimLogin("not-a-state-we-issued"); ok {
		t.Error("an unknown state must not be claimable")
	}
}

func TestClaimLoginExpires(t *testing.T) {
	s := NewPendingStore()
	start := time.Now()
	at(s, start)
	state, _ := s.StartLogin("http://localhost:5000", true)

	at(s, start.Add(pendingLoginTTL+time.Second))
	if _, ok := s.ClaimLogin(state); ok {
		t.Error("an expired login must not be claimable")
	}
}

func TestStatesAreDistinct(t *testing.T) {
	s := NewPendingStore()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		state, err := s.StartLogin("http://localhost:5000", true)
		if err != nil {
			t.Fatalf("StartLogin: %v", err)
		}
		if seen[state] {
			t.Fatal("two logins were issued the same state")
		}
		seen[state] = true
	}
}

func TestTicketPeekThenSpend(t *testing.T) {
	s := NewPendingStore()
	ticket, err := s.IssueTicket(DiscordProfile{ID: "42", Username: "someone"})
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}

	// Peek doesn't consume: a taken username has to be retryable at the same
	// prompt, without another trip through Discord.
	for i := 0; i < 3; i++ {
		got, ok := s.PeekTicket(ticket)
		if !ok {
			t.Fatalf("peek %d should succeed", i)
		}
		if got.Profile.ID != "42" {
			t.Errorf("peeked profile %+v", got.Profile)
		}
	}

	s.SpendTicket(ticket)
	if _, ok := s.PeekTicket(ticket); ok {
		t.Error("a spent ticket must not be usable again")
	}
}

func TestTicketExpires(t *testing.T) {
	s := NewPendingStore()
	start := time.Now()
	at(s, start)
	ticket, _ := s.IssueTicket(DiscordProfile{ID: "42"})

	at(s, start.Add(signupTicketTTL+time.Second))
	if _, ok := s.PeekTicket(ticket); ok {
		t.Error("an expired ticket must not be usable")
	}
}

func TestSweepDropsExpiredEntries(t *testing.T) {
	s := NewPendingStore()
	start := time.Now()
	at(s, start)
	for i := 0; i < 5; i++ {
		if _, err := s.StartLogin("http://localhost:5000", true); err != nil {
			t.Fatal(err)
		}
		if _, err := s.IssueTicket(DiscordProfile{ID: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	// Abandoned logins are the common case — people close the Discord tab — so
	// the maps must not grow forever just because nobody finished.
	at(s, start.Add(signupTicketTTL+time.Minute))
	if _, err := s.StartLogin("http://localhost:5000", false); err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	logins, tickets := len(s.logins), len(s.tickets)
	s.mu.Unlock()
	if logins != 1 {
		t.Errorf("logins = %d, want only the fresh one", logins)
	}
	if tickets != 0 {
		t.Errorf("tickets = %d, want all expired ones gone", tickets)
	}
}
