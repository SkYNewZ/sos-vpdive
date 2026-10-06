package payments

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

var update = flag.Bool("update", false, "rewrite testdata/fixtures from the definitions")

// Witness values of ignored columns: none may ever appear in the database.
const (
	witnessComment   = "Commentaire privé du paiement"
	witnessAddress   = "12 rue des Oursins"
	witnessPostCode  = "83999"
	witnessCity      = "Villeneuve-sur-Mer"
	witnessEquipment = "Détendeur et gilet"
)

// fixtureCreated is the creation date the valid fixture declares.
var fixtureCreated = time.Date(2026, 9, 1, 12, 50, 27, 0, time.FixedZone("CEST", 2*3600))

// paymentsHeader is the header of the VPDive payments export: "Ville" and
// "Materiel" appear twice; the second "Materiel" is the rental amount.
var paymentsHeader = []any{
	"Nom", "Prénom", "Prix unitaire", "Quantité", "Montant paiement", "État", "Methode de paiement",
	"Type de produit", "Produit/Événement", "Du", "Au", "Ville", "Nbr.", "Activité", "Materiel",
	"Date paiement", "Commentaire", "Civilité", "Adresse", "Code postal", "Ville", "Évènement",
	"Materiel", "Créé le", "Montant réduc.",
}

// line is one row of the export, by column name; columns left out are empty.
type line map[string]any

func (l line) row() []any {
	out := make([]any, len(paymentsHeader))
	seen := map[string]int{}
	for i, h := range paymentsHeader {
		name := h.(string)
		key := name
		if seen[name] > 0 {
			key = name + "#2"
		}
		seen[name]++
		out[i] = l[key]
	}
	out[16], out[17], out[18], out[19], out[20] = witnessComment, "Mme", witnessAddress, witnessPostCode, witnessCity
	if _, ok := l["Materiel"]; !ok {
		out[14] = witnessEquipment
	}
	return out
}

// paid is a settled line of a member.
func paid(last, first string, price float64, method, product, created string) line {
	return line{"Nom": last, "Prénom": first, "Prix unitaire": price, "Quantité": 1, "Montant paiement": price,
		"État": "Payé", "Methode de paiement": method, "Produit/Événement": product, "Créé le": created, "Montant réduc.": 0}
}

// prepaid is a dive settled with a carnet: the price is in "Prix unitaire",
// the payment amount is 0.
func prepaid(last, first string, price float64, product string, starts any, created string) line {
	l := paid(last, first, price, "Prépayé", product, created)
	l["Montant paiement"], l["Du"] = 0, starts
	return l
}

// balance is the remaining credit of a carnet or a training: negative, due.
func balance(last, first string, amount float64, kind, product, created string) line {
	return line{"Nom": last, "Prénom": first, "Prix unitaire": amount, "Quantité": 1, "Montant paiement": 0,
		"État": "À payer", "Type de produit": kind, "Produit/Événement": product, "Créé le": created, "Montant réduc.": 0}
}

func sheet(lines ...line) xlsxtest.Sheet {
	s := make(xlsxtest.Sheet, 0, 1+len(lines))
	s = append(s, paymentsHeader)
	for _, l := range lines {
		s = append(s, l.row())
	}
	return s
}

const (
	cancelledCaps  = "SORTIE ANNULÉE - Île du Levant"
	cancelledLower = "Plongée de nuit (annulée, météo)"
	carnetTitle    = "Carte 10 plongées niveau 1 et 2"
)

