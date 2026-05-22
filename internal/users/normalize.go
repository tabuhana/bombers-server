package users

import "strings"

// asciiWhitespace is the trim cutset shared with the SQL CHECK constraint
// on users.username_canonical (see migrations/20260522120000_create_users.sql:
// lower(btrim(username, E' \t\n\r\v\f'))). Keep this set byte-identical to
// the SQL expression — any drift will cause INSERTs to fail the CHECK.
const asciiWhitespace = " \t\n\r\v\f"

// CanonicalUsername returns the lookup form of a username: trimmed of ASCII
// whitespace and lowercased. It MUST stay equivalent to the SQL expression
// lower(btrim(username, E' \t\n\r\v\f')) used in the users_username_canonical_matches
// CHECK constraint.
func CanonicalUsername(s string) string {
	return strings.ToLower(strings.Trim(s, asciiWhitespace))
}
