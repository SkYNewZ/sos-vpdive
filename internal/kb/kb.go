// Package kb reads the knowledge base: one Markdown fiche per recurring
// problem in kb/, embedded in the binary (spec §5.1). Each fiche holds the
// answer shown to the member and the procedure for the resolver.
package kb

import (
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
)

// BlockKind is the kind of a block of text.
type BlockKind string

// Block kinds: the only Markdown the fiches use.
const (
	Paragraph BlockKind = "paragraph"
	Bullets   BlockKind = "bullets"
	Numbers   BlockKind = "numbers"
)

// Block is a paragraph (one item, its lines joined) or a list (one item per
// entry).
type Block struct {
	Kind  BlockKind
	Items []string
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
	Todo       int // [À COMPLÉTER : …] marks still to fill in
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
	return Fiche{
		ID: fm.ID, Title: fm.Title, Categories: fm.Categories, Links: fm.Links,
		AnswerText: sections[0], Answer: blocks(sections[0]), Procedure: blocks(sections[1]),
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
	return nil
}

// blocks splits a section into paragraphs and lists: a line starting "- "
// is a bullet, "1. " a numbered entry, a blank line ends a paragraph.
func blocks(text string) []Block {
	var out []Block
	open := false // the last block takes more lines
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
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
	return out
}
