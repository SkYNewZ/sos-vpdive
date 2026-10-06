package xlsx

import (
	"archive/zip"
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/xlsx/xlsxtest"
)

var testLimits = Limits{MaxUncompressed: 1 << 20, MaxRows: 100, MaxCells: 1000}

func TestReadCellKindsAndGaps(t *testing.T) {
	data := xlsxtest.Raw(t,
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c><c r="D1" t="s"><v>2</v></c></row>`+
			`<row r="3"><c r="A3"><v>2026</v></c><c r="B3" t="b"><v>1</v></c>`+
			`<c r="C3" t="inlineStr"><is><t>en ligne</t></is></c><c r="D3" t="str"><v>formule</v></c>`+
			`<c r="E3" t="e"><v>#N/A</v></c><c r="F3" s="2"/></row>`,
		`<si><t>Email</t></si>`,
		`<si><r><t>Pré</t></r><r><t>nom</t></r><rPh sb="0" eb="1"><t>ignoré</t></rPh></si>`,
		`<si><t></t></si>`,
	)

	rows, err := ReadFirstSheet(data, testLimits)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, Row{Num: 1, Cells: []Cell{
		{Col: 0, Kind: KindString, Text: "Email"},
		{Col: 2, Kind: KindString, Text: "Prénom"},
	}}, rows[0])
	assert.Equal(t, Row{Num: 3, Cells: []Cell{
		{Col: 0, Kind: KindNumber, Text: "2026"},
		{Col: 1, Kind: KindBool, Text: "1"},
		{Col: 2, Kind: KindString, Text: "en ligne"},
		{Col: 3, Kind: KindString, Text: "formule"},
		{Col: 4, Kind: KindError},
	}}, rows[1], "an error value is kept, so a reader can refuse it, but holds no text")
	n, ok := rows[1].Cells[0].Number()
	assert.True(t, ok)
	assert.InDelta(t, 2026.0, n, 0)
}

func TestReadWithoutReferences(t *testing.T) {
	data := xlsxtest.Raw(t, `<row><c t="s"><v>0</v></c><c><v>5</v></c></row><row><c><v>1</v></c></row>`, `<si><t>a</t></si>`)
	rows, err := ReadFirstSheet(data, testLimits)
	require.NoError(t, err)
	assert.Equal(t, []Row{
		{Num: 1, Cells: []Cell{{Col: 0, Kind: KindString, Text: "a"}, {Col: 1, Kind: KindNumber, Text: "5"}}},
		{Num: 2, Cells: []Cell{{Col: 0, Kind: KindNumber, Text: "1"}}},
	}, rows)
}

func TestReadFirstSheetOnly(t *testing.T) {
	data := xlsxtest.Build(t, xlsxtest.Sheet{{"first"}}, xlsxtest.Sheet{{"second"}})
	rows, err := ReadFirstSheet(data, testLimits)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "first", rows[0].Text(0))
}

func TestRowHelpers(t *testing.T) {
	r := Row{Num: 4, Cells: []Cell{{Col: 2, Kind: KindString, Text: "\u00a0\u00a0Prénom\u00a0\u00a0"}}}
	assert.Equal(t, "Prénom", r.Text(2))
	assert.Empty(t, r.Text(0))
	_, ok := r.Cell(0)
	assert.False(t, ok)
}

func TestReadRejectsInvalidInput(t *testing.T) {
	var noWorkbook bytes.Buffer
	zw := zip.NewWriter(&noWorkbook)
	w, err := zw.Create("hello.txt")
	require.NoError(t, err)
	_, err = w.Write([]byte("hi"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	cases := map[string][]byte{
		"not a zip":                 []byte("%PDF-1.7 not a workbook"),
		"zip without workbook":      noWorkbook.Bytes(),
		"shared index out of range": xlsxtest.Raw(t, `<row r="1"><c r="A1" t="s"><v>7</v></c></row>`),
		"bad column reference":      xlsxtest.Raw(t, `<row r="1"><c r="ZZZZ1"><v>1</v></c></row>`),
		"bad number":                xlsxtest.Raw(t, `<row r="1"><c r="A1"><v>abc</v></c></row>`),
		"truncated xml":             xlsxtest.Raw(t, `<row r="1"><c r="A1"><v>1</v>`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ReadFirstSheet(data, testLimits)
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestReadEnforcesLimits(t *testing.T) {
	sheet := make(xlsxtest.Sheet, 0, 30000)
	for range 30000 {
		sheet = append(sheet, []any{strings.Repeat("x", 30)})
	}
	big := xlsxtest.Build(t, sheet)
	require.Less(t, len(big), 200_000, "the bomb compresses well")
	_, err := ReadFirstSheet(big, Limits{MaxUncompressed: 1 << 20, MaxRows: 100_000, MaxCells: 100_000})
	require.ErrorIs(t, err, ErrTooLarge)

	five := xlsxtest.Build(t, xlsxtest.Sheet{{1}, {2}, {3}, {4}, {5}})
	_, err = ReadFirstSheet(five, Limits{MaxUncompressed: 1 << 20, MaxRows: 3, MaxCells: 100})
	require.ErrorIs(t, err, ErrTooManyRows)
	_, err = ReadFirstSheet(five, Limits{MaxUncompressed: 1 << 20, MaxRows: 100, MaxCells: 4})
	require.ErrorIs(t, err, ErrTooManyCells)
}

func TestSerialDate(t *testing.T) {
	d, ok := SerialDate(46387)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), d)
	d, ok = SerialDate(46387.375)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 12, 31, 9, 0, 0, 0, time.UTC), d, "the fraction is the time of day")
	d, ok = SerialDate(46387.416666666664)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 12, 31, 10, 0, 0, 0, time.UTC), d, "rounded to the second")
	d, ok = SerialDate(46387.999999999)
	require.True(t, ok)
	assert.Equal(t, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), d, "a rounded midnight moves to the next day")
	d, ok = SerialDate(2958465.5)
	require.True(t, ok)
	assert.Equal(t, time.Date(9999, 12, 31, 12, 0, 0, 0, time.UTC), d, "no overflow on the last day")
	_, ok = SerialDate(0)
	assert.False(t, ok)
	_, ok = SerialDate(3_000_000)
	assert.False(t, ok)
}

