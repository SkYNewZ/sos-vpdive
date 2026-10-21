package assistant

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRender(t *testing.T) {
	cases := map[string]struct {
		in        string
		want, not []string
	}{
		"blocks and inlines": {
			in: "### Paiements\n\n- un\n- deux\n\n1. premier\n\n> cité\n\n`code` **gras** _ital_ ~~barré~~\n\n| a | b |\n|---|---|\n| 1 | 2 |",
			want: []string{"<h3>Paiements</h3>", "<li>un</li>", "<ol>", "<blockquote>", "<code>code</code>",
				"<strong>gras</strong>", "<em>ital</em>", "<del>barré</del>", "<table>", "<td>1</td>"},
		},
		"line breaks kept": {in: "ligne 1\nligne 2", want: []string{"ligne 1<br>"}},
		// The CSP (style-src 'self') refuses a style attribute: alignment is an align attribute.
		"aligned table": {in: "| Date | Montant |\n|:---|---:|\n| 02/10 | 45,00 € |",
			want: []string{`<th align="left">Date</th>`, "<td align=\"right\">45,00\u00a0€</td>"}, not: []string{"style="}},
		"unit glued": {in: "−25,00 € et 10 %, pas 5 ans", want: []string{"25,00\u00a0€", "10\u00a0%", "5 ans"}},
		"link":       {in: "[VPDive](https://evil.example/x)", want: []string{"VPDive"}, not: []string{"href", "evil"}},
		"autolink":   {in: "<https://evil.example/x>", want: []string{"https://evil.example/x"}, not: []string{"href"}},
		"image":      {in: "![carnet](https://evil.example/p.png)", want: []string{"carnet"}, not: []string{"<img", "evil"}},
		"raw html": {in: "<script>alert(1)</script>\n\nTexte <b onclick=x>gras</b>",
			want: []string{"Texte", "gras"}, not: []string{"<script", "onclick", "<b "}},
		"code": {in: "```\nx < y\n```", want: []string{"<pre><code>x &lt; y\n</code></pre>"}},
		"draft": {in: "```brouillon\nSalut Léa,\nton carnet <est> à jour.\n```",
			want: []string{`data-draft-reply`, "Brouillon automatique, à relire", "Salut Léa,\nton carnet &lt;est&gt; à jour.", "data-copy-draft"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := string(Render(tc.in))
			for _, w := range tc.want {
				assert.Contains(t, got, w)
			}
			for _, n := range tc.not {
				assert.NotContains(t, got, n)
			}
		})
	}
}

func TestRenderVisualBlocks(t *testing.T) {
	valid := `{"titre":"Cotisation","source":"Import club","indicateurs":[{"libelle":"Réglé","valeur":"25 €","etat":"success"}]}`
	for name, data := range map[string]string{
		"normal space":              valid,
		"literal nonbreaking space": strings.ReplaceAll(valid, "25 €", "25\u00a0€"),
		"escaped nonbreaking space": strings.ReplaceAll(valid, "25 €", `25\u00a0€`),
	} {
		t.Run(name, func(t *testing.T) {
			got := string(Render("Avant **gras**.\n\n```bloc:cartes\n" + data + "\n```\n\nAprès [lien](https://evil.example)."))
			assert.Contains(t, got, `class="viz-block`)
			assert.Contains(t, got, "<strong>gras</strong>")
			if name == "normal space" {
				assert.Contains(t, got, "25 €")
			} else {
				assert.Contains(t, got, "25\u00a0€")
			}
			assert.NotContains(t, got, "<pre>")
			assert.NotContains(t, got, "href=")
		})
	}
	for name, md := range map[string]string{
		"unknown kind":       "```bloc:inconnu\n" + valid + "\n```",
		"unknown field":      "```bloc:cartes\n" + strings.Replace(valid, `"titre"`, `"surprise":true,"titre"`, 1) + "\n```",
		"missing field":      "```bloc:cartes\n{}\n```",
		"invalid JSON":       "```bloc:cartes\n{\n```",
		"unterminated fence": "```bloc:cartes\n" + valid,
		"trailing JSON":      "```bloc:cartes\n" + valid + " {}\n```",
	} {
		t.Run(name, func(t *testing.T) {
			got := string(Render(md))
			assert.Contains(t, got, "<pre><code>")
			assert.NotContains(t, got, `class="viz-block`)
			if strings.Contains(md, "25 €") {
				assert.Contains(t, got, "25 €")
			}
		})
	}
	got := string(Render("```bloc:alerte\n" + `{"titre":"<script>bad</script>","source":"[x](https://evil.example)","note":"<img src=x> **text**"}` + "\n```"))
	assert.Contains(t, got, "&lt;script&gt;bad&lt;/script&gt;")
	assert.Contains(t, got, "&lt;img src=x&gt; **text**")
	assert.NotContains(t, got, "<img")
	assert.NotContains(t, got, "<script")
}

func TestRenderVisualFenceBoundaries(t *testing.T) {
	data := `{"titre":"Activité","source":"Calendrier","periode":"2026","mois":[{"libelle":"Janvier","valeur":0}]}`
	for _, md := range []string{
		"~~~~bloc:sorties\n" + data + "\n~~~~",
		"````bloc:sorties\n" + data + "\n````",
		"> ```bloc:sorties\n> " + data + "\n> ```",
		"- ```bloc:sorties\n  " + data + "\n  ```",
	} {
		got := string(Render(md))
		assert.Contains(t, got, `class="viz-block`, md)
	}
	md := "```bloc:sorties\n" + data + "\n```\n\n```brouillon\n25 € <relire>\n```\n\n```bloc:sorties\n" + data + "\n```"
	want := string(Render(md))
	assert.Equal(t, 2, strings.Count(want, `class="viz-block`))
	assert.Contains(t, want, "data-copy-draft")
	assert.Contains(t, want, "25 € &lt;relire&gt;")
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			for range 10 {
				assert.Equal(t, want, string(Render(md)))
			}
		})
	}
	wg.Wait()
}
