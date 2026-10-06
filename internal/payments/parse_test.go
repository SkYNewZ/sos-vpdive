package payments

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	return loc
}

func parseError(t *testing.T, err error) *ParseError {
	t.Helper()
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	return pe
}

// parseSheet runs an in-memory export through the real reader.
func parseSheet(t *testing.T, lines ...line) (*Export, error) {
	t.Helper()
	rows, err := xlsx.ReadFirstSheet(xlsxtest.Build(t, sheet(lines...)), imports.Limits())
	require.NoError(t, err)
	return Parse(rows, time.Time{}, paris(t))
}

func lineOfRow(t *testing.T, exp *Export, row int) Line {
	t.Helper()
	for _, l := range exp.Lines {
		if l.Row == row {
			return l
		}
	}
	t.Fatalf("no line for row %d", row)
	return Line{}
}

func TestParseValidExport(t *testing.T) {
	rows, created := readFixture(t, "payments_valid.xlsx")
	loc := paris(t)
	at := func(day, month, hour, minute int) time.Time {
		return time.Date(2026, time.Month(month), day, hour, minute, 0, 0, loc)
	}

	exp, err := Parse(rows, created, loc)
	require.NoError(t, err)
	assert.True(t, fixtureCreated.Equal(exp.Created))
	assert.Len(t, exp.Lines, 20)
	assert.Equal(t, 1, exp.Skipped, "the nameless line")
	assert.Equal(t, map[string]int{"En attente": 1}, exp.UnknownStates)
	assert.True(t, at(5, 1, 10, 12).Equal(exp.PeriodFrom), "%v", exp.PeriodFrom)
	assert.True(t, at(20, 6, 15, 0).Equal(exp.PeriodTo), "%v", exp.PeriodTo)

	assert.Equal(t, Line{
		Row: 2, NameKey: "bernard|hugo",
		UnitPrice: 30000, Quantity: 100, Paid: 30000,
		State: StatePaid, Method: "vpaydive", ProductType: TypeCard, Product: carnetTitle,
		PaidAt: at(5, 1, 10, 30), Created: at(5, 1, 10, 12),
	}, lineOfRow(t, exp, 2), "native date with a time of day, read in Paris time")
	assert.Equal(t, Amount(-18000), lineOfRow(t, exp, 3).UnitPrice)
	assert.Equal(t, at(10, 5, 9, 0), lineOfRow(t, exp, 6).Starts, "text date with a time")
	assert.Equal(t, at(8, 5, 20, 0), lineOfRow(t, exp, 7).Starts, "native date, 20:00")
	assert.Equal(t, Amount(1050), lineOfRow(t, exp, 9).Discount)
	assert.Equal(t, Amount(-3500), lineOfRow(t, exp, 10).Paid)
	assert.Equal(t, "En attente", lineOfRow(t, exp, 11).State, "an unknown state is kept as read")
	assert.Equal(t, Amount(800), lineOfRow(t, exp, 13).Rental, "the second Materiel is the rental amount")
	assert.Equal(t, "martin|lea", lineOfRow(t, exp, 16).NameKey)
	assert.Equal(t, lineOfRow(t, exp, 16).NameKey, lineOfRow(t, exp, 17).NameKey, "homonyms share the key")
	assert.Equal(t, "leroy|", lineOfRow(t, exp, 18).NameKey)
	assert.Equal(t, at(1, 6, 12, 0), lineOfRow(t, exp, 20).Created, "native creation date")

	text := lineOfRow(t, exp, 21)
	assert.Equal(t, Amount(1250), text.UnitPrice, "text amount with a decimal comma")
	assert.Equal(t, Amount(101250), text.Paid, "spaces as thousands separators")
	assert.Empty(t, text.Product, "an isolated empty title is kept")
}

func TestParseNamesTheMissingColumn(t *testing.T) {
	rows, created := readFixture(t, "payments_missing_column.xlsx")
	_, err := Parse(rows, created, paris(t))
	pe := parseError(t, err)
	assert.Equal(t, ProblemMissingColumn, pe.Kind)
	assert.Equal(t, "Montant paiement", pe.Column)

	rows, err = xlsx.ReadFirstSheet(xlsxtest.Build(t, xlsxtest.Sheet{{"Nom", "Prénom"}, {"Bernard", "Hugo"}}), imports.Limits())
	require.NoError(t, err)
	_, err = Parse(rows, time.Time{}, paris(t))
	pe = parseError(t, err)
	assert.Equal(t, ProblemNoHeader, pe.Kind)
	assert.Contains(t, pe.Error(), `"Créé le"`)
}

