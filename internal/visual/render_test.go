package visual

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderChartAndTable(t *testing.T) {
	b := Block{Title: "Sorties <club>", Source: "Calendrier", Period: "2026", Partial: true,
		Months:  []Month{{Label: "Janvier", Count: 0}, {Label: "Février", Count: 4}},
		Headers: []string{"Date", "Nom"}, Rows: [][]string{{"01/01", "A"}, {"02/01", "B"}, {"03/01", "C"}, {"04/01", "D"}, {"05/01", "E"}, {"06/01", "<img src=x>"}}}
	got, ok := Render("sorties", b)
	require.True(t, ok)
	for _, value := range []string{`class="viz-block`, "Sorties &lt;club&gt;", "Données partielles", `<title>Janvier : 0</title>`, `width="0"`, `<title>Février : 4</title>`, `width="100"`, `class="viz-details"`, `<caption>`, `scope="col"`, `data-label="Nom"`, "&lt;img src=x&gt;"} {
		assert.Contains(t, string(got), value)
	}
	assert.Equal(t, 6, strings.Count(string(got), "<tr role=\"row\"><td"))
	b.Months = nil
	got, ok = Render("sorties", b)
	require.True(t, ok)
	assert.Contains(t, string(got), "Aucune donnée mensuelle")
	b.Months = []Month{{Label: "Janvier", Count: 0}}
	got, ok = Render("sorties", b)
	require.True(t, ok)
	assert.NotContains(t, string(got), "Aucune donnée mensuelle")
	assert.NotContains(t, string(got), "NaN")
	assert.Contains(t, string(got), `width="0"`)
}

func TestDecodeRejectsUnsafeOrIncompleteBlocks(t *testing.T) {
	base := Block{Title: "Paiements", Source: "Import", Metrics: []Metric{{Label: "Réglé", Value: "25 €", State: "success"}}}
	for name, mutate := range map[string]func(*Block){
		"missing title":        func(b *Block) { b.Title = " " },
		"missing source":       func(b *Block) { b.Source = "" },
		"missing metric label": func(b *Block) { b.Metrics[0].Label = "" },
		"missing metric value": func(b *Block) { b.Metrics[0].Value = "" },
		"CSS injection":        func(b *Block) { b.Metrics[0].State = `success onclick=alert(1)` },
		"oversized text":       func(b *Block) { b.Title = strings.Repeat("x", 513) },
		"too many metrics":     func(b *Block) { b.Metrics = make([]Metric, 13) },
		"too many months":      func(b *Block) { b.Months = make([]Month, 25) },
		"negative month":       func(b *Block) { b.Period = "2026"; b.Months = []Month{{Label: "Jan", Count: -1}} },
		"huge month":           func(b *Block) { b.Period = "2026"; b.Months = []Month{{Label: "Jan", Count: 1000001}} },
		"missing month period": func(b *Block) { b.Months = []Month{{Label: "Jan", Count: 1}} },
		"short row":            func(b *Block) { b.Headers = []string{"A", "B"}; b.Rows = [][]string{{"a"}} },
		"long row":             func(b *Block) { b.Headers = []string{"A"}; b.Rows = [][]string{{"a", "b"}} },
		"too many rows":        func(b *Block) { b.Headers = []string{"A"}; b.Rows = make([][]string, 101) },
		"too many columns":     func(b *Block) { b.Headers = make([]string, 9) },
	} {
		t.Run(name, func(t *testing.T) {
			b := base
			b.Metrics = append([]Metric(nil), base.Metrics...)
			mutate(&b)
			data, err := json.Marshal(b)
			require.NoError(t, err)
			got, ok := Decode("cartes", data)
			assert.False(t, ok)
			assert.Empty(t, got)
		})
	}
	data, err := json.Marshal(base)
	require.NoError(t, err)
	for _, kind := range []string{"cartes", "paiements", "adhesion"} {
		got, ok := Decode(kind, data)
		require.True(t, ok, kind)
		assert.Contains(t, string(got), `class="viz-metric viz-success"`)
	}
	for _, value := range []string{string(data) + " {}", strings.Replace(string(data), `"titre"`, `"url":"https://evil.example","titre"`, 1), "{}", "null", strings.Repeat(" ", 65537)} {
		got, ok := Decode("cartes", []byte(value))
		assert.False(t, ok, value)
		assert.Empty(t, got)
	}
	for _, tc := range []struct {
		kind string
		b    Block
	}{{"inconnu", base}, {"sorties", base}, {"alerte", base}, {"cartes", Block{Title: "A", Source: "B"}}, {"adhesion", Block{Title: "A", Source: "B"}}, {"paiements", Block{Title: "A", Source: "B"}}} {
		_, ok := Render(tc.kind, tc.b)
		assert.False(t, ok, tc.kind)
	}
}

func TestRenderEscapesTextAndIsConcurrent(t *testing.T) {
	b := Block{Title: `<script>x</script>`, Source: `<a href=x>`, Note: `<img src=x> **note**`}
	want, ok := Render("alerte", b)
	require.True(t, ok)
	assert.Contains(t, string(want), "&lt;script&gt;x&lt;/script&gt;")
	assert.Contains(t, string(want), "&lt;img src=x&gt; **note**")
	assert.NotContains(t, string(want), "<script")
	assert.NotContains(t, string(want), "<img")
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			for range 10 {
				got, ok := Render("alerte", b)
				assert.True(t, ok)
				assert.Equal(t, want, got)
			}
		})
	}
	wg.Wait()
}
