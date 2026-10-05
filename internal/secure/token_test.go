package secure

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsToken(t *testing.T) {
	tok, err := NewToken()
	require.NoError(t, err)
	assert.Len(t, tok, TokenLength)
	assert.True(t, IsToken(tok))
	assert.False(t, IsToken(tok[1:]))
	assert.False(t, IsToken(tok+"a"))
	assert.False(t, IsToken(strings.Repeat("+", TokenLength)))
	assert.False(t, IsToken(""))
}
