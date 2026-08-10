package users

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	UsernameMinLen = 3
	UsernameMaxLen = 32
)

var (
	ErrUsernameEmpty      = errors.New("username is required")
	ErrUsernameLength     = errors.New("username must be between 3 and 32 characters")
	ErrUsernameWhitespace = errors.New("username may not contain whitespace")
)

// validateUsername trims the incoming value with the same cutset used by
// CanonicalUsername / the SQL CHECK, then enforces length and a no-whitespace
// rule. Returns the trimmed display value to store.
func validateUsername(raw string) (string, error) {
	trimmed := strings.Trim(raw, asciiWhitespace)
	if trimmed == "" {
		return "", ErrUsernameEmpty
	}
	if n := utf8.RuneCountInString(trimmed); n < UsernameMinLen || n > UsernameMaxLen {
		return "", ErrUsernameLength
	}
	for _, r := range trimmed {
		if unicode.IsSpace(r) {
			return "", ErrUsernameWhitespace
		}
	}
	return trimmed, nil
}
