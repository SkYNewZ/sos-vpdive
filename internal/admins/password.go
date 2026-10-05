package admins

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters: RFC 9106 §4, second recommended option.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
	// Bounds accepted when reading a hash, so a malformed accounts file cannot
	// make a login allocate gigabytes or loop for minutes.
	maxMemory = 1024 * 1024 // KiB
	maxTime   = 10
	minSalt   = 8
	minKey    = 16
)

var b64 = base64.RawStdEncoding

var errBadHash = errors.New("not an argon2id hash produced by hash-password")

type argonParams struct {
	time    uint32
	memory  uint32
	threads uint8
	salt    []byte
	key     []byte
}

// HashPassword returns a PHC string: $argon2id$v=19$m=65536,t=3,p=4$salt$key.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks password against a PHC string in constant time.
func VerifyPassword(encoded, password string) (bool, error) {
	p, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	key := argon2.IDKey([]byte(password), p.salt, p.time, p.memory, p.threads, uint32(len(p.key))) //nolint:gosec // G115: len is never negative and key size is bounded by the hash string
	return subtle.ConstantTimeCompare(key, p.key) == 1, nil
}

func parseHash(encoded string) (argonParams, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argonParams{}, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, fmt.Errorf("%w: unsupported version", errBadHash)
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return argonParams{}, fmt.Errorf("%w: parameters", errBadHash)
	}
	if p.memory == 0 || p.memory > maxMemory || p.time == 0 || p.time > maxTime || p.threads == 0 {
		return argonParams{}, fmt.Errorf("%w: parameters out of bounds", errBadHash)
	}
	var err error
	if p.salt, err = b64.DecodeString(parts[4]); err != nil || len(p.salt) < minSalt {
		return argonParams{}, fmt.Errorf("%w: salt", errBadHash)
	}
	if p.key, err = b64.DecodeString(parts[5]); err != nil || len(p.key) < minKey {
		return argonParams{}, fmt.Errorf("%w: key", errBadHash)
	}
	return p, nil
}
