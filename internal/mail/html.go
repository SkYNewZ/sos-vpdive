package mail

import (
	"fmt"
	"html/template"
	"regexp"
	"strings"
)

// urlPattern finds the links of a mail text: tracking and committee links.
var urlPattern = regexp.MustCompile(`https?://[^\s<>"]+`)

// htmlPart renders the HTML alternative of a text mail. Escaping is
// html/template's, so member-typed text in a mail never becomes markup.
var htmlPart = template.Must(template.New("mail").Parse(`<!doctype html>
<html lang="fr">
<head><meta charset="utf-8"></head>
<body>
{{range .}}<p>{{range $i, $line := .}}{{if $i}}<br>
{{end}}{{range $line}}{{if .URL}}<a href="{{.URL}}">{{.Text}}</a>{{else}}{{.Text}}{{end}}{{end}}{{end}}</p>
{{end}}</body>
</html>
`))

// segment is a run of text; a link when URL is set.
type segment struct {
	Text string
	URL  string
}

// renderHTML turns a text body into minimal HTML: paragraphs at blank lines,
// line breaks kept, http(s) URLs as links. No image, no style, no tracking.
func renderHTML(text string) (string, error) {
	var paragraphs [][][]segment
	for para := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		para = strings.Trim(para, "\n")
		if strings.TrimSpace(para) == "" {
			continue
		}
		var lines [][]segment
		for line := range strings.SplitSeq(para, "\n") {
			lines = append(lines, linkify(line))
		}
		paragraphs = append(paragraphs, lines)
	}
	var b strings.Builder
	if err := htmlPart.Execute(&b, paragraphs); err != nil {
		return "", fmt.Errorf("render html part: %w", err)
	}
	return b.String(), nil
}

// linkify splits a line into text and links. Punctuation that ends a
// sentence is left out of the link.
func linkify(line string) []segment {
	var out []segment
	last := 0
	for _, loc := range urlPattern.FindAllStringIndex(line, -1) {
		start := loc[0]
		end := start + len(strings.TrimRight(line[start:loc[1]], ".,;:!?)»"))
		out = append(out, segment{Text: line[last:start]}, segment{Text: line[start:end], URL: line[start:end]})
		last = end
	}
	return append(out, segment{Text: line[last:]})
}
