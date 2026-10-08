package assistant

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	text, _ = Mask("GB29 NWBK 6016 1331 9268 19, NL91ABNA0417164300, DE89 3704 0044 0532 0130 00", nil)
	assert.Equal(t, "[iban], [iban], [iban]", text, "letters in the account number, 12 digits in all")
}

func TestMaskUnicodeSpacesAndForeignNumbers(t *testing.T) {
	cases := map[string]string{
		"no-break space phone":        "06\u00a012\u00a034\u00a056\u00a078",
		"narrow no-break space phone": "06\u202f12\u202f34\u202f56\u202f78",
		"no-break space +33 phone":    "+33\u00a06\u00a012\u00a034\u00a056\u00a078",
		"parenthesised +33":           "(+33) 6 12 34 56 78",
		"parenthesised +33 glued":     "(+33)6 12 34 56 78",
		"parenthesised 0033":          "(0033) 6 12 34 56 78",
		"parenthesised +33 and (0)":   "(+33) (0)6 12 34 56 78",
		"belgian":                     "+32 475 12 34 56",
		"belgian with 00":             "0032 475 12 34 56",
		"swiss":                       "+41 79 123 45 67",
		"british":                     "+44 7911 123456",
		"parenthesised belgian":       "(+32) 475 12 34 56",
		"no-break space iban":         "FR76\u00a03000\u00a06000\u00a00112\u00a03456\u00a07890\u00a0189",
		"narrow no-break space iban":  "FR76\u202f3000\u202f6000\u202f0112\u202f3456\u202f7890\u202f189",
	}
	for name, in := range cases {
		want := "[téléphone]"
		if strings.Contains(name, "iban") {
			want = "[iban]"
		}
		text, _ := Mask("Appelle "+in+".", nil)
		assert.Equal(t, "Appelle "+want+".", text, name)
	}
}

func TestMaskLeavesDatesAmountsAndRequestNumbers(t *testing.T) {
	in := "Sortie 05/01/2026, 05.01.2026, 15/08/2026 à 8h30, 0,50 €, 120 €, 1\u00a0250,00 €, CPP-0042, version 3.12.1."
	text, emails := Mask(in, nil)
	assert.Equal(t, in, text)
	assert.Empty(t, emails)

	// Dive levels look like an IBAN's head: PA40 and the next words are no IBAN.
	for _, in := range []string{"Sortie PA40 Porquerolles samedi", "Formation PE40 avec Marc dans la fosse",
		"PA40 Port Cros 2026", "pe60 niveau trois au port"} {
		text, _ := Mask(in, nil)
		assert.Equal(t, in, text)
	}
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

// Encoded, a line break or an angle bracket sticks to what follows it: Mask on
// the encoded document misses the number and records "u003cjean@…".
func TestMaskJSON(t *testing.T) {
	doc := `{"0612345678":"cle","n":12,"prix":1.50e2,"ok":true,"rien":null,` +
		`"liste":["a\n06 12 34 56 78","b\nFR7630006000011234567890189"],` +
		`"note":"\u003cjean@example.org\u003e et \"Hugo\" \\ 0612345678","ré":{"x":"hugo@example.org"}}`
	out, emails, err := MaskJSON([]byte(doc), []string{"hugo@example.org"})
	require.NoError(t, err)
	//nolint:testifylint // the exact text is the point: key order, number text and escapes, which JSONEq ignores
	assert.Equal(t, `{"0612345678":"cle","n":12,"prix":1.50e2,"ok":true,"rien":null,`+
		`"liste":["a\n[téléphone]","b\n[iban]"],`+
		`"note":"\u003c[email 2]\u003e et \"Hugo\" \\ [téléphone]","ré":{"x":"[email 1]"}}`, string(out),
		"keys, numbers and order as written, strings masked and encoded again")
	assert.Equal(t, []string{"hugo@example.org", "jean@example.org"}, emails, "the address, not an escape glued to it")
	assert.True(t, json.Valid(out))

	spaced, _, err := MaskJSON([]byte("{ \"0612345678\" : \"0612345678\" }"), nil)
	require.NoError(t, err)
	//nolint:testifylint // the exact text is the point: the spaces around the colon, which JSONEq ignores
	assert.Equal(t, "{ \"0612345678\" : \"[téléphone]\" }", string(spaced), "a key is told by its colon, spaces apart")
}

func TestMaskJSONRefusesMalformedDocuments(t *testing.T) {
	for _, doc := range []string{`{"a":"b`, `{"a":"b\`, `{"a":"\x"}`} {
		out, emails, err := MaskJSON([]byte(doc), []string{"a@example.org"})
		require.ErrorIs(t, err, errMaskJSON, doc)
		assert.Nil(t, out)
		assert.Nil(t, emails)
		assert.NotContains(t, err.Error(), "\\x", "nothing of the document in the error")
	}
}
