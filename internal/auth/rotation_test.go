package auth

import (
	"testing"
	"time"
)

func TestDecideRotation(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	past := now.Add(-time.Hour)

	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	jti := func(s string) *string { return &s }

	// A used token whose replacement is the given state.
	used := func(usedAgo time.Duration) tokenState {
		return tokenState{ExpiresAt: future, UsedAt: ago(usedAgo), ReplacedBy: jti("next")}
	}
	freshReplacement := &tokenState{ExpiresAt: future}

	cases := []struct {
		name        string
		presented   tokenState
		replacement *tokenState
		want        rotateVerdict
	}{
		{
			name:      "never used and not expired rotates normally",
			presented: tokenState{ExpiresAt: future},
			want:      verdictRotate,
		},
		{
			name:      "never used but expired is just invalid",
			presented: tokenState{ExpiresAt: past},
			want:      verdictInvalid,
		},
		{
			name:      "revoked and never used is theft",
			presented: tokenState{ExpiresAt: future, RevokedAt: ago(time.Minute)},
			want:      verdictReuse,
		},
		{
			name:        "used 30 seconds ago with an unused replacement is a lost answer",
			presented:   used(30 * time.Second),
			replacement: freshReplacement,
			want:        verdictReissue,
		},
		{
			name:        "used 30 seconds ago but the replacement was used is theft",
			presented:   used(30 * time.Second),
			replacement: &tokenState{ExpiresAt: future, UsedAt: ago(10 * time.Second)},
			want:        verdictReuse,
		},
		{
			name:        "used 30 seconds ago but the replacement was revoked is theft",
			presented:   used(30 * time.Second),
			replacement: &tokenState{ExpiresAt: future, RevokedAt: ago(10 * time.Second)},
			want:        verdictReuse,
		},
		{
			name:        "used 6 hours ago with an unused replacement is a lost answer (a laptop that slept)",
			presented:   used(6 * time.Hour),
			replacement: freshReplacement,
			want:        verdictReissue,
		},
		{
			name:        "used 29 days ago with an unused replacement, still unexpired, is a lost answer",
			presented:   used(29 * 24 * time.Hour),
			replacement: freshReplacement,
			want:        verdictReissue,
		},
		{
			name:        "used 3 days ago but the replacement was used is theft",
			presented:   used(3 * 24 * time.Hour),
			replacement: &tokenState{ExpiresAt: future, UsedAt: ago(2 * 24 * time.Hour)},
			want:        verdictReuse,
		},
		{
			name:        "used with no recorded replacement (killed before the migration) is theft",
			presented:   tokenState{ExpiresAt: future, UsedAt: ago(30 * time.Second)},
			replacement: freshReplacement,
			want:        verdictReuse,
		},
		{
			name:        "used but the replacement row is gone is theft",
			presented:   used(30 * time.Second),
			replacement: nil,
			want:        verdictReuse,
		},
		{
			name:        "a used token that has expired is just invalid",
			presented:   tokenState{ExpiresAt: past, UsedAt: ago(30 * time.Second), ReplacedBy: jti("next")},
			replacement: freshReplacement,
			want:        verdictInvalid,
		},
		{
			name:        "used 30 seconds ago but the replacement has expired is just invalid",
			presented:   used(30 * time.Second),
			replacement: &tokenState{ExpiresAt: past},
			want:        verdictInvalid,
		},
		{
			name:        "second retry two minutes after the use, pointing at an unused re-issue, is still a lost answer",
			presented:   used(2 * time.Minute),
			replacement: freshReplacement,
			want:        verdictReissue,
		},
		{
			name:        "second retry hours later, pointing at an unused re-issue, is still a lost answer",
			presented:   used(5 * time.Hour),
			replacement: freshReplacement,
			want:        verdictReissue,
		},
		{
			name:        "a used and revoked token is theft",
			presented:   tokenState{ExpiresAt: future, UsedAt: ago(30 * time.Second), RevokedAt: ago(20 * time.Second), ReplacedBy: jti("next")},
			replacement: freshReplacement,
			want:        verdictReuse,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideRotation(tc.presented, tc.replacement, now); got != tc.want {
				t.Fatalf("decideRotation = %d, want %d", got, tc.want)
			}
		})
	}
}
