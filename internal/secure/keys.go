// Package secure holds the application's cryptography (spec §8.4): purpose
// keys derived from SECRET_KEY, column encryption, keyed hashes, CSRF tokens,
// random tokens, and the normalization rules that feed the hashes.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// HKDF info strings. They are a storage contract: changing one makes every
// existing value unreadable.
const (
	infoEncryption = "sos-vpdive/v1/encryption"
	infoHashing    = "sos-vpdive/v1/hashing"
	infoCSRF       = "sos-vpdive/v1/csrf"
	keySize        = 32
	sealVersion    = 0x01
	// sealMinSize is the version byte, a 12-byte nonce and a 16-byte tag.
	sealMinSize = 1 + 12 + 16
)

// ErrDecrypt reports a value that cannot be decrypted: wrong key or altered data.
var ErrDecrypt = errors.New("cannot decrypt value (wrong SECRET_KEY or altered data)")

// Keys holds the three purpose keys derived from SECRET_KEY.
type Keys struct {
	aead    cipher.AEAD
	hashKey []byte
	csrfKey []byte
}

// NewKeys derives the encryption, hashing and CSRF keys from the 32-byte secret.
func NewKeys(secret []byte) (*Keys, error) {
	if len(secret) != keySize {
		return nil, fmt.Errorf("secret must be %d bytes, got %d", keySize, len(secret))
	}
	derive := func(info string) ([]byte, error) {
		k, err := hkdf.Key(sha256.New, secret, nil, info, keySize)
		if err != nil {
			return nil, fmt.Errorf("derive %s: %w", info, err)
		}
		return k, nil
	}
	encKey, err := derive(infoEncryption)
	if err != nil {
		return nil, err
	}
	hashKey, err := derive(infoHashing)
	if err != nil {
		return nil, err
	}
	csrfKey, err := derive(infoCSRF)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	// The random-nonce GCM prepends its 12-byte nonce to the ciphertext,
	// which gives exactly the spec format once the version byte is added.
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Keys{aead: aead, hashKey: hashKey, csrfKey: csrfKey}, nil
}

// Seal encrypts plaintext as version byte ‖ nonce ‖ ciphertext ‖ tag.
func (k *Keys) Seal(plaintext []byte) []byte {
	//nolint:gosec // G407 false positive: nonce is generated randomly by NewGCMWithRandomNonce
	return k.aead.Seal([]byte{sealVersion}, nil, plaintext, nil)
}

// Open decrypts a value produced by Seal. Any failure is ErrDecrypt; it never
// returns an empty value in place of an error.
func (k *Keys) Open(sealed []byte) ([]byte, error) {
	if len(sealed) < sealMinSize || sealed[0] != sealVersion {
		return nil, ErrDecrypt
	}
	plaintext, err := k.aead.Open(nil, nil, sealed[1:], nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// SealString encrypts a string.
func (k *Keys) SealString(s string) []byte {
	return k.Seal([]byte(s))
}

// OpenString decrypts a value sealed by SealString.
func (k *Keys) OpenString(b []byte) (string, error) {
	p, err := k.Open(b)
	if err != nil {
		return "", err
	}
	return string(p), nil
}

// Hash returns the HMAC-SHA256 of value under the hashing key. It feeds
// email_hash, name_hash and rate-limit counter keys.
func (k *Keys) Hash(value string) []byte {
	m := hmac.New(sha256.New, k.hashKey)
	m.Write([]byte(value))
	return m.Sum(nil)
}

// HashHex is Hash encoded in hexadecimal, for text keys.
func (k *Keys) HashHex(value string) string {
	return hex.EncodeToString(k.Hash(value))
}

// CSRFToken returns the anti-CSRF token bound to a session token hash.
func (k *Keys) CSRFToken(sessionHash []byte) string {
	m := hmac.New(sha256.New, k.csrfKey)
	m.Write(sessionHash)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// CheckCSRF compares a submitted token with the expected one in constant time.
func (k *Keys) CheckCSRF(sessionHash []byte, token string) bool {
	return subtle.ConstantTimeCompare([]byte(k.CSRFToken(sessionHash)), []byte(token)) == 1
}
