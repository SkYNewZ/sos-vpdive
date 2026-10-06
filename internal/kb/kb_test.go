package kb

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validFiche = `---
id: carnet-test
titre: Mon carnet : un montant négatif
categories: [carnet]
liens_vpdive: [paiements]
---

## Réponse adhérent

C'est normal,
ce n'est pas une dette.

Exemple : -180,00 €.

## Procédure résolveur

1. Ouvrir la page Paiements.
2. Déplier les lignes. [À COMPLÉTER : manipulation]

- une remarque
- une autre
`

func known(ids ...string) func(string) bool {
	return func(id string) bool { return slices.Contains(ids, id) }
}

func load(files map[string]string) (*Base, error) {
	fsys := fstest.MapFS{}
	for name, text := range files {
		fsys["kb/"+name] = &fstest.MapFile{Data: []byte(text)}
	}
	return Load(fsys, known("carnet", "adhesion"), known("paiements", "membres"))
}

func TestLoadValidFiche(t *testing.T) {
	b, err := load(map[string]string{"carnet-test.md": validFiche})
	require.NoError(t, err)
	require.Len(t, b.Fiches, 1)
	f, ok := b.Get("carnet-test")
	require.True(t, ok)
	assert.Equal(t, "Mon carnet : un montant négatif", f.Title, "a title may hold « : »")
	assert.Equal(t, []string{"carnet"}, f.Categories)
	assert.Equal(t, []string{"paiements"}, f.Links)
	assert.Equal(t, "C'est normal,\nce n'est pas une dette.\n\nExemple : -180,00 €.", f.AnswerText)
	assert.Equal(t, []Block{
		{Kind: Paragraph, Items: []string{"C'est normal, ce n'est pas une dette."}},
		{Kind: Paragraph, Items: []string{"Exemple : -180,00 €."}},
	}, f.Answer)
	assert.Equal(t, []Block{
		{Kind: Numbers, Items: []string{"Ouvrir la page Paiements.", "Déplier les lignes. [À COMPLÉTER : manipulation]"}},
		{Kind: Bullets, Items: []string{"une remarque", "une autre"}},
	}, f.Procedure)
	assert.Equal(t, 1, f.Todo)
	assert.Equal(t, []string{"kb/carnet-test.md: 1 [À COMPLÉTER] mark(s) to fill in"}, b.Warnings())
	_, ok = b.Get("absente")
	assert.False(t, ok)
}

func TestBlocksListAfterParagraph(t *testing.T) {
	assert.Equal(t, []Block{
		{Kind: Paragraph, Items: []string{"Trois cas :"}},
		{Kind: Bullets, Items: []string{"un ;", "deux."}},
		{Kind: Paragraph, Items: []string{"Ensuite, écris-nous."}},
	}, blocks("Trois cas :\n- un ;\n- deux.\nEnsuite, écris-nous."))
}

func TestLoadRefusesMalformedFiches(t *testing.T) {
	replace := func(old, repl string) string { return strings.Replace(validFiche, old, repl, 1) }
	tests := []struct {
		name, text, want string
	}{
		{"no front matter", strings.TrimPrefix(validFiche, "---\n"), "must start with a --- line"},
		{"unclosed front matter", replace("\n---\n\n##", "\n\n##"), "no closing --- line"},
		{"unknown key", replace("titre:", "auteur: x\ntitre:"), "front matter auteur: unknown key"},
		{"key twice", replace("titre:", "id: autre\ntitre:"), "each key once"},
		{"list without brackets", replace("categories: [carnet]", "categories: carnet"), "must be a list in brackets"},
		{"list half open", replace("categories: [carnet]", "categories: carnet]"), "must be a list in brackets"},
		{"bad id", replace("id: carnet-test", "id: Carnet_Test"), "must match"},
		{"empty title", replace("titre: Mon carnet : un montant négatif", "titre:"), "empty titre"},
		{"no category", replace("categories: [carnet]", "categories: []"), "no category"},
		{"unknown category", replace("categories: [carnet]", "categories: [plongee]"), `unknown category "plongee"`},
		{"unknown link", replace("liens_vpdive: [paiements]", "liens_vpdive: [banque]"), `unknown VPDive link "banque"`},
		{"missing section", replace("## Procédure résolveur", "Procédure résolveur"), "« Procédure résolveur » is missing"},
		{"empty section", replace("C'est normal,\nce n'est pas une dette.\n\nExemple : -180,00 €.", ""), "« Réponse adhérent » is missing or empty"},
		{"extra section", validFiche + "\n## Notes\n\nTexte.\n", `unexpected section "Notes"`},
		{"sections swapped", replace("## Réponse adhérent", "## Procédure résolveur"), "unexpected section"},
		{"text before", replace("\n## Réponse adhérent", "Intro.\n\n## Réponse adhérent"), "text before"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(map[string]string{"carnet-test.md": tt.text})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "kb/carnet-test.md")
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestLoadRefusesDuplicateIDs(t *testing.T) {
	_, err := load(map[string]string{"a.md": validFiche, "b.md": validFiche})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `kb/b.md: duplicate id "carnet-test"`)
}
