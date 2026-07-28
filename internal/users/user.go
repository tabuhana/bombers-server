package users

import (
	"time"

	"github.com/tabuhana/bombers-server/internal/types"
)

// User is the full domain object for a registered account. It MUST NOT be
// serialized to clients directly — it carries the password hash. Convert to
// types.PublicUser via Public() before any response.
type User struct {
	ID                string
	Username          string
	UsernameCanonical string
	PasswordHash      string
	FriendCode        string
	CreatedAt         time.Time
	// IsAdmin mirrors users.is_admin — the operator role (see internal/admin).
	IsAdmin bool
	// Banned mirrors `banned_at IS NOT NULL`. Checked at the doors (login,
	// refresh) rather than on every request — see the ban migration.
	Banned bool
}

func (u *User) Public() types.PublicUser {
	return types.PublicUser{
		ID:         u.ID,
		Username:   u.Username,
		FriendCode: u.FriendCode,
		CreatedAt:  u.CreatedAt,
		IsAdmin:    u.IsAdmin,
	}
}
