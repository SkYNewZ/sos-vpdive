package assistant

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
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

// Codex review: an address with letters of any script, in its local part or
// in its domain, is masked whole (never « lé[email 1] ») and resolves to the
// address the members import stores, through secure.NormalizeEmail.
func TestMaskUnicodeAddresses(t *testing.T) {
	addrs := []string{"léa@example.org", "lea@école.fr", "LÉA.Ünal@Straße.de", "иван@пример.рф",
		"le\u0301a@example.org", "chloé2@exemple.fr"}
	text, emails := Mask("À : «"+strings.Join(addrs, "», «")+"».", nil)
	assert.Equal(t, "À : «[email 1]», «[email 2]», «[email 3]», «[email 4]», «[email 5]», «[email 6]».", text)
	for i, addr := range addrs {
		want, err := secure.NormalizeEmail(addr)
		require.NoError(t, err)
		got, ok := Placeholder("[email "+strconv.Itoa(i+1)+"]", emails)
		assert.True(t, ok, addr)
		assert.Equal(t, want, got, "the address as the members list stores it")
	}
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
		"PA40 Port Cros 2026", "pe60 niveau trois au port", "PA40 Cros"} {
		text, _ := Mask(in, nil)
		assert.Equal(t, in, text)
	}
}

// Re-review: a dive level and the words after it may run into the head of
// an IBAN; the IBAN is masked all the same.
func TestMaskIBANAfterADiveLevel(t *testing.T) {
	const iban = "FR76 3000 6000 0112 3456 7890 189"
	for _, head := range []string{"Sortie PA40 Port Cros avec Marc dans ", "PA40 Porquerolles avec Marc "} {
		text, _ := Mask(head+iban, nil)
		assert.Equal(t, head+"[iban]", text)
	}
}

// Re-review: a dive level whose seven groups end inside an IBAN passed for
// the IBAN, and the rest of the account number went through unmasked.
func TestMaskIBANTailAfterADiveLevel(t *testing.T) {
	const iban = "FR76 3000 6000 0112 3456 7890 189"
	for _, c := range []struct{ in, want string }{
		{"PA40 Porquerolles avec " + iban, "PA40 Porquerolles avec [iban]"},
		{"Sortie PE60 Port Cros " + iban + ".", "Sortie PE60 Port Cros [iban]."},
		{"pa40 porquerolles avec " + strings.ToLower(iban), "pa40 porquerolles avec [iban]"},
		{"PA40 Porquerolles avec " + strings.ReplaceAll(iban, " ", "\u00a0"), "PA40 Porquerolles avec [iban]"},
		{strings.ReplaceAll("PA40 Porquerolles avec "+iban, " ", "\u00a0"), "PA40\u00a0Porquerolles\u00a0avec\u00a0[iban]"},
		{"PA40 PE40 Porquerolles " + iban, "PA40 PE40 Porquerolles [iban]"},
	} {
		text, _ := Mask(c.in, nil)
		assert.Equal(t, c.want, text, c.in)
		assert.NotContains(t, text, "7890", c.in)
	}
}

// An IBAN grouped with dashes or dots, as some banks print it, reached the
// model whole; dive levels joined to their words that way stay.
func TestMaskIBANGroupedWithDashesOrDots(t *testing.T) {
	for _, in := range []string{
		"FR76-3000-6000-0112-3456-7890-189",
		"FR76.3000.6000.0112.3456.7890.189",
		"fr76-3000-6000-0112-3456-7890-189",
		"FR76 3000-6000.0112 3456-7890 189",
		"GB29-NWBK-6016-1331-9268-19",
		"NO93.8601.1117.947",
	} {
		text, _ := Mask("IBAN : "+in+".", nil)
		assert.Equal(t, "IBAN : [iban].", text, in)
	}
	for _, c := range []struct{ in, want string }{
		{"PA40 Porquerolles avec FR76-3000-6000-0112-3456-7890-189", "PA40 Porquerolles avec [iban]"},
		{"PA40-Port-Cros avec FR76.3000.6000.0112.3456.7890.189.", "PA40-Port-Cros avec [iban]."},
	} {
		text, _ := Mask(c.in, nil)
		assert.Equal(t, c.want, text, c.in)
	}
	for _, in := range []string{"PA40-Port-Cros samedi", "Sortie PE40.Porquerolles.2026", "pe60-niveau-trois au port",
		"AB12.3456.7890.12"} { // 14 characters, separators apart: shorter than any IBAN
		text, _ := Mask(in, nil)
		assert.Equal(t, in, text)
	}
}

