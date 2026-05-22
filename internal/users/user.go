package users

import "time"

// User is the full domain object for a registered account. It MUST NOT be
// serialized to clients directly — it carries the password hash. Convert to
// PublicUser via Public() before any response.
type User struct {
	ID                string
	Username          string
	UsernameCanonical string
	PasswordHash      string
	FriendCode        string
	CreatedAt         time.Time
}

// PublicUser is the single JSON-safe view of a user. This is the ONLY user
// shape that may leave the server. Do not add fields that should remain
// private (password hash, refresh token, internal flags) — anything added
// here leaks from every endpoint that returns a user.
type PublicUser struct {
	ID         string    `json:"id"`
	Username   string    `json:"username"`
	FriendCode string    `json:"friend_code"`
	CreatedAt  time.Time `json:"created_at"`
}

func (u *User) Public() PublicUser {
	return PublicUser{
		ID:         u.ID,
		Username:   u.Username,
		FriendCode: u.FriendCode,
		CreatedAt:  u.CreatedAt,
	}
}
