package users

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	UsernameMinLen   = 3
	UsernameMaxLen   = 32
	PasswordMinBytes = 8
	PasswordMaxBytes = 72 // bcrypt's hard input ceiling — reject rather than truncate
)

var (
	ErrUsernameEmpty      = errors.New("username is required")
	ErrUsernameLength     = errors.New("username must be between 3 and 32 characters")
	ErrUsernameWhitespace = errors.New("username may not contain whitespace")
	ErrPasswordLength     = errors.New("password must be between 8 and 72 bytes")
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

// validatePassword enforces the byte-length window. Length is measured in
// bytes, not runes, because bcrypt operates on bytes and silently truncates
// inputs over 72 — rejecting here surfaces the limit to the caller.
func validatePassword(p string) error {
	n := len(p)
	if n < PasswordMinBytes || n > PasswordMaxBytes {
		return ErrPasswordLength
	}
	return nil
}
