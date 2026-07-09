package types

import (
	"fmt"
	"time"
)

// Media kinds — the profile images a user can attach. Mirror the CHECK
// constraint on the user_media table.
const (
	MediaKindAvatar = "avatar"
	MediaKindBanner = "banner"
)

// ValidMediaKind reports whether s is a known media kind.
func ValidMediaKind(s string) bool {
	return s == MediaKindAvatar || s == MediaKindBanner
}

// MediaURL is the server-relative URL a client fetches a user's media from
// (authenticated, proxied through the server — never a direct bucket link).
// The ?v= query is a cache-buster derived from the row's updated_at, so a
// reupload produces a new URL and stale client caches miss. Shared here so the
// media domain (upload responses) and the profiles domain (avatar_url /
// banner_url fields) emit the exact same shape without reaching into each
// other.
func MediaURL(userID, kind string, updatedAt time.Time) string {
	return fmt.Sprintf("/media/%s/%s?v=%d", userID, kind, updatedAt.Unix())
}
