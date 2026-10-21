package campaign

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

// parseSheet runs an in-memory sheet through the real reader.
func parseSheet(t *testing.T, s xlsxtest.Sheet) (*Export, error) {
	t.Helper()
	rows, err := xlsx.ReadFirstSheet(xlsxtest.Build(t, s), imports.Limits())
	require.NoError(t, err)
	return Parse(rows, paris(t))
}

func personOfRow(t *testing.T, exp *Export, row int) Person {
	t.Helper()
	for _, p := range exp.People {
		if p.Row == row {
			return p
		}
	}
	t.Fatalf("no person for row %d", row)
	return Person{}
}

func TestParseValidExport(t *testing.T) {
	loc := paris(t)
	day := func(d, m int) time.Time { return time.Date(2026, time.Month(m), d, 0, 0, 0, 0, loc) }

	exp, err := Parse(readFixture(t, "campaign_valid.xlsx"), loc)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 10, 21, 9, 15, 0, 0, loc), exp.Exported)
	assert.Len(t, exp.People, 10)
	assert.Equal(t, 1, exp.Skipped, "the nameless row")

	assert.Equal(t, Person{
		Row: 6, NameKey: "bernard|hugo",
		Status: StatusPending, LastName: "Bernard", FirstName: "Hugo",
		Membership: []Entry{{Label: "Adhésion", State: StatePaid, Amount: 5000}},
		Licence:    []Entry{{Label: "Licence fédérale", State: StatePaid, Amount: 4200, Method: "vpaydive", PaidOn: day(12, 10)}},
		Settlement: "Tout réglé",
	}, personOfRow(t, exp, 6))
	chloe := personOfRow(t, exp, 7)
	assert.Equal(t, StatusValidated, chloe.Status)
	assert.Equal(t, time.Date(2026, 10, 14, 18, 32, 0, 0, loc), chloe.ValidatedAt, "« : » between month and year")
	assert.Equal(t, "Hugo Bernard", chloe.ValidatedBy)
	assert.Equal(t, []Entry{{Label: "Adhésion", State: StatePaid, Amount: 5000, Method: "Virements", PaidOn: day(3, 10)}}, chloe.Membership)
	assert.Equal(t, []Entry{{Label: "Licence fédérale", State: StateInCart, Amount: 4200}}, personOfRow(t, exp, 8).Licence)
	lea := personOfRow(t, exp, 9)
	assert.Equal(t, time.Date(2026, 10, 2, 8, 5, 0, 0, loc), lea.ValidatedAt, "« / » between month and year")
	assert.Equal(t, []Entry{
		{Label: "Adhésion", State: StatePaid, Amount: 2500},
		{Label: "Adhésion", State: "Remboursé", Amount: 2500},
	}, lea.Membership, "one entry per line, an unknown state kept as received")

	leroy := personOfRow(t, exp, 11)
	assert.Equal(t, Person{Row: 11, NameKey: "leroy|", Status: StatusPending, LastName: "Leroy"}, leroy, "an empty first name is allowed")
	row, key := leroy.Origin()
	assert.Equal(t, 11, row)
	assert.Equal(t, "leroy|", key)
	assert.Equal(t, "inconnu|paul", personOfRow(t, exp, 12).NameKey)
	assert.Equal(t, "inconnu|paul", personOfRow(t, exp, 13).NameKey, "duplicates are kept: the store marks them")

	roux := personOfRow(t, exp, 15)
	assert.True(t, roux.ValidationUnreadable)
	assert.Zero(t, roux.ValidatedAt)
	assert.Empty(t, roux.ValidatedBy, "never the raw validation text")
	require.Len(t, roux.Membership, 1)
	assert.True(t, roux.Membership[0].Unreadable())
	assert.Equal(t, Entry{Raw: "Adhésion réglée en espèces"}, roux.Membership[0])
	assert.Equal(t, []Entry{{Label: "Licence fédérale", State: StatePaid, Amount: 104250}}, roux.Licence, "spaced thousands")
	assert.False(t, roux.Licence[0].Unreadable())
}

func TestParseNeverReadsIgnoredColumns(t *testing.T) {
	exp, err := Parse(readFixture(t, "campaign_valid.xlsx"), paris(t))
	require.NoError(t, err)
	data, err := json.Marshal(exp.People)
	require.NoError(t, err)
	for _, w := range []string{
		witnessAge, witnessSection, witnessCACI, witnessExtension, witnessLicence,
		witnessCard, witnessTraining, witnessRules, witnessBlank, "Validé par le bureau",
	} {
		assert.NotContains(t, string(data), w)
	}
}

func TestParseFindsTheHeader(t *testing.T) {
	people := []person{enrolled("Bernard", "Hugo", paidMembership, paidLicence, "Tout réglé")}

	s := campaignSheet(fixtureExported, people...)
	s[2] = []any{"Adhésion"} // a lone « Adhésion » above the header is no header
	exp, err := parseSheet(t, s)
	require.NoError(t, err)
	assert.Equal(t, 6, exp.People[0].Row)

	s = campaignSheet(fixtureExported, people...)
	s = append(xlsxtest.Sheet{nil, nil, nil, nil, nil, nil}, s...) // header on row 11
	_, err = parseSheet(t, s)
	assert.Equal(t, &ParseError{Kind: ProblemNoHeader}, err)
}

