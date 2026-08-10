package users

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// The two short-lived things a Discord login needs to remember, and nothing
// else does: what a login in flight was for, and an identity that has been
// verified but has no account yet.
//
// Both live in memory. A restart mid-login loses them and the person tries
// again, which is a worse outcome than a database table would give — but only
// for the seconds somebody happens to be mid-login, and it's the difference
// between two tables of garbage to expire and none. Rooms make the same trade.

// How long each thing is good for.
const (
	// A login in flight: long enough to read Discord's consent screen and think
	// about it, short enough that an abandoned one doesn't linger.
	pendingLoginTTL = 10 * time.Minute
	// A verified identity waiting for a username, on the website. Longer,
	// because a person is typing at a prompt, and re-authorizing to fix a typo
	// would be miserable.
	signupTicketTTL = 20 * time.Minute
	// A finished login waiting to be picked up. Seconds, not minutes: the
	// browser is redirected the instant it's issued and the client claims it
	// immediately, so anything longer is a window for nothing.
	handoffTTL = 2 * time.Minute
)

// PendingLogin is a login that has left for Discord and not come back.
type PendingLogin struct {
	// ReturnTo is where the browser is sent afterwards: an app's loopback
	// address, or the website. VALIDATED WHEN IT'S STORED, never on the way out —
	// an open redirect is exactly the bug an OAuth callback attracts.
	ReturnTo string
	// FromApp distinguishes the desktop client from the website. It decides what
	// happens to somebody with no account yet, which only the website can finish.
	FromApp   bool
	ExpiresAt time.Time
}

// SignupTicket is a Discord identity we have verified, allowed, and which has
// no Bombers account yet. It exists for exactly as long as it takes somebody to
// choose a username.
//
// It carries the identity so that step needs no second trip to Discord — and
// so the username form can't be used to create an account for an identity
// nobody proved they own.
type SignupTicket struct {
	Profile   DiscordProfile
	ExpiresAt time.Time
}

// handoff is a completed login waiting for the client to collect it.
//
// The browser is redirected with a code rather than with tokens, so an access
// token never lands in a URL, browser history, or a referrer header. The client
// trades the code for the real pair over HTTPS, once.
type handoff struct {
	UserID    string
	ExpiresAt time.Time
}

// PendingStore holds all three, expiring them as it goes.
type PendingStore struct {
	mu       sync.Mutex
	logins   map[string]PendingLogin
	tickets  map[string]SignupTicket
	handoffs map[string]handoff
	// now is time.Now except in tests.
	now func() time.Time
}

func NewPendingStore() *PendingStore {
	return &PendingStore{
		logins:   make(map[string]PendingLogin),
		tickets:  make(map[string]SignupTicket),
		handoffs: make(map[string]handoff),
		now:      time.Now,
	}
}

// IssueHandoff parks a signed-in user for the client to collect.
func (s *PendingStore) IssueHandoff(userID string) (string, error) {
	code, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.handoffs[code] = handoff{UserID: userID, ExpiresAt: s.now().Add(handoffTTL)}
	return code, nil
}

// ClaimHandoff trades a code for the user it belongs to. Single use — a code
// that worked twice would hand a second session to whoever replayed it.
func (s *PendingStore) ClaimHandoff(code string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.handoffs[code]
	if !ok {
		return "", false
	}
	delete(s.handoffs, code)
	if s.now().After(h.ExpiresAt) {
		return "", false
	}
	return h.UserID, true
}

// StartLogin records a login in flight and returns the opaque `state` to hand
// Discord. Discord returns it untouched, which is what makes a callback
// traceable to a login this server actually started.
func (s *PendingStore) StartLogin(returnTo string, fromApp bool) (string, error) {
	state, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.logins[state] = PendingLogin{
		ReturnTo:  returnTo,
		FromApp:   fromApp,
		ExpiresAt: s.now().Add(pendingLoginTTL),
	}
	return state, nil
}

// ClaimLogin consumes a state, returning what the login was for.
//
// Single use: a state that worked twice would let a replayed callback mint a
// second session. Consuming it is the whole protection, so it happens before
// anything else can fail.
func (s *PendingStore) ClaimLogin(state string) (PendingLogin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.logins[state]
	if !ok {
		return PendingLogin{}, false
	}
	delete(s.logins, state)
	if s.now().After(pending.ExpiresAt) {
		return PendingLogin{}, false
	}
	return pending, true
}

// IssueTicket parks a verified identity that still needs a username.
func (s *PendingStore) IssueTicket(profile DiscordProfile) (string, error) {
	ticket, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.tickets[ticket] = SignupTicket{
		Profile:   profile,
		ExpiresAt: s.now().Add(signupTicketTTL),
	}
	return ticket, nil
}

// PeekTicket reads a ticket without spending it, so a rejected username — one
// already taken — can be retried at the same prompt rather than sending
// somebody back through Discord to fix a typo.
func (s *PendingStore) PeekTicket(ticket string) (SignupTicket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[ticket]
	if !ok || s.now().After(t.ExpiresAt) {
		return SignupTicket{}, false
	}
	return t, true
}

// SpendTicket consumes a ticket, once an account has actually been created.
func (s *PendingStore) SpendTicket(ticket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tickets, ticket)
}

// sweepLocked drops anything expired. Called on the write paths rather than
// from a goroutine: the map only grows when logins start, so that's exactly
// when it's worth looking, and there's no ticker to shut down.
func (s *PendingStore) sweepLocked() {
	now := s.now()
	for k, v := range s.logins {
		if now.After(v.ExpiresAt) {
			delete(s.logins, k)
		}
	}
	for k, v := range s.tickets {
		if now.After(v.ExpiresAt) {
			delete(s.tickets, k)
		}
	}
	for k, v := range s.handoffs {
		if now.After(v.ExpiresAt) {
			delete(s.handoffs, k)
		}
	}
}

// randomToken is 32 bytes of crypto/rand, URL-safe. Used for both the OAuth
// state and the signup ticket: one has to be unguessable to stop a forged
// callback, the other to stop somebody claiming an identity they didn't prove.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