func TestBuildIsDeterministic(t *testing.T) {
	s := xlsxtest.Sheet{{"a", 1, 2.5, true, xlsxtest.Inline("b")}, nil, {nil, "a"}}
	first, second := xlsxtest.Build(t, s), xlsxtest.Build(t, s)
	assert.Equal(t, first, second)
}

func TestReadBoundsTextAndSharedStrings(t *testing.T) {
	_, err := ReadFirstSheet(xlsxtest.Raw(t, `<row r="1"><c r="A1"><v>1</v></c></row>`,
		strings.Repeat(`<si/>`, 20)), Limits{MaxUncompressed: 1 << 20, MaxRows: 10, MaxCells: 10})
	require.ErrorIs(t, err, ErrTooManyCells)

	long := strings.Repeat("x", maxTextBytes+1)
	lim := Limits{MaxUncompressed: 1 << 20, MaxRows: 10, MaxCells: 10}
	_, err = ReadFirstSheet(xlsxtest.Raw(t, `<row r="1"><c r="A1" t="s"><v>0</v></c></row>`,
		`<si><t>`+long+`</t></si>`), lim)
	require.ErrorIs(t, err, ErrInvalid)

	_, err = ReadFirstSheet(xlsxtest.Raw(t, `<row r="1"><c r="A1" t="inlineStr"><is><t>`+long+`</t></is></c></row>`), lim)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestNonFiniteNumbers(t *testing.T) {
	_, ok := SerialDate(math.NaN())
	assert.False(t, ok)
	_, ok = SerialDate(math.Inf(1))
	assert.False(t, ok)
	_, err := ReadFirstSheet(xlsxtest.Raw(t, `<row r="1"><c r="A1"><v>NaN</v></c></row>`), testLimits)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestCreated(t *testing.T) {
	created := time.Date(2026, 9, 1, 10, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	got, ok := Created(xlsxtest.BuildCreated(t, created, xlsxtest.Sheet{{"a"}}), testLimits)
	require.True(t, ok)
	assert.True(t, created.Equal(got), "got %v", got)

	_, ok = Created(xlsxtest.Build(t, xlsxtest.Sheet{{"a"}}), testLimits)
	assert.False(t, ok, "no core properties")
	_, ok = Created([]byte("not a zip"), testLimits)
	assert.False(t, ok)
	_, ok = Created(xlsxtest.BuildCreated(t, created, xlsxtest.Sheet{{"a"}}), Limits{MaxUncompressed: 10, MaxRows: 1, MaxCells: 1})
	assert.False(t, ok, "the size budget applies")
}

func TestFindHeader(t *testing.T) {
	rows := []Row{
		{Num: 1, Cells: []Cell{{Col: 0, Kind: KindString, Text: "Liste des paiements"}}},
		{Num: 3, Cells: []Cell{
			{Col: 0, Kind: KindString, Text: " Nom "},
			{Col: 1, Kind: KindString, Text: "Pre\u0301nom"},
			{Col: 2, Kind: KindString, Text: "Materiel"},
			{Col: 4, Kind: KindString, Text: "Materiel"},
			{Col: 5, Kind: KindString, Text: "Créé le"},
		}},
		{Num: 4, Cells: []Cell{{Col: 0, Kind: KindString, Text: "Créé le"}}},
	}
	i, h, ok := FindHeader(rows, "Créé le", 10)
	require.True(t, ok)
	assert.Equal(t, 1, i, "index in rows, not the row number")
	col, ok := h.Col("Nom", 0)
	assert.True(t, ok)
	assert.Equal(t, 0, col, "names are trimmed")
	col, ok = h.Col("Prénom", 0)
	assert.True(t, ok)
	assert.Equal(t, 1, col, "names are compared in NFC")
	col, ok = h.Col("Materiel", 1)
	assert.True(t, ok)
	assert.Equal(t, 4, col, "the second occurrence")
	_, ok = h.Col("Materiel", 2)
	assert.False(t, ok)
	_, ok = h.Col("Email", 0)
	assert.False(t, ok)

	_, _, ok = FindHeader(rows, "Créé le", 2)
	assert.False(t, ok, "only rows numbered up to maxRow are searched")
}
