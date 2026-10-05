package admins

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$"), hash)

	ok, err := VerifyPassword(hash, "correct horse battery staple")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = VerifyPassword(hash, "wrong")
	require.NoError(t, err)
	assert.False(t, ok)

	other, err := HashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.NotEqual(t, hash, other, "random salt")
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	for _, h := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=18$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=99999999,t=3,p=4$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0$c2hvcnQ",
	} {
		_, err := VerifyPassword(h, "x")
		assert.Error(t, err, h)
	}
}