func TestParseRefusesUnreadableAmountsWithTheirRows(t *testing.T) {
	ok := paid("Bernard", "Hugo", 30, "vpaydive", "Baptême", "05/01/2026 10:12:00")
	words := paid("Bernard", "Hugo", 30, "vpaydive", "Baptême", "05/01/2026 10:12:00")
	words["Prix unitaire"] = "trente"
	twoDots := paid("Bernard", "Hugo", 30, "vpaydive", "Baptême", "05/01/2026 10:12:00")
	twoDots["Montant réduc."] = "12.5.1"
	fraction := paid("Bernard", "Hugo", 30, "vpaydive", "Baptême", "05/01/2026 10:12:00")
	fraction["Quantité"] = "1/2"

	_, err := parseSheet(t, ok, words, twoDots, fraction)
	pe := parseError(t, err)
	assert.Equal(t, ProblemInvalidNumber, pe.Kind)
	assert.Equal(t, []int{3, 4, 5}, pe.Rows)
}

func TestParseRefusesUnreadableCreationDates(t *testing.T) {
	bad := paid("Bernard", "Hugo", 30, "vpaydive", "Baptême", "bientôt")
	empty := paid("Bernard", "Hugo", 30, "vpaydive", "Baptême", "")
	_, err := parseSheet(t, paid("Bernard", "Hugo", 30, "vpaydive", "Baptême", "05/01/2026 10:12:00"), bad, empty)
	pe := parseError(t, err)
	assert.Equal(t, ProblemInvalidDate, pe.Kind)
	assert.Equal(t, []int{3, 4}, pe.Rows)
}

func TestParseKeepsUnreadableOptionalDatesEmpty(t *testing.T) {
	l := prepaid("Bernard", "Hugo", 30, "Sortie", "un jour", "05/01/2026 10:12:00")
	l["Date paiement"] = "32/13/2026"
	exp, err := parseSheet(t, l)
	require.NoError(t, err)
	assert.True(t, exp.Lines[0].Starts.IsZero())
	assert.True(t, exp.Lines[0].PaidAt.IsZero())
}

// The known export bug leaves "Produit/Événement" empty: more than 5 % of
// empty titles refuses the file, an isolated one is kept (spec §7.3).
func TestParseRefusesAFileWithManyEmptyTitles(t *testing.T) {
	lines := make([]line, 20)
	for i := range lines {
		lines[i] = paid("Bernard", "Hugo", 30, "vpaydive", "Baptême", "05/01/2026 10:12:00")
	}
	lines[0]["Produit/Événement"] = nil
	exp, err := parseSheet(t, lines...)
	require.NoError(t, err, "1 in 20 is 5 percent: accepted")
	assert.Len(t, exp.Lines, 20)

	lines[1]["Produit/Événement"] = "  "
	_, err = parseSheet(t, lines...)
	assert.Equal(t, ProblemEmptyProduct, parseError(t, err).Kind)
}

func TestParseCountsEmptyAmountsAsZero(t *testing.T) {
	l := paid("Bernard", "Hugo", 30, "vpaydive", "Baptême", "05/01/2026 10:12:00")
	l["Montant réduc."] = nil
	exp, err := parseSheet(t, l)
	require.NoError(t, err)
	assert.Zero(t, exp.Lines[0].Discount)
}

func TestAmount(t *testing.T) {
	for _, c := range []struct {
		kind xlsx.Kind
		text string
		want Amount
	}{
		{xlsx.KindNumber, "180", 18000},
		{xlsx.KindNumber, "-180.0", -18000},
		{xlsx.KindNumber, "14.5", 1450},
		{xlsx.KindNumber, "13.999999999999998", 1400},
		{xlsx.KindNumber, "1E-2", 1},
		{xlsx.KindNumber, "0.005", 1},
		{xlsx.KindNumber, "-0.005", -1},
		{xlsx.KindString, "12,50", 1250},
		{xlsx.KindString, "-0,5", -50},
		{xlsx.KindString, " 1 012,50 ", 101250},
		{xlsx.KindString, "1\u00a0012,50", 101250},
		{xlsx.KindString, "1\u202f012,50", 101250},
		{xlsx.KindString, "+3", 300},
	} {
		got, ok := amount(xlsx.Cell{Kind: c.kind, Text: c.text})
		assert.True(t, ok, c.text)
		assert.Equal(t, c.want, got, c.text)
	}
	for _, bad := range []string{"trente", "1/2", "12.5.1", "1e3", "12,50 €", "99999999999999999999", "-"} {
		_, ok := amount(xlsx.Cell{Kind: xlsx.KindString, Text: bad})
		assert.False(t, ok, bad)
	}
	_, ok := amount(xlsx.Cell{Kind: xlsx.KindBool, Text: "1"})
	assert.False(t, ok, "a boolean is not an amount")
}

func TestAmountFormatting(t *testing.T) {
	assert.Equal(t, "180,00\u00a0€", Amount(18000).Euros())
	assert.Equal(t, "-180,00\u00a0€", Amount(-18000).Euros())
	assert.Equal(t, "1\u00a0012,50\u00a0€", Amount(101250).Euros())
	assert.Equal(t, "0,05\u00a0€", Amount(5).Euros())
	assert.Equal(t, "180,00\u00a0€", Amount(-18000).Abs().Euros())
	assert.Equal(t, "1", Amount(100).Number())
	assert.Equal(t, "1,5", Amount(150).Number())
	assert.Equal(t, "-1,25", Amount(-125).Number())
}
