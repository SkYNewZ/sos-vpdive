package kb

import (
	"crypto/sha256"
	"fmt"
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
	got, err := blocks("Trois cas :\n- un ;\n- deux.\nEnsuite, écris-nous.", "x")
	require.NoError(t, err)
	assert.Equal(t, []Block{
		{Kind: Paragraph, Items: []string{"Trois cas :"}},
		{Kind: Bullets, Items: []string{"un ;", "deux."}},
		{Kind: Paragraph, Items: []string{"Ensuite, écris-nous."}},
	}, got)
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

const diagramSource = "flowchart TD\n  accTitle: Prix\n  accDescr: Le prix dépend de la carte.\n  A --> B\n"

func ficheWith(answer string) string {
	return "---\nid: tarif-test\ntitre: Tarifs\ncategories: [carnet]\nliens_vpdive: [paiements]\n---\n\n## Réponse adhérent\n\n" +
		answer + "\n\n## Procédure résolveur\n\n1. Vérifier.\n"
}

func svgFor(source string) string {
	return "<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>\n" + fmt.Sprintf(diagramMark, sha256.Sum256([]byte(source))) + "\n"
}

func TestLoadTableAndDiagram(t *testing.T) {
	answer := "Prix :\n\n| Cas | Prix |\n|---|---:|\n| Sans carte | 37 € |\n| Avec carte | 30 € |\n\n```mermaid\n" + diagramSource + "```  \n\nFin."
	for name, text := range map[string]string{"lf": ficheWith(answer), "crlf": strings.ReplaceAll(ficheWith(answer), "\n", "\r\n")} {
		b, err := load(map[string]string{"tarif-test.md": text, "tarif-test.svg": svgFor(diagramSource)})
		require.NoError(t, err, name)
		f, ok := b.Get("tarif-test")
		require.True(t, ok, name)
		assert.Equal(t, []Block{
			{Kind: Paragraph, Items: []string{"Prix :"}},
			{Kind: Table, Rows: [][]string{{"Cas", "Prix"}, {"Sans carte", "37 €"}, {"Avec carte", "30 €"}}},
			{Kind: Diagram, Items: []string{"Le prix dépend de la carte."}, Source: diagramSource, Image: "/kb/tarif-test.svg"},
			{Kind: Paragraph, Items: []string{"Fin."}},
		}, f.Answer, name)
		assert.Equal(t, svgFor(diagramSource), string(f.Diagram), name)
		assert.NotContains(t, f.AnswerText, "mermaid", name+": the model reads the tables, not the drawing")
		assert.Contains(t, f.AnswerText, "| Avec carte | 30 € |", name)
		assert.Equal(t, "1. Vérifier.", f.ProcedureText, name)
	}
}

func TestLoadRefusesBadTablesAndDiagrams(t *testing.T) {
	fence := "```mermaid\n" + diagramSource + "```"
	for _, tc := range []struct{ name, answer, svg, want string }{
		{"uneven row", "| a | b |\n|---|---|\n| 1 |", "", "1 cells, the header has 2"},
		{"other fence", "```go\nx\n```", "", "only ```mermaid"},
		{"unclosed fence", "```mermaid\nflowchart TD", "", "not closed"},
		{"no accDescr", "```mermaid\nflowchart TD\n  A --> B\n```", "", "no accDescr"},
		{"two diagrams", fence + "\n\n" + fence, svgFor(diagramSource), "one diagram at most"},
		{"svg missing", fence, "", "make diagrams"},
		{"svg stale", fence, svgFor("flowchart LR\n"), "is stale"},
	} {
		files := map[string]string{"tarif-test.md": ficheWith(tc.answer)}
		if tc.svg != "" {
			files["tarif-test.svg"] = tc.svg
		}
		_, err := load(files)
		require.Error(t, err, tc.name)
		assert.Contains(t, err.Error(), tc.want, tc.name)
		assert.Contains(t, err.Error(), "kb/tarif-test.md", tc.name)
	}
}
