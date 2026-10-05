package secure

import (
	"errors"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Email normalization errors.
var (
	ErrEmailEmpty = errors.New("email is empty")
	ErrEmailSpace = errors.New("email contains a space")
)

// NormalizeEmail trims and lowercases an address (spec §3.6). An address that
// still contains a space is refused. Import, form, hash and lost-link page all
// use this one function.
func NormalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", ErrEmailEmpty
	}
	if strings.ContainsFunc(s, unicode.IsSpace) {
		return "", ErrEmailSpace
	}
	return s, nil
}

// NormalizeName lowercases, strips accents and keeps letters only (spec §7.3).
func NormalizeName(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NameKey is the value hashed into name_hash: last and first names normalized
// and joined by a separator that NormalizeName never produces.
func NameKey(last, first string) string {
	return NormalizeName(last) + "|" + NormalizeName(first)
}
