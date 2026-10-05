package members

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
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

func TestParseValidExport(t *testing.T) {
	exp, err := Parse(readFixture(t, "members_valid.xlsx"), paris(t))
	require.NoError(t, err)

	assert.Equal(t, time.Date(2026, 9, 1, 8, 15, 0, 0, paris(t)), exp.ExportedAt)
	assert.Equal(t, 1, exp.Skipped)
	assert.Equal(t, []Member{
		{Row: 5, LastName: "Martin", FirstName: "Léa", Email: "lea.martin@example.org", Seasons: new("2026"), LicenceExpires: "2026-12-31"},
		{Row: 6, LastName: "Bernard", FirstName: "Hugo", Email: "hugo.bernard@example.org", Seasons: new("2024, 2025, 2026"), LicenceExpires: "2026-12-31"},
		{Row: 7, LastName: "Petit", FirstName: "Chloé", Email: "chloe.petit@example.org", Seasons: new(""), LicenceExpires: ""},
		{Row: 9, LastName: "Durand", FirstName: "Noé", Email: "noe.durand@example.org", Seasons: new("2025, 2026"), LicenceExpires: ""},
		{Row: 10, LastName: "MARTIN", FirstName: "Lea", Email: "lea.martin2@example.org", Seasons: new("2025"), LicenceExpires: "2027-03-15"},
		{Row: 11, LastName: "Leroy", FirstName: "", Email: "ines.leroy@example.org", Seasons: new("2026"), LicenceExpires: "2027-06-30"},
	}, exp.Members)
}

func TestParseWithoutOptionalColumns(t *testing.T) {
	exp, err := Parse(readFixture(t, "members_minimal.xlsx"), paris(t))
	require.NoError(t, err)
	require.Len(t, exp.Members, 2)
	assert.Nil(t, exp.Members[0].Seasons)
	assert.Empty(t, exp.Members[0].LicenceExpires)
	assert.True(t, exp.ExportedAt.IsZero())
}

func TestParseMissingEmailColumnNamesIt(t *testing.T) {
	_, err := Parse(readFixture(t, "members_missing_email.xlsx"), paris(t))
	pe := parseError(t, err)
	assert.Equal(t, ProblemNoHeader, pe.Kind)
	assert.Contains(t, pe.Error(), `"Email"`)
}

func TestParseMissingFirstNameColumn(t *testing.T) {
	_, err := Parse(readFixture(t, "members_missing_first_name.xlsx"), paris(t))
	pe := parseError(t, err)
	assert.Equal(t, ProblemMissingColumn, pe.Kind)
	assert.Equal(t, ColumnFirstName, pe.Column)
}

func TestParseDuplicateEmailListsRows(t *testing.T) {
	_, err := Parse(readFixture(t, "members_duplicate_email.xlsx"), paris(t))
	pe := parseError(t, err)
	assert.Equal(t, ProblemDuplicateEmail, pe.Kind)
	assert.Equal(t, []int{5, 7}, pe.Rows)
}

func TestParseEmailWithSpaceListsRows(t *testing.T) {
	_, err := Parse(readFixture(t, "members_email_space.xlsx"), paris(t))
	pe := parseError(t, err)
	assert.Equal(t, ProblemInvalidEmail, pe.Kind)
	assert.Equal(t, []int{6}, pe.Rows)
}

// Review focus 1: header cells with trailing or non-breaking spaces and
// decomposed accents are still recognized.
func TestParseHeaderVariants(t *testing.T) {
	rows := []xlsx.Row{
		{Num: 1, Cells: []xlsx.Cell{
			{Col: 0, Kind: xlsx.KindString, Text: "Nom "},
			{Col: 1, Kind: xlsx.KindString, Text: "Prénom "},
			{Col: 2, Kind: xlsx.KindString, Text: " Email"},
		}},
		{Num: 2, Cells: []xlsx.Cell{
			{Col: 0, Kind: xlsx.KindString, Text: "Martin"},
			{Col: 1, Kind: xlsx.KindString, Text: "Léa"},
			{Col: 2, Kind: xlsx.KindString, Text: "lea.martin@example.org"},
		}},
	}
	exp, err := Parse(rows, paris(t))
	require.NoError(t, err)
	require.Len(t, exp.Members, 1)
	assert.Equal(t, "Léa", exp.Members[0].FirstName)
}

func TestParseHeaderBeyondTenthRowIsNotFound(t *testing.T) {
	rows := []xlsx.Row{{Num: 11, Cells: []xlsx.Cell{{Col: 0, Kind: xlsx.KindString, Text: "Email"}}}}
	_, err := Parse(rows, paris(t))
	assert.Equal(t, ProblemNoHeader, parseError(t, err).Kind)
}

func TestAmbiguousGroups(t *testing.T) {
	exp, err := Parse(readFixture(t, "members_valid.xlsx"), paris(t))
	require.NoError(t, err)
	groups, accounts := AmbiguousGroups(exp.Members)
	assert.Equal(t, 1, groups)
	assert.Equal(t, 2, accounts)
}
