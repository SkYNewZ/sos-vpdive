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

// parseMollieSheet runs an in-memory VPayDive export through the real reader.
func parseMollieSheet(t *testing.T, rows ...mollieRow) (*MollieExport, error) {
	t.Helper()
	read, err := xlsx.ReadFirstSheet(xlsxtest.Build(t, mollieSheet(rows...)), imports.Limits())
	require.NoError(t, err)
	return ParseMollie(read, time.Time{}, paris(t))
}

func mollieLineOfRow(t *testing.T, exp *MollieExport, row int) MollieLine {
	t.Helper()
	for _, l := range exp.Lines {
		if l.Row == row {
			return l
		}
	}
	t.Fatalf("no line for row %d", row)
	return MollieLine{}
}

func TestParseMollieValidExport(t *testing.T) {
	rows, created := readFixture(t, "vpaydive_valid.xlsx")
	loc := paris(t)
	at := func(day, month, hour, minute int) time.Time {
		return time.Date(2026, time.Month(month), day, hour, minute, 0, 0, loc)
	}

	exp, err := ParseMollie(rows, created, loc)
	require.NoError(t, err)
	assert.True(t, mollieCreated.Equal(exp.Created))
	assert.Len(t, exp.Lines, 16)
	assert.Equal(t, 1, exp.Skipped, "the nameless line")
	assert.Equal(t, map[string]int{"En attente": 1}, exp.UnknownSettled)
	assert.True(t, at(2, 4, 10, 0).Equal(exp.PeriodFrom), "%v", exp.PeriodFrom)
	assert.True(t, at(12, 8, 14, 5).Equal(exp.PeriodTo), "%v", exp.PeriodTo)
	assert.Equal(t, 3, exp.ToCheck(), "the « Non » lines")

	assert.Equal(t, MollieLine{
		Row: 2, NameKey: "bernard|hugo",
		Product: "Calendrier", Service: mollieOuting, Starts: at(15, 8, 0, 0),
		Amount: 4000, Settled: SettledYes, PaidAt: at(12, 8, 14, 5), Method: "Carte de crédit",
	}, mollieLineOfRow(t, exp, 2), "text dates read in Paris time")
	bank := mollieLineOfRow(t, exp, 5)
	assert.Equal(t, SettledNo, bank.Settled)
	assert.Equal(t, "Pay by Bank", bank.Method)
	assert.True(t, bank.Unsettled())
	assert.False(t, mollieLineOfRow(t, exp, 2).Unsettled())
	assert.Equal(t, Amount(-4000), mollieLineOfRow(t, exp, 7).Amount)
	assert.Equal(t, Amount(0), mollieLineOfRow(t, exp, 10).Amount)
	native := mollieLineOfRow(t, exp, 11)
	assert.Equal(t, at(7, 6, 12, 0), native.PaidAt, "native payment date")
	assert.Equal(t, at(12, 6, 0, 0), native.Starts, "native start date")
	assert.Equal(t, Amount(1250), native.Amount, "a text amount")
	assert.Equal(t, at(20, 7, 8, 0), mollieLineOfRow(t, exp, 12).PaidAt, "seconds accepted")
	assert.Equal(t, "martin|lea", mollieLineOfRow(t, exp, 13).NameKey)
	assert.Equal(t, "martin|lea", mollieLineOfRow(t, exp, 14).NameKey, "homonyms share the key")
	assert.Equal(t, "leroy|", mollieLineOfRow(t, exp, 18).NameKey)
	assert.Equal(t, "En attente", mollieLineOfRow(t, exp, 18).Settled, "an unknown value is kept as read")
	assert.Empty(t, mollieLineOfRow(t, exp, 4).Service, "no outing for a distance supplement")
}

func TestParseMollieNamesTheMissingColumn(t *testing.T) {
	rows, created := readFixture(t, "vpaydive_missing_column.xlsx")
	_, err := ParseMollie(rows, created, paris(t))
	pe := parseError(t, err)
	assert.Equal(t, ProblemMissingColumn, pe.Kind)
	assert.Equal(t, "Payé", pe.Column)

	payments, created := readFixture(t, "payments_valid.xlsx")
	_, err = ParseMollie(payments, created, paris(t))
	pe = parseError(t, err)
	assert.Equal(t, ProblemNoHeader, pe.Kind, "the payments export has no « Montant Panier »")
	assert.Contains(t, pe.Error(), `"Montant Panier"`)
}

func TestParseMollieRefusesUnreadableAmountsAndDatesWithTheirRows(t *testing.T) {
	ok := collected("Bernard", "Hugo", "Adhésion", 70.0, "02/04/2026 10:00")
	words := collected("Bernard", "Hugo", "Adhésion", "soixante-dix", "02/04/2026 10:00")
	excelError := collected("Bernard", "Hugo", "Adhésion", xlsxtest.Error("#VALUE!"), "02/04/2026 10:00")
	_, err := parseMollieSheet(t, ok, words, excelError)
	pe := parseError(t, err)
	assert.Equal(t, ProblemInvalidNumber, pe.Kind)
	assert.Equal(t, []int{3, 4}, pe.Rows)

	dayOnly := collected("Bernard", "Hugo", "Adhésion", 70.0, "02/04/2026")
	empty := collected("Bernard", "Hugo", "Adhésion", 70.0, nil)
	_, err = parseMollieSheet(t, ok, dayOnly, empty)
	pe = parseError(t, err)
	assert.Equal(t, ProblemInvalidDate, pe.Kind, "the payment date groups lines: it needs its minute")
	assert.Equal(t, []int{3, 4}, pe.Rows)
}

func TestParseMollieKeepsUnreadableStartDatesEmpty(t *testing.T) {
	l := hugoOuting("Calendrier", mollieOuting, "un jour", 40.0, "12/08/2026 14:05")
	exp, err := parseMollieSheet(t, l)
	require.NoError(t, err)
	assert.True(t, exp.Lines[0].Starts.IsZero())
}

func TestPaymentsExportToCheck(t *testing.T) {
	rows, created := readFixture(t, "payments_valid.xlsx")
	exp, err := Parse(rows, created, paris(t))
	require.NoError(t, err)
	assert.Equal(t, 1, exp.ToCheck(), "the « Paiement partiel » line")
	assert.True(t, Line{State: StatePartial}.Partial())
	assert.False(t, Line{State: StateDue}.Partial())
}
