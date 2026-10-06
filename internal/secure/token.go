package secure

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const tokenBytes = 32

// TokenLength is the length of a NewToken string: 32 bytes in unpadded base64url.
const TokenLength = (tokenBytes*8 + 5) / 6

// NewToken returns 32 random bytes encoded for use in a URL or a cookie.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// TokenHash is the SHA-256 of a random token, the only form stored in the
// database (a key adds nothing to 32 random bytes, spec §8.4).
func TokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// IsToken reports whether s has the shape of a NewToken string, so a lookup
// can refuse anything else without touching the database.
func IsToken(s string) bool {
	if len(s) != TokenLength {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}