func TestParseRequiredAndOptionalColumns(t *testing.T) {
	_, err := Parse(readFixture(t, "campaign_missing_column.xlsx"), paris(t))
	assert.Equal(t, &ParseError{Kind: ProblemMissingColumn, Column: "License"}, err)

	for i, name := range []string{"Nom", "Prénom"} {
		s := campaignSheet(fixtureExported, invited("Bernard", "Hugo"))
		header := append([]any{}, campaignHeader...)
		header[i+1] = "Autre"
		s[4] = header
		_, err := parseSheet(t, s)
		assert.Equal(t, &ParseError{Kind: ProblemMissingColumn, Column: name}, err)
	}

	s := campaignSheet(fixtureExported, enrolled("Bernard", "Hugo", paidMembership, paidLicence, "Tout réglé"))
	header := append([]any{}, campaignHeader...)
	header[4], header[15] = "Validation", "Paiement" // both optional
	s[4] = header
	exp, err := parseSheet(t, s)
	require.NoError(t, err)
	assert.Empty(t, exp.People[0].Settlement)
	assert.False(t, exp.People[0].ValidationUnreadable)
}

func TestParseExportDate(t *testing.T) {
	for _, text := range []string{"Export effectué hier", "Export effectué le 32/10/2026 à 09:15", ""} {
		exp, err := parseSheet(t, campaignSheet(text, invited("Bernard", "Hugo")))
		require.NoError(t, err)
		assert.Zero(t, exp.Exported, "%q: indicative, never refused", text)
	}
}

func TestEntries(t *testing.T) {
	loc := paris(t)
	for text, want := range map[string][]Entry{
		"":                           nil,
		"Adhésion (Réglé) - 50,00 €": {{Label: "Adhésion", State: StatePaid, Amount: 5000}},
		"Adhésion (Réglé) - 50,00 € - vpaydive (12/10/2026)": {
			{Label: "Adhésion", State: StatePaid, Amount: 5000, Method: "vpaydive", PaidOn: time.Date(2026, 10, 12, 0, 0, 0, 0, loc)},
		},
		"Licence fédérale (Ajouté au panier mais pas encore payé) - 42,00 €": {
			{Label: "Licence fédérale", State: StateInCart, Amount: 4200},
		},
		"Adhésion famille (2 personnes) (Réglé) - 80 €": {{Label: "Adhésion famille (2 personnes)", State: StatePaid, Amount: 8000}},
		"Adhésion (Réglé) - 50,00 €\r\n\nAdhésion (Réglé) - 10,50 € - Espèces (01/09/2026)": {
			{Label: "Adhésion", State: StatePaid, Amount: 5000},
			{Label: "Adhésion", State: StatePaid, Amount: 1050, Method: "Espèces", PaidOn: time.Date(2026, 9, 1, 0, 0, 0, 0, loc)},
		},
		"Adhésion (Réglé) - 50,00 €":                       {{Label: "Adhésion", State: StatePaid, Amount: 5000}},
		"Adhésion (Réglé) - cinquante €":                   {{Raw: "Adhésion (Réglé) - cinquante €"}},
		"Adhésion (Réglé) - 50,00 € - Chèque (31/02/2026)": {{Raw: "Adhésion (Réglé) - 50,00 € - Chèque (31/02/2026)"}},
		"Adhésion - 50,00 €\nAdhésion (Réglé) - 50,00 €": {
			{Raw: "Adhésion - 50,00 €"},
			{Label: "Adhésion", State: StatePaid, Amount: 5000},
		},
	} {
		assert.Equal(t, want, entries(text, loc), "%q", text)
	}
}

func TestValidation(t *testing.T) {
	loc := paris(t)
	at := time.Date(2026, 10, 14, 18, 32, 0, 0, loc)
	for text, want := range map[string]Person{
		"": {},
		"Validé le 14/10:2026 18:32 par Hugo Bernard":               {ValidatedAt: at, ValidatedBy: "Hugo Bernard"},
		"Validé le 14/10/2026 18:32 par Hugo Bernard":               {ValidatedAt: at, ValidatedBy: "Hugo Bernard"},
		"Adhesion confirmée le 14/10:2026 18:32 par Hugo de la Mer": {ValidatedAt: at, ValidatedBy: "Hugo de la Mer"},
		"Validé par le bureau":                                      {ValidationUnreadable: true},
		"Validé le 31/02:2026 18:32 par Hugo Bernard":               {ValidationUnreadable: true},
		"Validé le 14/10:2026 18:32 par ":                           {ValidationUnreadable: true},
	} {
		var got Person
		got.ValidatedAt, got.ValidatedBy, got.ValidationUnreadable = validation(text, loc)
		assert.Equal(t, want, got, "%q", text)
	}
}
