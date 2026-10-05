package secure

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		in, want string
		err      error
	}{
		{"  Lea.Martin@Example.ORG \n", "lea.martin@example.org", nil},
		{" hugo@example.org ", "hugo@example.org", nil},
		{"lea martin@example.org", "", ErrEmailSpace},
		{"lea\tmartin@example.org", "", ErrEmailSpace},
		{"   ", "", ErrEmailEmpty},
	}
	for _, tt := range tests {
		got, err := NormalizeEmail(tt.in)
		if tt.err != nil {
			require.ErrorIs(t, err, tt.err, tt.in)
			continue
		}
		require.NoError(t, err, tt.in)
		assert.Equal(t, tt.want, got)
	}
}

func TestNormalizeName(t *testing.T) {
	assert.Equal(t, "eloisemariedarc", NormalizeName("Éloïse-Marie d'Arc"))
	assert.Equal(t, "lea", NormalizeName("Léa"), "decomposed accent")
	assert.Equal(t, "lea", NormalizeName(" LÉA "))
	assert.Empty(t, NormalizeName(" 42 - "))
}

func TestNameKey(t *testing.T) {
	assert.Equal(t, NameKey("Martin", "Léa"), NameKey("MARTIN ", "Lea"))
	assert.NotEqual(t, NameKey("Martin", "Léa"), NameKey("Léa", "Martin"))
	assert.NotEqual(t, NameKey("Ab", "C"), NameKey("A", "Bc"), "separator keeps parts apart")
}
