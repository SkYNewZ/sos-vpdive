// Package push sends the committee alerts that reach a phone without the
// mail: Web Push to the browsers a resolver subscribed (RFC 8030, 8291,
// 8292) and Pushover to the resolvers who gave a user key (spec §6, §9.6).
// A push carries a reference and a category, never a name or member text.
package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// recordSize is the aes128gcm record size: the message is one record.
	recordSize = 4096
	// maxPayload leaves room in the record for the delimiter and the tag.
	maxPayload = recordSize - 1 - 16
	authSize   = 16
	saltSize   = 16
	// keyIDSize is an uncompressed P-256 point, the key id of the header.
	keyIDSize = 65
)

// encrypt seals payload for a browser (RFC 8291, content coding aes128gcm).
// The caller draws serverKey and salt for each message; tests pass the
// RFC's. The result is the header (salt, record size, server public key)
// followed by the ciphertext.
func encrypt(payload, uaPublic, authSecret []byte, serverKey *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(payload) > maxPayload {
		return nil, fmt.Errorf("push payload of %d bytes exceeds one record", len(payload))
	}
	if len(authSecret) != authSize || len(salt) != saltSize {
		return nil, errors.New("push auth secret and salt are 16 bytes each")
	}
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("push subscription key: %w", err)
	}
	secret, err := serverKey.ECDH(ua)
	if err != nil {
		return nil, fmt.Errorf("push key agreement: %w", err)
	}
	asPublic := serverKey.PublicKey().Bytes()
	prkKey, err := hkdf.Extract(sha256.New, secret, authSecret)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(uaPublic)+string(asPublic), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, saltSize+5+keyIDSize)
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, keyIDSize)
	header = append(header, asPublic...)
	record := append(append(make([]byte, 0, len(payload)+1), payload...), 2) // 2: last record, no padding
	return gcm.Seal(header, nonce, record, nil), nil
}
