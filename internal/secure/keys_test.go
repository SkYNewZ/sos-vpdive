package secure

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newKeys(t *testing.T, b byte) *Keys {
	t.Helper()
	k, err := NewKeys(bytes.Repeat([]byte{b}, 32))
	require.NoError(t, err)
	return k
}

func TestNewKeysRejectsWrongLength(t *testing.T) {
	_, err := NewKeys(make([]byte, 16))
	require.Error(t, err)
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := newKeys(t, 1)
	for _, s := range []string{"", "léa.martin@example.org", "un texte plus long\navec des retours"} {
		got, err := k.OpenString(k.SealString(s))
		require.NoError(t, err)
		assert.Equal(t, s, got)
	}
}

func TestSealFormatAndRandomNonce(t *testing.T) {
	k := newKeys(t, 1)
	a, b := k.SealString("x"), k.SealString("x")
	assert.Equal(t, byte(0x01), a[0], "format version byte")
	assert.Len(t, a, 1+12+1+16, "version, nonce, ciphertext, tag")
	assert.NotEqual(t, a, b, "nonce must differ between seals")
}

func TestOpenRejectsAlteredValues(t *testing.T) {
	k := newKeys(t, 1)
	sealed := k.SealString("secret")
	cases := map[string][]byte{
		"flipped tag":     append(append([]byte{}, sealed[:len(sealed)-1]...), sealed[len(sealed)-1]^1),
		"unknown version": append([]byte{0x02}, sealed[1:]...),
		"truncated":       sealed[:10],
		"empty":           nil,
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := k.Open(v)
			require.ErrorIs(t, err, ErrDecrypt)
		})
	}
}

func TestOpenRejectsOtherKey(t *testing.T) {
	_, err := newKeys(t, 2).Open(newKeys(t, 1).SealString("secret"))
	require.ErrorIs(t, err, ErrDecrypt)
}

func TestHashIsDeterministicAndKeyed(t *testing.T) {
	k1, k2 := newKeys(t, 1), newKeys(t, 2)
	//nolint:testifylint // verifying hash is deterministic, not useless assertion
	assert.Equal(t, k1.Hash("a@example.org"), k1.Hash("a@example.org"))
	assert.NotEqual(t, k1.Hash("a@example.org"), k1.Hash("b@example.org"))
	assert.NotEqual(t, k1.Hash("a@example.org"), k2.Hash("a@example.org"))
	assert.Len(t, k1.Hash("x"), 32)
	assert.Len(t, k1.HashHex("x"), 64)
}

func TestCSRF(t *testing.T) {
	k := newKeys(t, 1)
	session := TokenHash("session-token")
	token := k.CSRFToken(session)
	assert.True(t, k.CheckCSRF(session, token))
	assert.False(t, k.CheckCSRF(TokenHash("other"), token))
	assert.False(t, k.CheckCSRF(session, ""))
	assert.False(t, newKeys(t, 2).CheckCSRF(session, token))
}

func TestNewToken(t *testing.T) {
	a, err := NewToken()
	require.NoError(t, err)
	b, err := NewToken()
	require.NoError(t, err)
	assert.Len(t, a, 43)
	assert.NotEqual(t, a, b)
	assert.Len(t, TokenHash(a), 32)
}
