package auth

import "time"

// rotateVerdict is what Rotate must do with a presented refresh token.
type rotateVerdict int

const (
	verdictRotate  rotateVerdict = iota // never used: ordinary rotation
	verdictReissue                      // lost answer: fresh pair, cancel the unused replacement
	verdictReuse                        // theft: kill every token the user has
	verdictInvalid                      // expired: plain 401, no kill
)

// tokenState is the part of a refresh_tokens row the decision needs.
type tokenState struct {
	ExpiresAt  time.Time
	UsedAt     *time.Time
	RevokedAt  *time.Time
	ReplacedBy *string
}

// decideRotation is the whole rotation policy as a pure function, so the rules
// can be tested without a database. replacement is nil when there is none to
// look at (no replaced_by, or the row is gone).
//
// A used token presented again is usually theft, but not always: if the reply
// to a rotation never reached the app (wifi drop, a laptop that slept), the app
// still holds the old token and retries with it. That retry is a LOST ANSWER
// and is let through for as long as the old token itself is valid, bounded only
// by its own refresh expiry. There is no time window.
//
// No time limit is needed because the replacement check is what catches theft.
// If a thief presents the old token they get a pair, and the real app's
// unreceived replacement is cancelled; the real app's next renewal then
// presents a revoked token (or, if it still holds the old token, finds the
// thief's replacement already used) and everything is killed. A used token is
// only forgivable while its successor is unused, which is exactly as long as
// the successor itself would be usable to whoever could have taken the old
// one, so forgiving it adds no exposure.
func decideRotation(presented tokenState, replacement *tokenState, now time.Time) rotateVerdict {
	// 1. A killed token presented again is theft.
	if presented.RevokedAt != nil {
		return verdictReuse
	}
	// 2. Expired, used or not: a plain refusal, no kill. The JWT's own exp
	// check in Issuer.parse normally refuses an expired token before this runs;
	// this is the database's copy of the same rule.
	if now.After(presented.ExpiresAt) {
		return verdictInvalid
	}
	// 3. Never used: an ordinary rotation.
	if presented.UsedAt == nil {
		return verdictRotate
	}
	// 4. Used. Only a lost answer is forgiven.
	if presented.ReplacedBy == nil {
		return verdictReuse // killed before the migration, or no record of a replacement
	}
	if replacement == nil || replacement.UsedAt != nil || replacement.RevokedAt != nil {
		return verdictReuse
	}
	if now.After(replacement.ExpiresAt) {
		return verdictInvalid // nothing left to forgive, and no sign of theft
	}
	return verdictReissue
}
