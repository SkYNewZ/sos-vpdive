package campaign

import (
	"flag"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

var update = flag.Bool("update", false, "rewrite testdata/fixtures from the definitions")

// Witness values of the columns the tool never reads (the CACI is health
// data): none may reach a stored person.
const (
	witnessAge       = "41"
	witnessSection   = "Section témoin"
	witnessCACI      = "CACI témoin"
	witnessExtension = "Attestation témoin"
	witnessLicence   = "A-00-000000"
	witnessCard      = "Carte témoin"
	witnessTraining  = "Formation témoin"
	witnessRules     = "Règlement lu témoin"
	witnessBlank     = "CACI vierge témoin"
)

// campaignHeader is the header of the campaign export, its 16 columns in
// order. « Licence » is the licence number, « License » the licence entries.
var campaignHeader = []any{
	"Statut", "Nom", "Prénom", "Âge", "Status de validation", "Section", "CACI", "Attestation prolongation",
	"Licence", "Carte", "Formation", "License", "Adhésion",
	"Règlement intérieur (merci de le télécharger pour en prendre connaissance)", "CACI vierge 2026", "Règlement",
}

// person is one row of the export by column name; the ignored columns always
// hold their witness values.
type person map[string]any

func (p person) row() []any {
	out := make([]any, len(campaignHeader))
	for i, h := range campaignHeader {
		out[i] = p[h.(string)]
	}
	out[3], out[5], out[6], out[7], out[8] = witnessAge, witnessSection, witnessCACI, witnessExtension, witnessLicence
	out[9], out[10], out[13], out[14] = witnessCard, witnessTraining, witnessRules, witnessBlank
	return out
}

// invited is a person of the campaign who has neither membership nor licence.
func invited(last, first string) person {
	return person{"Statut": StatusPending, "Nom": last, "Prénom": first}
}

// enrolled is a person with one membership and one licence entry.
func enrolled(last, first, membership, licence, settlement string) person {
	p := invited(last, first)
	p["Adhésion"], p["License"], p["Règlement"] = membership, licence, settlement
	return p
}

// campaignSheet lays the rows out as VPDive does: title, export date, a blank
// row, a row of sub-headers, then the header on row 5.
func campaignSheet(exported string, people ...person) xlsxtest.Sheet {
	s := make(xlsxtest.Sheet, 0, 5+len(people))
	s = append(s,
		[]any{"Campagne : Adhésion 2027 - du 01 septembre 2026 au 31 décembre 2026"},
		[]any{exported},
		nil,
		[]any{nil, nil, nil, nil, nil, nil, nil, nil, nil, "Recto / Verso", nil, nil, nil, "PDF"},
		campaignHeader,
	)
	for _, p := range people {
		s = append(s, p.row())
	}
	return s
}

const (
	fixtureExported = "Export effectué le 21/10/2026 à 09:15"
	paidMembership  = "Adhésion (Réglé) - 50,00 €"
	paidLicence     = "Licence fédérale (Réglé) - 42,00 €"
	cartLicence     = "Licence fédérale (Ajouté au panier mais pas encore payé) - 42,00 €"
)

// validPeople covers the traps of the campaign export with invented names.
// The members of members_valid.xlsx are Martin Léa and MARTIN Lea
// (homonyms), Bernard Hugo, Petit Chloé, Durand Noé and Leroy (no first
// name). Rows, from 6:
//
//	6  Bernard Hugo, both paid, licence by vpaydive: to validate
//	7  Petit Chloé, validated (« : » in the date) by Hugo Bernard, both paid by hand
//	8  Durand Noé, licence in the cart: incomplete
//	9  Martin Léa, validated (« / » in the date), two membership entries, one in an unknown state
//	10 MARTIN Lea, the homonym, invited only
//	11 Leroy, no first name, invited only
//	12-13 Inconnu Paul twice, no member: paid, then invited only
//	14 no name
//	15 Roux Max, no member: unreadable validation, an unreadable entry, a spaced amount
//	16 an invitation whose name is an address, no first name
func validPeople() []person {
	chloe := enrolled("Petit", "Chloé", "Adhésion (Réglé) - 50,00 € - Virements (03/10/2026)",
		"Licence fédérale (Réglé) - 42,00 € - Autre (04/10/2026)", "Tout réglé")
	chloe["Statut"], chloe["Status de validation"] = StatusValidated, "Adhesion confirmée le 14/10:2026 18:32 par Hugo Bernard"
	lea := enrolled("Martin", "Léa", "Adhésion (Réglé) - 25,00 €\nAdhésion (Remboursé) - 25,00 €", paidLicence, "Tout réglé")
	lea["Statut"], lea["Status de validation"] = StatusValidated, "Adhesion confirmée le 02/10/2026 08:05 par Hugo Bernard"
	roux := enrolled("Roux", "Max", "Adhésion réglée en espèces", "Licence fédérale (Réglé) - 1 042,50 €", "Non réglé")
	roux["Statut"], roux["Status de validation"] = StatusValidated, "Validé par le bureau"
	return []person{
		enrolled("Bernard", "Hugo", paidMembership, "Licence fédérale (Réglé) - 42,00 € - vpaydive (12/10/2026)", "Tout réglé"),
		chloe,
		enrolled("Durand", "Noé", paidMembership, cartLicence, "Non réglé"),
		lea,
		invited("MARTIN", "Lea"),
		invited("Leroy", ""),
		enrolled("Inconnu", "Paul", paidMembership, paidLicence, "Tout réglé"),
		invited("Inconnu", "Paul"),
		invited("", ""),
		roux,
		invited("invite@example.org", ""),
	}
}

func fixtures(tb testing.TB) map[string][]byte {
	tb.Helper()
	missing := campaignSheet(fixtureExported, enrolled("Bernard", "Hugo", paidMembership, paidLicence, "Tout réglé"))
	header := append([]any{}, campaignHeader...)
	header[11] = "Licence fédérale"
	missing[4] = header
	return map[string][]byte{
		"campaign_valid.xlsx":          xlsxtest.Build(tb, campaignSheet(fixtureExported, validPeople()...)),
		"campaign_missing_column.xlsx": xlsxtest.Build(tb, missing),
	}
}

// TestFixturesAreUpToDate keeps testdata/fixtures in sync with the definitions.
// Regenerate with: make fixtures.
func TestFixturesAreUpToDate(t *testing.T) {
	for name, want := range fixtures(t) {
		if *update {
			require.NoError(t, os.WriteFile(memberstest.FixturePath(name), want, 0o644))
			continue
		}
		got, err := os.ReadFile(memberstest.FixturePath(name))
		require.NoError(t, err, "regenerate with make fixtures")
		assert.Equal(t, want, got, name)
	}
}

// readFixture runs a fixture through the real reader.
func readFixture(t *testing.T, name string) []xlsx.Row {
	t.Helper()
	data, err := os.ReadFile(memberstest.FixturePath(name))
	require.NoError(t, err)
	rows, err := xlsx.ReadFirstSheet(data, imports.Limits())
	require.NoError(t, err)
	return rows
}

func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	return loc
}
