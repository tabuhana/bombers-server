// Package types holds DTOs shared across domain packages. Anything in here
// is consumed by more than one of internal/<domain> — it should never be
// imported by only one. Domain-private types stay inside their domain.
package types

import "time"

// PublicUser is the single JSON-safe view of a user. This is the ONLY user
// shape that may leave the server. Do not add fields that should remain
// private (password hash, refresh token, internal flags) — anything added
// here leaks from every endpoint that returns a user.
type PublicUser struct {
	ID         string    `json:"id"`
	Username   string    `json:"username"`
	FriendCode string    `json:"friend_code"`
	CreatedAt  time.Time `json:"created_at"`
	// IsAdmin lets the CLIENT know whether to offer operator actions at all —
	// the server still enforces them, this just stops the UI showing a door
	// that won't open.
	IsAdmin bool `json:"is_admin"`
}
