package auth

import "time"

// LostAnswerWindow is how long after a refresh token was rotated its owner may
// present it again and be treated as having never received the reply.
const LostAnswerWindow = 10 * time.Minute

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
// to a rotation never reached the app (wifi drop, restart), the app still holds
// the old token and retries with it. That retry is a LOST ANSWER and is let
// through, once, inside LostAnswerWindow of the original use.
//
// WHY the replacement check exists: the replacement is the token the server
// handed out in the lost reply. If it has been used or revoked, then the real
// app is already running on it (or it was cancelled), so the old token can
// only be coming from someone else. The window therefore never lets a stolen
// token race the real one: the moment the real app moves on, the old token is
// theft again, inside the window or not.
func decideRotation(presented tokenState, replacement *tokenState, now time.Time) rotateVerdict {
	// 1. A killed token presented again is theft.
	if presented.RevokedAt != nil {
		return verdictReuse
	}
	// 2. Never used: an ordinary rotation, unless it has expired.
	if presented.UsedAt == nil {
		if now.After(presented.ExpiresAt) {
			return verdictInvalid
		}
		return verdictRotate
	}
	// 3. Used. Only a lost answer is forgiven.
	if presented.ReplacedBy == nil {
		return verdictReuse // killed before the migration, or no record of a replacement
	}
	if now.Sub(*presented.UsedAt) > LostAnswerWindow {
		return verdictReuse // exactly LostAnswerWindow is still inside
	}
	if now.After(presented.ExpiresAt) {
		return verdictReuse
	}
	if replacement == nil || replacement.UsedAt != nil || replacement.RevokedAt != nil {
		return verdictReuse
	}
	if now.After(replacement.ExpiresAt) {
		return verdictReuse
	}
	return verdictReissue
}
