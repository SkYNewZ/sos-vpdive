package assistant

import (
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
			want: []string{`<th align="left">Date</th>`, `<td align="right">45,00 €</td>`}, not: []string{"style="}},
		"link":     {in: "[VPDive](https://evil.example/x)", want: []string{"VPDive"}, not: []string{"href", "evil"}},
		"autolink": {in: "<https://evil.example/x>", want: []string{"https://evil.example/x"}, not: []string{"href"}},
		"image":    {in: "![carnet](https://evil.example/p.png)", want: []string{"carnet"}, not: []string{"<img", "evil"}},
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
