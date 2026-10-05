package members

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

var update = flag.Bool("update", false, "rewrite testdata/fixtures from the definitions")

// Witness values of ignored columns: none may ever appear in the database.
const (
	witnessBirthDate = "14/07/1980"
	witnessAddress   = "12 rue des Oursins"
	witnessPhone     = "0600000000"
	witnessContact   = "Gaspard Urgence"
	witnessComment   = "Commentaire privé"
	witnessCACI      = "CACI valide jusqu'au 01/02/2027"
)

var fullHeader = []any{
	"n°", "Photo", "Licence", "Expire le", "Civilité", "Nom", "Prénom", "Nom de naissance",
	"Date de naissance", "Email", "Adresse", "CP", "Ville", "CACI", "Téléphone fixe", "Portable",
	"Nom", "Prénom", "Lien de parenté", "Téléphone fixe", "Portable", "Commentaire", "Année(s)",
}

// memberRow fills a row of fullHeader. The second "Nom"/"Prénom" pair is the
// emergency contact and must never be read as the member's name.
func memberRow(n int, expires any, last string, first, email, seasons any) []any {
	row := make([]any, len(fullHeader))
	row[0], row[3], row[4] = n, expires, "Mme"
	row[5], row[6], row[8], row[9] = last, first, witnessBirthDate, email
	row[10], row[11], row[12], row[13] = witnessAddress, "83000", "Villeneuve", witnessCACI
	row[15], row[16], row[17], row[18] = witnessPhone, witnessContact, "Gaspard", "Conjoint"
	row[20], row[21], row[22] = witnessPhone, witnessComment, seasons
	return row
}

func exportSheet(rows ...[]any) xlsxtest.Sheet {
	return append(xlsxtest.Sheet{
		{"Liste des membres Club de plongée d'exemple"},
		{"Export effectué le 01/09/2026 à 08:15"},
		nil, // row 3 is absent, as in VPDive exports
		fullHeader,
	}, rows...)
}

func fixtures() map[string]xlsxtest.Sheet {
	return map[string]xlsxtest.Sheet{
		"members_valid.xlsx": exportSheet(
			memberRow(1, "31/12/2026", "Martin", "Léa", "  Lea.Martin@Example.ORG ", 2026),
			memberRow(2, 46387.0, "Bernard", "Hugo", "hugo.bernard@example.org", "2024, 2025, 2026"),
			memberRow(3, nil, "Petit", "Chloé", "chloe.petit@example.org", nil),
			[]any{4}, // a row without email in the middle of the file
			memberRow(5, "bientôt", "Durand", "Noé", "noe.durand@example.org", " 2025 , 2026 "),
			memberRow(6, "15/03/2027", "MARTIN", "Lea", "lea.martin2@example.org", 2025),
			memberRow(7, "30/06/2027", "Leroy", nil, "ines.leroy@example.org", "2026"),
		),
		"members_missing_email.xlsx": func() xlsxtest.Sheet {
			s := exportSheet(memberRow(1, "31/12/2026", "Martin", "Léa", "lea.martin@example.org", 2026))
			header := append([]any{}, fullHeader...)
			header[9] = "Courriel"
			s[3] = header
			return s
		}(),
		"members_missing_first_name.xlsx": {
			{"Nom", "Email"},
			{"Martin", "lea.martin@example.org"},
		},
		"members_duplicate_email.xlsx": exportSheet(
			memberRow(1, nil, "Martin", "Léa", "dup@example.org", 2026),
			memberRow(2, nil, "Bernard", "Hugo", "hugo.bernard@example.org", 2026),
			memberRow(3, nil, "Petit", "Chloé", " DUP@example.org", 2026),
		),
		"members_email_space.xlsx": exportSheet(
			memberRow(1, nil, "Martin", "Léa", "lea.martin@example.org", 2026),
			memberRow(2, nil, "Dupont", "Jean", "jean dupont@example.org", 2026),
		),
		"members_minimal.xlsx": {
			{"Nom", "Prénom", "Email"},
			{"Martin", "Léa", "lea.martin@example.org"},
			{"Bernard", "Hugo", "hugo.bernard@example.org"},
		},
	}
}

func fixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "fixtures", name)
}

// TestFixturesAreUpToDate keeps testdata/fixtures in sync with the definitions.
// Regenerate with: go test ./internal/members -run TestFixturesAreUpToDate -update.
func TestFixturesAreUpToDate(t *testing.T) {
	for name, sheet := range fixtures() {
		want := xlsxtest.Build(t, sheet)
		if *update {
			require.NoError(t, os.MkdirAll(filepath.Dir(fixturePath(name)), 0o755))
			require.NoError(t, os.WriteFile(fixturePath(name), want, 0o644))
			continue
		}
		got, err := os.ReadFile(fixturePath(name))
		require.NoError(t, err, "regenerate with -update")
		assert.Equal(t, want, got, name)
	}
}

// readFixture runs a fixture through the real reader.
func readFixture(t *testing.T, name string) []xlsx.Row {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err)
	rows, err := xlsx.ReadFirstSheet(data, ImportLimits())
	require.NoError(t, err)
	return rows
}
