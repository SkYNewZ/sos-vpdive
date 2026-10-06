package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
)

// contentCipher derives the receiver's AES-GCM key and nonce (RFC 8291 §3.4).
func contentCipher(t *testing.T, secret, auth, uaPublic, asPublic, salt []byte) (cipher.AEAD, []byte) {
	t.Helper()
	prkKey, err := hkdf.Extract(sha256.New, secret, auth)
	require.NoError(t, err)
	ikm, err := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(uaPublic)+string(asPublic), 32)
	require.NoError(t, err)
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	require.NoError(t, err)
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	require.NoError(t, err)
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	require.NoError(t, err)
	block, err := aes.NewCipher(cek)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	return gcm, nonce
}
