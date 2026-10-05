package tickets

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sosvpdive "github.com/SkYNewZ/sos-vpdive"
)

const testProducts = `products:
  - {id: carnet-10, label: Carnet de 10 plongées}
  - {id: licence, label: Licence fédérale}
`

const testCategories = `categories:
  - id: carnet
    label: Carnet
    help: Ton carnet est un avoir.
    fields:
      - {id: montant, label: Montant affiché, type: text}
      - {id: plongees, label: Plongées, type: number}
      - {id: sortie, label: Date de la sortie, type: date}
      - {id: details, label: Détails, type: textarea}
      - id: taille
        label: Taille du carnet
        type: choice
        options:
          - {id: "5", label: 5 plongées}
          - {id: "10", label: 10 plongées}
      - {id: produit, label: Produit, type: choice, options_from: products, required: true}
  - id: autre
    label: Autre
  - id: bug
    label: Bug VPDive
    committee_only: true
`

func testCatalog(t *testing.T, categories, products string) (*Catalog, error) {
	t.Helper()
	return LoadCatalog(fstest.MapFS{
		"config/categories.yaml": {Data: []byte(categories)},
		"config/products.yaml":   {Data: []byte(products)},
	})
}

func mustCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := testCatalog(t, testCategories, testProducts)
	require.NoError(t, err)
	return c
}

func TestLoadCatalogEmbedded(t *testing.T) {
	c, err := LoadCatalog(sosvpdive.Content)
	require.NoError(t, err)

	ids := make([]string, 0, len(c.Public()))
	for _, cat := range c.Public() {
		ids = append(ids, cat.ID)
	}
	assert.Equal(t, []string{"carnet", "remboursement", "inscription", "paiement", "compte", "adhesion", "autre"}, ids)

	bug, ok := c.Category("bug")
	require.True(t, ok)
	assert.True(t, bug.CommitteeOnly)
	assert.Empty(t, bug.Fields)

	carnet, _ := c.Category("carnet")
	assert.Contains(t, carnet.Help, "-180,00 €")

	refund, _ := c.Category("remboursement")
	require.NotEmpty(t, refund.Fields)
	assert.Equal(t, productsSource, refund.Fields[0].OptionsFrom)
	assert.True(t, refund.Fields[0].Required)
	assert.Len(t, refund.Fields[0].Options, len(c.Products))

	membership, _ := c.Category("adhesion")
	assert.Len(t, membership.Fields[0].Options, 8, "the 8 campaign steps of spec §9.7")
}

func TestLoadCatalogRefusesInvalidContent(t *testing.T) {
	tests := []struct {
		name, categories, want string
	}{
		{"duplicate category", "categories:\n  - {id: a, label: A}\n  - {id: a, label: B}\n", `duplicate id "a"`},
		{"duplicate field", "categories:\n  - id: a\n    label: A\n    fields:\n      - {id: x, label: X, type: text}\n      - {id: x, label: Y, type: text}\n", `duplicate id "x"`},
		{"unknown type", "categories:\n  - id: a\n    label: A\n    fields:\n      - {id: x, label: X, type: color}\n", `unknown type "color"`},
		{"choice without options", "categories:\n  - id: a\n    label: A\n    fields:\n      - {id: x, label: X, type: choice}\n", "either options or options_from"},
		{"choice with both", "categories:\n  - id: a\n    label: A\n    fields:\n      - id: x\n        label: X\n        type: choice\n        options_from: products\n        options: [{id: o, label: O}]\n", "either options or options_from"},
		{"unknown options source", "categories:\n  - id: a\n    label: A\n    fields:\n      - {id: x, label: X, type: choice, options_from: sorties}\n", "either options or options_from"},
		{"options on a text field", "categories:\n  - id: a\n    label: A\n    fields:\n      - {id: x, label: X, type: text, options_from: products}\n", "only a choice field takes options"},
		{"committee-only with fields", "categories:\n  - id: a\n    label: A\n  - id: b\n    label: B\n    committee_only: true\n    fields:\n      - {id: x, label: X, type: text}\n", "committee-only category has no fields"},
		{"unknown key", "categories:\n  - {id: a, label: A, couleur: bleu}\n", "couleur"},
		{"no public category", "categories:\n  - {id: a, label: A, committee_only: true}\n", "no category is offered"},
		{"empty label", "categories:\n  - {id: a, label: \" \"}\n", "empty label"},
		{"id with underscore", "categories:\n  - {id: mon_id, label: A}\n", "must match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := testCatalog(t, tt.categories, testProducts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Contains(t, err.Error(), "config/categories.yaml")
		})
	}

	_, err := testCatalog(t, testCategories, "products:\n  - {id: a, label: A}\n  - {id: a, label: B}\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config/products.yaml")
}

