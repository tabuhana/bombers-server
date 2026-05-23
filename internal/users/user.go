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
}

func (u *User) Public() types.PublicUser {
	return types.PublicUser{
		ID:         u.ID,
		Username:   u.Username,
		FriendCode: u.FriendCode,
		CreatedAt:  u.CreatedAt,
	}
}
