package users

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

const (
	friendCodeGroups         = 4
	friendCodeDigitsPerGroup = 4
)

// GenerateFriendCode returns a fresh friend code in XXXX-XXXX-XXXX-XXXX form
// (16 decimal digits drawn from crypto/rand, grouped in fours with hyphens).
// Uniqueness is the caller's responsibility — handle the UNIQUE-constraint
// collision and retry.
func GenerateFriendCode() (string, error) {
	var b strings.Builder
	b.Grow(friendCodeGroups*friendCodeDigitsPerGroup + friendCodeGroups - 1)

	ten := big.NewInt(10)
	for g := 0; g < friendCodeGroups; g++ {
		if g > 0 {
			b.WriteByte('-')
		}
		for d := 0; d < friendCodeDigitsPerGroup; d++ {
			n, err := rand.Int(rand.Reader, ten)
			if err != nil {
				return "", fmt.Errorf("generate friend code digit: %w", err)
			}
			b.WriteByte('0' + byte(n.Int64()))
		}
	}
	return b.String(), nil
}
