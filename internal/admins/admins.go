// Package admins keeps the committee accounts (spec §4.1 as amended):
// identifier, display name, function and argon2id password hash, in the
// database. The owner creates them on the committee site; a temporary
// password is changed at the first sign-in.
package admins

import (
	"crypto/rand"
	"crypto/sha256"
	"html/template"
	"regexp"
	"strings"
)

// MinPasswordLength is the shortest password a resolver may choose.
const MinPasswordLength = 12

var (
	usernamePattern = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)
	// pushoverKeyPattern is the shape of a Pushover user key.
	pushoverKeyPattern = regexp.MustCompile(`^[A-Za-z0-9]{30}$`)
)

// Account is one committee member. Name and Role are shown wherever the
// account is cited; there are no permission levels. PushoverUserKey, when
// set, gets the committee alerts on Pushover (spec §6, as amended).
type Account struct {
	Username           string
	Name               string
	Role               string
	PasswordHash       string
	MustChangePassword bool // a temporary password is in place
	PushoverUserKey    string
	Avatar             template.URL
}

// CredentialHash fingerprints the password hash. A session stores it and
// stays valid only while it matches the account.
func (a Account) CredentialHash() []byte {
	sum := sha256.Sum256([]byte(a.PasswordHash))
	return sum[:]
}

// NormalizeUsername is the form a username typed by a person is looked up
// in: a phone keyboard that capitalizes the first letter or adds a space must
// not matter.
func NormalizeUsername(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidUsername reports whether u can identify an account.
func ValidUsername(u string) bool {
	return usernamePattern.MatchString(u)
}

// ValidPushoverKey reports whether k looks like a Pushover user key.
func ValidPushoverKey(k string) bool {
	return pushoverKeyPattern.MatchString(k)
}

// temporaryAlphabet leaves out 0, 1, l and o, easily misread when the
// password is read aloud or copied from a screen. 32 symbols: a random byte
// masked to 5 bits picks one uniformly.
const temporaryAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

// TemporaryPassword returns 16 random symbols (80 bits) in four groups of
// four joined by dashes, such as kx7p-29mq-tr4w-hn3c. The dashes are part of
// the password.
func TemporaryPassword() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) //nolint:forbidigo // unreachable: crypto/rand.Read never returns an error on Go ≥ 1.24
	}
	var out strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			out.WriteByte('-')
		}
		out.WriteByte(temporaryAlphabet[c&31])
	}
	return out.String()
}
