// Package kb reads the knowledge base: one Markdown fiche per recurring
// problem in kb/, embedded in the binary (spec §5.1). Each fiche holds the
// answer shown to the member and the procedure for the resolver.
package kb

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
)

// Section headings of a fiche, in this order.
const (
	answerHeading    = "Réponse adhérent"
	procedureHeading = "Procédure résolveur"
	todoMark         = "[À COMPLÉTER"
)

var (
	idPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	numberedPattern = regexp.MustCompile(`^\d+\. `)
	separatorCell   = regexp.MustCompile(`^:?-+:?$`)
	accDescrPattern = regexp.MustCompile(`(?m)^\s*accDescr:\s*(.+)$`)
)

// BlockKind is the kind of a block of text.
type BlockKind string

// Block kinds: the only Markdown the fiches use.
const (
	Paragraph BlockKind = "paragraph"
	Bullets   BlockKind = "bullets"
	Numbers   BlockKind = "numbers"
	Table     BlockKind = "table"
	Diagram   BlockKind = "diagram"
)

// DiagramMark ends the SVG of a fiche's diagram: the SHA-256 of the Mermaid
// text it was drawn from (make diagrams). Load refuses a stale drawing.
const DiagramMark = "<!-- mermaid sha256:%x -->"

// Block is a paragraph (one item, its lines joined), a list (one item per
// entry), a table (Rows, the header first) or a Mermaid diagram (Source, its
// text alternative as the one item, and the URL of its drawing).
type Block struct {
	Kind   BlockKind
	Items  []string
	Rows   [][]string
	Source string // diagram: the Mermaid lines, each ending in "\n"
	Image  string // diagram: where the web package serves Fiche.Diagram
}

// Fiche is one recurring problem.
type Fiche struct {
	ID         string
	Title      string
	Categories []string // category ids
	Links      []string // VPDive link keys, shown first on the request page
	AnswerText string   // « Réponse adhérent » as written, sent to the model
	Answer     []Block
	Procedure  []Block
	Todo       int    // [À COMPLÉTER : …] marks still to fill in
	Diagram    []byte // the SVG of the fiche's diagram, if it has one
}

// Base holds every fiche, in file order.
type Base struct {
	Fiches []Fiche
}

