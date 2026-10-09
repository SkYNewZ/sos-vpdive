package assistant

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

// draftLanguage is the fence language of a draft reply to the member.
const draftLanguage = "brouillon"

// markdown renders CommonMark with GFM tables and strikethrough, line breaks
// kept. Raw HTML is dropped (no html.WithUnsafe), and safeNodes takes over
// links, images and fences. A table cell's alignment is an align attribute:
// the CSP (style-src 'self') refuses the default style attribute.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.NewTable(extension.WithTableCellAlignMethod(extension.TableCellAlignAttribute)),
		extension.Strikethrough),
	goldmark.WithRendererOptions(html.WithHardWraps(), renderer.WithNodeRenderers(util.Prioritized(safeNodes{}, 100))),
)

// Render turns the model's Markdown into the HTML a resolver reads. Links
// and autolinks show their text, images their alt text: a planted message
// cannot hand the resolver a link. A ```brouillon fence is the draft reply,
// labelled and copyable.
func Render(md string) template.HTML {
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(md), &buf); err != nil {
		return template.HTML("<p>" + template.HTMLEscapeString(md) + "</p>") //nolint:gosec // escaped above
	}
	return template.HTML(buf.String()) //nolint:gosec // goldmark escapes text and drops raw HTML
}

type safeNodes struct{}

func (safeNodes) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindLink, func(util.BufWriter, []byte, ast.Node, bool) (ast.WalkStatus, error) {
		return ast.WalkContinue, nil // the children, the link's text, render alone
	})
	reg.Register(ast.KindAutoLink, renderAutoLink)
	reg.Register(ast.KindImage, renderImage)
	reg.Register(ast.KindFencedCodeBlock, renderFence)
}

func renderAutoLink(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	link, ok := n.(*ast.AutoLink)
	if !entering || !ok {
		return ast.WalkContinue, nil
	}
	_, err := w.WriteString(template.HTMLEscapeString(string(link.Label(source))))
	return ast.WalkContinue, err
}

func renderImage(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	_, err := w.WriteString(template.HTMLEscapeString(plainText(n, source)))
	return ast.WalkSkipChildren, err
}

func renderFence(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	fence, ok := n.(*ast.FencedCodeBlock)
	if !entering || !ok {
		return ast.WalkContinue, nil
	}
	text := template.HTMLEscapeString(string(fence.Lines().Value(source)))
	out := "<pre><code>" + text + "</code></pre>"
	if string(fence.Language(source)) == draftLanguage {
		out = `<div class="draft" data-draft-reply><p class="draft-label">Brouillon automatique, à relire</p><pre>` + text +
			`</pre><button type="button" class="btn btn-sm btn-outline min-h-11" data-copy-draft>Copier</button></div>`
	}
	_, err := w.WriteString(out)
	return ast.WalkSkipChildren, err
}

// plainText is the text under n, such as an image's alt text.
func plainText(n ast.Node, source []byte) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(source))
		case *ast.String:
			b.Write(t.Value)
		default:
			b.WriteString(plainText(c, source))
		}
	}
	return b.String()
}