// validLines covers the traps of spec §7.3 and §9.5 with invented names. The
// members of members_valid.xlsx are Martin Léa and MARTIN Lea (homonyms),
// Bernard Hugo, Petit Chloé, Durand Noé and Leroy (no first name).
func validLines() []line {
	purchase := paid("Bernard", "Hugo", 300, "vpaydive", carnetTitle, "05/01/2026 10:12:00")
	purchase["Type de produit"], purchase["Date paiement"] = "Carte", 46027.4375 // 05/01/2026 10:30
	toSettle := line{"Nom": "Bernard", "Prénom": "Hugo", "Prix unitaire": 35, "Quantité": 1, "Montant paiement": 0,
		"État": "À payer", "Produit/Événement": "Plongée Porquerolles", "Créé le": "03/06/2026 19:00:00", "Montant réduc.": 0}
	partial := line{"Nom": "Bernard", "Prénom": "Hugo", "Prix unitaire": 120, "Quantité": 1, "Montant paiement": 60,
		"État": "Paiement partiel", "Type de produit": "Formation", "Produit/Événement": "Formation RIFAP",
		"Créé le": "04/06/2026 08:00:00", "Montant réduc.": 10.5}
	refund := paid("Bernard", "Hugo", 35, "vpaydive", "Plongée Porquerolles", "06/06/2026 09:00:00")
	refund["Montant paiement"] = -35
	unknownState := paid("Bernard", "Hugo", 30, "Prépayé", "Sortie Cap Garonne", "07/06/2026 09:00:00")
	unknownState["État"] = "En attente"
	cancelledLine := prepaid("Bernard", "Hugo", 30, "Sortie Cap Garonne", 46180.375, "08/06/2026 09:00:00")
	cancelledLine["État"] = "Annulé"
	rental := paid("Bernard", "Hugo", 30, "Prépayé", "Sortie Sec de la Croix", "09/06/2026 09:00:00")
	rental["Materiel"], rental["Materiel#2"] = "Bouteille 12 L", 8
	byVPayDive := paid("Bernard", "Hugo", 35, "vpaydive", cancelledLower, "02/05/2026 18:30:00")
	byVPayDive["Du"] = 46150.833333333336 // 08/05/2026 20:00
	nativeCreated := paid("Petit", "Chloé", 25, "vpaydive", "Location de matériel", "")
	nativeCreated["Créé le"] = 46174.5 // 01/06/2026 12:00
	textAmount := paid("Petit", "Chloé", 0, "Espèces", "", "10/06/2026 10:00:00")
	textAmount["Prix unitaire"], textAmount["Montant paiement"], textAmount["Montant réduc."] = "12,50", " 1 012,50 ", "0"
	nameless := paid("", "", 30, "vpaydive", "Plongée Porquerolles", "11/06/2026 10:00:00")

	return []line{
		purchase,
		balance("Bernard", "Hugo", -180, "Carte", carnetTitle, "05/01/2026 10:12:00"),
		balance("Bernard", "Hugo", -30, "Carte", carnetTitle, "12/03/2026 18:00:00"),
		balance("Bernard", "Hugo", -60, "Formation", "Formation N2", "02/02/2026 09:00:00"),
		prepaid("Bernard", "Hugo", 30, cancelledCaps, "10/05/2026 09:00", "01/05/2026 12:00:00"),
		byVPayDive,
		toSettle,
		partial,
		refund,
		unknownState,
		cancelledLine,
		rental,
		prepaid("Durand", "Noé", 14, cancelledCaps, "10/05/2026 09:00", "01/05/2026 12:05:00"),
		prepaid("Durand", "Noé", 16, cancelledCaps, "10/05/2026 09:00", "01/05/2026 12:05:00"),
		prepaid("MARTIN", "Lea", 30, cancelledCaps, "10/05/2026 09:00", "01/05/2026 12:10:00"),
		balance("Martin", "Léa", -90, "Carte", carnetTitle, "15/02/2026 11:00:00"),
		paid("Leroy", "", 30, "Prépayé", "Sortie Cap Garonne", "07/06/2026 10:00:00"),
		paid("Inconnu", "Paul", 25, "vpaydive", "Baptême", "20/06/2026 15:00:00"),
		nativeCreated,
		textAmount,
		nameless,
	}
}

func fixtures(tb testing.TB) map[string][]byte {
	tb.Helper()
	return map[string][]byte{
		"payments_valid.xlsx": xlsxtest.BuildCreated(tb, fixtureCreated, sheet(validLines()...)),
		"payments_missing_column.xlsx": func() []byte {
			s := sheet(paid("Bernard", "Hugo", 30, "vpaydive", "Baptême", "05/01/2026 10:12:00"))
			header := append([]any{}, paymentsHeader...)
			header[4] = "Montant"
			s[0] = header
			return xlsxtest.Build(tb, s)
		}(),
	}
}

func fixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "fixtures", name)
}

// TestFixturesAreUpToDate keeps testdata/fixtures in sync with the definitions.
// Regenerate with: make fixtures.
func TestFixturesAreUpToDate(t *testing.T) {
	for name, want := range fixtures(t) {
		if *update {
			require.NoError(t, os.WriteFile(fixturePath(name), want, 0o644))
			continue
		}
		got, err := os.ReadFile(fixturePath(name))
		require.NoError(t, err, "regenerate with make fixtures")
		assert.Equal(t, want, got, name)
	}
}

// readFixture runs a fixture through the real reader.
func readFixture(t *testing.T, name string) ([]xlsx.Row, time.Time) {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err)
	rows, err := xlsx.ReadFirstSheet(data, ImportLimits())
	require.NoError(t, err)
	created, _ := xlsx.Created(data, ImportLimits())
	return rows, created
}
