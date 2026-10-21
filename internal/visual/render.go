// Package visual renders bounded, escaped visual blocks shared by committee pages.
package visual

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"
	"io"
	"strings"
)

// Block holds plain-text data for one committee visual.
type Block struct {
	Title   string     `json:"titre"`
	Source  string     `json:"source"`
	Period  string     `json:"periode"`
	Note    string     `json:"note,omitempty"`
	Partial bool       `json:"partiel,omitempty"`
	Metrics []Metric   `json:"indicateurs,omitempty"`
	Months  []Month    `json:"mois,omitempty"`
	Headers []string   `json:"colonnes,omitempty"`
	Rows    [][]string `json:"lignes,omitempty"`
}

// Metric pairs a readable label and value with an optional semantic state.
type Metric struct {
	Label string `json:"libelle"`
	Value string `json:"valeur"`
	State string `json:"etat,omitempty"`
}

// Month holds one labeled monthly count.
type Month struct {
	Label string `json:"libelle"`
	Count int    `json:"valeur"`
}

//go:embed block.html
var blockHTML string

var blockTemplate = template.Must(template.New("block").Funcs(template.FuncMap{
	"barWidth": func(count, maximum int) int { return count * 100 / maximum },
}).Parse(blockHTML))

// Render accepts only known block kinds and bounded plain-text content.
func Render(kind string, block Block) (template.HTML, bool) {
	if !valid(kind, block) {
		return "", false
	}
	maximum := 1
	for _, month := range block.Months {
		maximum = max(maximum, month.Count)
	}
	var out bytes.Buffer
	if err := blockTemplate.Execute(&out, struct {
		Block

		Kind    string
		Maximum int
	}{block, kind, maximum}); err != nil {
		return "", false
	}
	return template.HTML(out.String()), true //nolint:gosec // html/template escapes every model value
}

// Decode rejects unknown fields, trailing JSON, and oversized input.
func Decode(kind string, data []byte) (template.HTML, bool) {
	if len(data) > 64*1024 {
		return "", false
	}
	var block Block
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&block); err != nil {
		return "", false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", false
	}
	return Render(kind, block)
}

func valid(kind string, b Block) bool {
	switch kind {
	case "sorties":
		if !required(b.Period) {
			return false
		}
	case "cartes":
		if len(b.Metrics) == 0 {
			return false
		}
	case "paiements", "adhesion":
		if len(b.Metrics) == 0 && len(b.Rows) == 0 {
			return false
		}
	case "alerte":
		if !required(b.Note) {
			return false
		}
	default:
		return false
	}
	if !required(b.Title) || !required(b.Source) || len(b.Title) > 512 || len(b.Source) > 512 ||
		len(b.Period) > 512 || len(b.Note) > 2000 || len(b.Metrics) > 12 || len(b.Months) > 24 {
		return false
	}
	for _, metric := range b.Metrics {
		if !required(metric.Label) || !required(metric.Value) || len(metric.Label) > 512 || len(metric.Value) > 512 {
			return false
		}
		switch metric.State {
		case "", "info", "success", "warning", "error":
		default:
			return false
		}
	}
	for _, month := range b.Months {
		if !required(b.Period) || !required(month.Label) || len(month.Label) > 512 || month.Count < 0 || month.Count > 1_000_000 {
			return false
		}
	}
	return validTable(b.Headers, b.Rows)
}

func validTable(headers []string, rows [][]string) bool {
	if len(headers) > 8 || len(rows) > 100 {
		return false
	}
	for _, header := range headers {
		if !required(header) || len(header) > 512 {
			return false
		}
	}
	for _, row := range rows {
		if len(headers) == 0 || len(row) != len(headers) {
			return false
		}
		for _, cell := range row {
			if len(cell) > 512 {
				return false
			}
		}
	}
	return true
}

func required(value string) bool { return strings.TrimSpace(value) != "" }