// Load reads and checks kb/*.md. Any problem refuses the start, naming the
// file at fault; marks left to fill in are only warnings (see Warnings).
func Load(content fs.FS, knownCategory, knownLink func(string) bool) (*Base, error) {
	names, err := fs.Glob(content, "kb/*.md")
	if err != nil {
		return nil, fmt.Errorf("list fiches: %w", err)
	}
	b := &Base{}
	var errs []error
	for _, name := range names {
		data, err := fs.ReadFile(content, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		f, err := parse(string(data))
		if err == nil {
			err = f.check(knownCategory, knownLink)
		}
		if err == nil {
			f.Diagram, err = f.drawing(content)
		}
		if _, dup := b.Get(f.ID); err == nil && dup {
			err = fmt.Errorf("duplicate id %q", f.ID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		b.Fiches = append(b.Fiches, f)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return b, nil
}

// Get returns the fiche of id.
func (b *Base) Get(id string) (Fiche, bool) {
	if i := slices.IndexFunc(b.Fiches, func(f Fiche) bool { return f.ID == id }); i >= 0 {
		return b.Fiches[i], true
	}
	return Fiche{}, false
}

// Warnings names the fiches that still hold marks to fill in.
func (b *Base) Warnings() []string {
	var out []string
	for _, f := range b.Fiches {
		if f.Todo > 0 {
			out = append(out, fmt.Sprintf("kb/%s.md: %d %s] mark(s) to fill in", f.ID, f.Todo, todoMark))
		}
	}
	return out
}

// parse reads the front matter and the two sections of a fiche.
func parse(text string) (Fiche, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Fiche{}, errors.New("the file must start with a --- line")
	}
	front, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return Fiche{}, errors.New("the front matter has no closing --- line")
	}
	fm, err := readFrontMatter(front)
	if err != nil {
		return Fiche{}, err
	}
	sections, err := splitSections(body)
	if err != nil {
		return Fiche{}, err
	}
	answer, err := blocks(sections[0], fm.ID)
	if err != nil {
		return Fiche{}, fmt.Errorf("« %s »: %w", answerHeading, err)
	}
	procedure, err := blocks(sections[1], fm.ID)
	if err != nil {
		return Fiche{}, fmt.Errorf("« %s »: %w", procedureHeading, err)
	}
	return Fiche{
		ID: fm.ID, Title: fm.Title, Categories: fm.Categories, Links: fm.Links,
		AnswerText: sections[0], Answer: answer, Procedure: procedure,
		Todo: strings.Count(text, todoMark),
	}, nil
}

type frontMatter struct {
	ID, Title         string
	Categories, Links []string
}

// readFrontMatter reads "key: value" lines, each key once; a list is
// written in brackets, comma-separated. It is not YAML: a title may hold
// " : " (« Renouveler mon adhésion : les huit étapes »).
func readFrontMatter(front string) (frontMatter, error) {
	var fm frontMatter
	seen := map[string]bool{}
	for line := range strings.SplitSeq(front, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || seen[key] {
			return fm, fmt.Errorf("front matter line %q: expected key: value, each key once", line)
		}
		seen[key] = true
		var err error
		switch key {
		case "id":
			fm.ID = value
		case "titre":
			fm.Title = value
		case "categories":
			fm.Categories, err = list(value)
		case "liens_vpdive":
			fm.Links, err = list(value)
		default:
			err = errors.New("unknown key: id, titre, categories and liens_vpdive only")
		}
		if err != nil {
			return fm, fmt.Errorf("front matter %s: %w", key, err)
		}
	}
	return fm, nil
}

// list reads "[a, b]".
func list(value string) ([]string, error) {
	inner, opened := strings.CutPrefix(value, "[")
	inner, closed := strings.CutSuffix(inner, "]")
	if !opened || !closed {
		return nil, errors.New("must be a list in brackets, such as [carnet, paiement]")
	}
	var out []string
	for item := range strings.SplitSeq(inner, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out, nil
}

// splitSections returns the text of « Réponse adhérent » and « Procédure
// résolveur », in that order, each present once and not empty.
func splitSections(body string) ([2]string, error) {
	want := [2]string{answerHeading, procedureHeading}
	var (
		out   [2]string
		lines [2][]string
	)
	current := -1
	for line := range strings.SplitSeq(body, "\n") {
		heading, isHeading := strings.CutPrefix(line, "## ")
		switch {
		case isHeading:
			current++
			if current >= len(want) || strings.TrimSpace(heading) != want[current] {
				return out, fmt.Errorf("unexpected section %q: a fiche has « %s » then « %s »", strings.TrimSpace(heading), want[0], want[1])
			}
		case current < 0 && strings.TrimSpace(line) != "":
			return out, fmt.Errorf("text before « %s »", want[0])
		case current >= 0:
			lines[current] = append(lines[current], line)
		}
	}
	for i := range want {
		out[i] = strings.TrimSpace(strings.Join(lines[i], "\n"))
		if out[i] == "" {
			return out, fmt.Errorf("section « %s » is missing or empty", want[i])
		}
	}
	return out, nil
}

// check validates the front matter against the content files.
func (f Fiche) check(knownCategory, knownLink func(string) bool) error {
	if !idPattern.MatchString(f.ID) {
		return fmt.Errorf("id %q must match %s", f.ID, idPattern)
	}
	if f.Title == "" {
		return errors.New("empty titre")
	}
	if len(f.Categories) == 0 {
		return errors.New("no category")
	}
	for _, c := range f.Categories {
		if !knownCategory(c) {
			return fmt.Errorf("unknown category %q (config/categories.yaml)", c)
		}
	}
	for _, l := range f.Links {
		if !knownLink(l) {
			return fmt.Errorf("unknown VPDive link %q (config/vpdive.yaml)", l)
		}
	}
	if len(f.diagrams()) > 1 {
		return errors.New("a fiche holds one diagram at most")
	}
	return nil
}

// diagrams returns the fiche's diagram blocks.
func (f Fiche) diagrams() []Block {
	var out []Block
	for _, b := range slices.Concat(f.Answer, f.Procedure) {
		if b.Kind == Diagram {
			out = append(out, b)
		}
	}
	return out
}

// drawing reads kb/<id>.svg, the drawing of the fiche's diagram if it has
// one, and refuses it when it was drawn from another source or holds a
// script.
func (f Fiche) drawing(content fs.FS) ([]byte, error) {
	ds := f.diagrams()
	if len(ds) == 0 {
		return nil, nil
	}
	name := "kb/" + f.ID + ".svg"
	svg, err := fs.ReadFile(content, name)
	if err != nil {
		return nil, fmt.Errorf("diagram: %w (make diagrams)", err)
	}
	if !bytes.Contains(svg, fmt.Appendf(nil, DiagramMark, sha256.Sum256([]byte(ds[0].Source)))) {
		return nil, fmt.Errorf("%s is stale: run make diagrams", name)
	}
	if bytes.Contains(bytes.ToLower(svg), []byte("<script")) {
		return nil, fmt.Errorf("%s holds a script", name)
	}
	return svg, nil
}

// blocks splits a section into paragraphs, lists, tables and a diagram: a
// line starting "- " is a bullet, "1. " a numbered entry, "|" a table row
// (the "|---|" row is skipped), a blank line ends a paragraph, and the lines
// between "```mermaid" and "```" are a diagram, served at /kb/<id>.svg.
func blocks(text, id string) ([]Block, error) {
	var out []Block
	open := false                // the last block takes more lines
	var diagram *strings.Builder // the Mermaid lines, inside the fence
	for line := range strings.SplitSeq(text, "\n") {
		if diagram != nil {
			if strings.TrimSpace(line) != "```" {
				diagram.WriteString(line + "\n")
				continue
			}
			m := accDescrPattern.FindStringSubmatch(diagram.String())
			if m == nil {
				return nil, errors.New("the diagram has no accDescr: line, its text alternative")
			}
			out = append(out, Block{Kind: Diagram, Items: []string{strings.TrimSpace(m[1])}, Source: diagram.String(), Image: "/kb/" + id + ".svg"})
			diagram, open = nil, false
			continue
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "```mermaid":
			diagram = &strings.Builder{}
			continue
		case strings.HasPrefix(line, "```"):
			return nil, fmt.Errorf("fence %q: only ```mermaid is allowed", line)
		case strings.HasPrefix(line, "|"):
			cells := tableRow(line)
			if last := len(out) - 1; open && last >= 0 && out[last].Kind == Table {
				if want := len(out[last].Rows[0]); len(cells) != want {
					return nil, fmt.Errorf("table row %q: %d cells, the header has %d", line, len(cells), want)
				}
				if !slices.ContainsFunc(cells, func(c string) bool { return !separatorCell.MatchString(c) }) {
					continue // the |---| row
				}
				out[last].Rows = append(out[last].Rows, cells)
			} else {
				out = append(out, Block{Kind: Table, Rows: [][]string{cells}})
			}
			open = true
			continue
		}
		kind, item := Paragraph, line
		if rest, ok := strings.CutPrefix(line, "- "); ok {
			kind, item = Bullets, rest
		} else if loc := numberedPattern.FindStringIndex(line); loc != nil {
			kind, item = Numbers, line[loc[1]:]
		}
		switch {
		case line == "":
			open = false
		case open && kind == Paragraph && out[len(out)-1].Kind == Paragraph:
			last := &out[len(out)-1]
			last.Items[0] += " " + item
		case kind != Paragraph && len(out) > 0 && out[len(out)-1].Kind == kind:
			last := &out[len(out)-1]
			last.Items = append(last.Items, item)
			open = true
		default:
			out = append(out, Block{Kind: kind, Items: []string{item}})
			open = true
		}
	}
	if diagram != nil {
		return nil, errors.New("a ```mermaid fence is not closed")
	}
	return out, nil
}

// tableRow splits "| a | b |" into its trimmed cells.
func tableRow(line string) []string {
	cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}
