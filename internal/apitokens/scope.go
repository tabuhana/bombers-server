// Package apitokens holds long-lived, scoped, revocable credentials — the way
// something other than the desktop app talks to a Bombers server.
//
// The session tokens in `auth` are built for a person sitting in front of a
// client: a 15-minute access token and a rotating refresh token that must not be
// presented twice. Nothing there can be handed to a script, a mini-client, or an
// agent, and nothing there can be narrowed or taken back on its own.
//
// An API token is the opposite shape. You name it, you choose what it may do,
// and you revoke it without touching your password or your other sessions. The
// secret is shown once and stored as a hash, so the server can check a token but
// never show you one again.
//
// The scopes are deliberately few and coarse. A long list of fine-grained
// permissions reads as careful and behaves as careless: nobody composes them
// correctly, so everybody grants all of them. These are the questions someone
// actually asks about an agent — can it read my writing, can it write, can it
// see who I know, can it read my messages.
package apitokens

import "strings"

// Scope is one thing a token may do.
type Scope string

const (
	// ProfileRead is who you are: `/me` and your own profile card. Every token
	// gets it implicitly — a credential that can't identify its owner is
	// useless, and it exposes nothing the token holder doesn't already know.
	ProfileRead Scope = "profile:read"

	// NotesRead / NotesWrite cover published items — sync pull and push. This
	// is the big one: your notes are the substance of the notebook, and an
	// agent that only builds nodes has no business holding it.
	NotesRead  Scope = "notes:read"
	NotesWrite Scope = "notes:write"

	// PeopleRead is the cards you keep about the people in your life: your own
	// self-card's shares and the about-cards you've written. The most personal
	// thing here, so it stands alone rather than riding along with notes.
	PeopleRead Scope = "people:read"

	// FriendsRead is the graph — who you know, not what you think of them.
	FriendsRead Scope = "friends:read"

	// MessagesRead / MessagesWrite are DMs. Write is separate because sending a
	// message to another person is the one thing here with a social consequence
	// a mistake can't take back.
	MessagesRead  Scope = "messages:read"
	MessagesWrite Scope = "messages:write"

	// StoreRead browses this server's nodes, packs and activities, and
	// downloads them. Harmless enough that a build agent can hold it alone.
	StoreRead Scope = "store:read"

	// StoreWrite publishes to those stores. It is NOT a way around the admin
	// role: a publish still requires both this scope and an admin account, so a
	// normal user's token cannot publish however it's scoped.
	StoreWrite Scope = "store:write"
)

// All is every grantable scope, in the order a UI should offer them: least
// alarming first.
var All = []Scope{
	StoreRead,
	StoreWrite,
	NotesRead,
	NotesWrite,
	FriendsRead,
	PeopleRead,
	MessagesRead,
	MessagesWrite,
}

// Valid reports whether a string names a real scope. Anything else is refused
// at creation rather than stored and silently ignored — a token that claims a
// scope the server doesn't understand is a token whose holder is wrong about
// what it can do.
func Valid(s string) bool {
	for _, scope := range All {
		if string(scope) == s {
			return true
		}
	}
	return s == string(ProfileRead)
}

// Set is the scopes a request carries, for the middleware to test.
type Set map[Scope]struct{}

// NewSet builds a scope set from stored strings. ProfileRead is always present:
// see its comment.
func NewSet(scopes []string) Set {
	set := Set{ProfileRead: {}}
	for _, s := range scopes {
		set[Scope(strings.TrimSpace(s))] = struct{}{}
	}
	return set
}

// Has reports whether the set allows a scope.
func (s Set) Has(scope Scope) bool {
	_, ok := s[scope]
	return ok
}

// Strings renders the set for a listing, in All's order so two tokens with the
// same grants always read the same.
func (s Set) Strings() []string {
	out := make([]string, 0, len(s))
	for _, scope := range All {
		if s.Has(scope) {
			out = append(out, string(scope))
		}
	}
	return out
}
