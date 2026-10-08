package assistant

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMask(t *testing.T) {
	text, emails := Mask("Écris à Lea.Martin@Example.ORG ou hugo@example.org, puis lea.martin@example.org.\n"+
		"Tél. 0612345678, 06 12 34 56 78, +33 6 12 34 56 78, 06.12.34.56.78\n"+
		"IBAN FR76 3000 6000 0112 3456 7890 189 et FR7630006000011234567890189.\n"+
		"Garde 05/01/2026, 05.01.2026, CPP-0042, 250 €, 0,50 €.", nil)
	assert.Equal(t, "Écris à [email 1] ou [email 2], puis [email 1].\n"+
		"Tél. [téléphone], [téléphone], [téléphone], [téléphone]\n"+
		"IBAN [iban] et [iban].\n"+
		"Garde 05/01/2026, 05.01.2026, CPP-0042, 250 €, 0,50 €.", text)
	assert.Equal(t, []string{"lea.martin@example.org", "hugo@example.org"}, emails)

	text, emails = Mask("Et hugo@example.org, chloe@example.org.", emails)
	assert.Equal(t, "Et [email 2], [email 3].", text, "numbers carry over a conversation")
	assert.Len(t, emails, 3)
}

// Codex review: a partial capture of o'connor@ would remember connor@, maybe
// another member's address, and find_member would resolve the wrong person.
func TestMaskKeepsWholeAddresses(t *testing.T) {
	text, emails := Mask("Écris à o'connor@example.org, pas à connor@example.org ni à jean+club@example.org.", nil)
	assert.Equal(t, "Écris à [email 1], pas à [email 2] ni à [email 3].", text)
	assert.Equal(t, []string{"o'connor@example.org", "connor@example.org", "jean+club@example.org"}, emails)
	got, ok := Placeholder("[email 1]", emails)
	assert.True(t, ok)
	assert.Equal(t, "o'connor@example.org", got)
}

func TestMaskSeparatorsAndCase(t *testing.T) {
	text, _ := Mask("+33-6-12-34-56-78, +33.6.12.34.56.78, 0033 6 12 34 56 78, +33 (0)6 12 34 56 78, fr7630006000011234567890189, Fr76 3000 6000 0112 3456 7890 189", nil)
	assert.Equal(t, "[téléphone], [téléphone], [téléphone], [téléphone], [iban], [iban]", text)
}

func TestPlaceholder(t *testing.T) {
	emails := []string{"lea.martin@example.org"}
	got, ok := Placeholder(" [email 1] ", emails)
	assert.True(t, ok)
	assert.Equal(t, "lea.martin@example.org", got)
	for _, q := range []string{"[email 2]", "[email 0]", "Léa [email 1]", "email 1"} {
		_, ok := Placeholder(q, emails)
		assert.False(t, ok, q)
	}
}