func TestReadFields(t *testing.T) {
	c := mustCatalog(t)
	form := map[string]string{
		FieldName("carnet", "montant"):  "  -180,00 €  ",
		FieldName("carnet", "plongees"): "007",
		FieldName("carnet", "sortie"):   "2026-09-14",
		FieldName("carnet", "details"):  "ligne 1\r\nligne 2",
		FieldName("carnet", "taille"):   "10",
		FieldName("carnet", "produit"):  "licence",
		FieldName("autre", "montant"):   "ignored: another category",
	}
	f, errs := c.ReadFields("carnet", func(name string) string { return form[name] })
	assert.Empty(t, errs)
	assert.Equal(t, Fields{Category: "carnet", Values: map[string]string{
		"montant": "-180,00 €", "plongees": "7", "sortie": "2026-09-14",
		"details": "ligne 1\nligne 2", "taille": "10", "produit": "licence",
	}}, f)
}

func TestReadFieldsRefusesBadValues(t *testing.T) {
	c := mustCatalog(t)
	form := map[string]string{
		FieldName("carnet", "montant"):  strings.Repeat("é", 201),
		FieldName("carnet", "plongees"): "12,5",
		FieldName("carnet", "sortie"):   "14/09/2026",
		FieldName("carnet", "details"):  strings.Repeat("a", 2001),
		FieldName("carnet", "taille"):   "7",
		// produit is required and missing.
	}
	f, errs := c.ReadFields("carnet", func(name string) string { return form[name] })
	assert.Empty(t, f.Values)
	assert.Equal(t, map[string]string{
		"montant":  "Une seule ligne de 200 caractères au plus.",
		"plongees": "Indique un nombre entier entre 0 et 9999.",
		"sortie":   "Indique une date valide.",
		"details":  "2000 caractères au plus.",
		"taille":   "Choisis une valeur de la liste.",
		"produit":  "Ce champ est obligatoire.",
	}, errs)

	// 200 accented characters fit: limits count runes, not bytes.
	form = map[string]string{FieldName("carnet", "montant"): strings.Repeat("é", 200), FieldName("carnet", "produit"): "licence"}
	_, errs = c.ReadFields("carnet", func(name string) string { return form[name] })
	assert.Empty(t, errs)
}

func TestDisplay(t *testing.T) {
	c := mustCatalog(t)
	got := c.Display(Fields{Category: "carnet", Values: map[string]string{
		"produit": "carnet-10", "sortie": "2026-09-14", "taille": "5",
	}})
	assert.Equal(t, []FieldValue{
		{Label: "Date de la sortie", Value: "14/09/2026"},
		{Label: "Taille du carnet", Value: "5 plongées"},
		{Label: "Produit", Value: "Carnet de 10 plongées"},
	}, got)
	assert.Equal(t, "Carnet de 10 plongées", c.Product(Fields{Category: "carnet", Values: map[string]string{"produit": "carnet-10"}}))
	assert.Empty(t, c.Product(Fields{Category: "autre"}))
}

// Review Focus 2: content removed from the YAML after a request was filed is
// shown as « retiré », never an error.
func TestDisplayMarksRemovedContent(t *testing.T) {
	c := mustCatalog(t)

	assert.Equal(t, "Carnet", c.CategoryLabel("carnet"))
	assert.Equal(t, "ancienne (retiré)", c.CategoryLabel("ancienne"))

	got := c.Display(Fields{Category: "carnet", Values: map[string]string{
		"taille":   "20",      // option removed
		"produit":  "bapteme", // product removed
		"couleur":  "bleu",    // field removed
		"plongees": "4",
	}})
	assert.Equal(t, []FieldValue{
		{Label: "Plongées", Value: "4"},
		{Label: "Taille du carnet", Value: "20", Removed: true},
		{Label: "Produit", Value: "bapteme", Removed: true},
		{Label: "couleur", Value: "bleu", Removed: true},
	}, got)
	assert.Empty(t, c.Product(Fields{Category: "carnet", Values: map[string]string{"produit": "bapteme"}}))

	got = c.Display(Fields{Category: "ancienne", Values: map[string]string{"x": "1"}})
	assert.Equal(t, []FieldValue{{Label: "x", Value: "1", Removed: true}}, got)
}

func TestFieldName(t *testing.T) {
	assert.Equal(t, "champ_carnet_montant", FieldName("carnet", "montant"))
}