// Codex review: Norway's IBANs have 15 characters, the fewest of any.
func TestMaskShortestIBAN(t *testing.T) {
	for _, in := range []string{"NO9386011117947", "NO93 8601 1117 947", "no93 8601 1117 947", "NO93\u00a08601\u00a01117\u00a0947"} {
		text, _ := Mask("IBAN "+in+".", nil)
		assert.Equal(t, "IBAN [iban].", text, in)
	}
}

// Final review: a French bank account in RIB grouping (bank 5 digits, branch
// 5, account 11 letters or digits, key 2), with or without its FR prefix,
// is an IBAN all the same.
func TestMaskRIB(t *testing.T) {
	for _, in := range []string{
		"FR76 30006 00001 12345678901 89",
		"30006 00001 12345678901 89",
		"fr76 30006 00001 1234567890a 89",
		"FR76\u00a030006\u00a000001\u00a012345678901\u00a089",
		"30006\u202f00001\u202f12345678901\u202f89",
		"30006.00001.12345678901.89",
		"FR76-30006-00001-12345678901-89",
		"30006000011234567890189",
	} {
		text, _ := Mask("RIB : "+in+".", nil)
		assert.Equal(t, "RIB : [iban].", text, in)
	}
	text, _ := Mask("PA40 Porquerolles avec FR76 30006 00001 12345678901 89", nil)
	assert.Equal(t, "PA40 Porquerolles avec [iban]", text, "after a dive level")

	const kept = "83000 Toulon, 83000 12345, 25 €, 30006 00001 89."
	text, _ = Mask(kept, nil)
	assert.Equal(t, kept, text, "postcodes and short numbers are no RIB")
}

// Codex review: a number typed with doubled spaces or pasted from a table
// has runs of blanks (spaces of any kind, tabs) between its groups. A line
// break never joins two groups.
func TestMaskRunsOfBlanks(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"06  12  34  56  78", "[téléphone]"},
		{"06\t12\t34\t56\t78", "[téléphone]"},
		{"+33 \t6  12  34  56  78", "[téléphone]"},
		{"+32\t\t475 12 34 56", "[téléphone]"},
		{"FR76\t3000\t6000\t0112\t3456\t7890\t189", "[iban]"},
		{"FR76  3000  6000  0112  3456  7890  189", "[iban]"},
		{"FR76\u00a0 3000 \u00a06000\u202f\t0112 3456 7890 189", "[iban]"},
		{"GB29\tNWBK\t6016\t1331\t9268\t19", "[iban]"},
		{"30006\t00001\t12345678901\t89", "[iban]"},
		{"FR76\t\t30006\t\t00001\t\t12345678901\t\t89", "[iban]"},
	} {
		text, _ := Mask("Copie : "+c.in+".", nil)
		assert.Equal(t, "Copie : "+c.want+".", text, c.in)
	}
	for _, in := range []string{
		"06\n12\n34\n56\n78",
		"06 12\r\n34 56 78",
		"05/01/2026\t250 €\tCPP-0042\tPA40  Porquerolles",
		"PA40\tPorquerolles\tsamedi",
		"PE40\t\tavec  Marc  dans  la  fosse",
		"0,50 €\t\t120 €\t1\u00a0250,00 €",
		"2026\t2025\t2024\t2023",
	} {
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
