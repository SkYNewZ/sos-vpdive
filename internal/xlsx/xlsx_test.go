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
	data := xlsxtest.MustRaw(t,
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
	}}, rows[1])
	n, ok := rows[1].Cells[0].Number()
	assert.True(t, ok)
	assert.InDelta(t, 2026.0, n, 0)
}

func TestReadWithoutReferences(t *testing.T) {
	data := xlsxtest.MustRaw(t, `<row><c t="s"><v>0</v></c><c><v>5</v></c></row><row><c><v>1</v></c></row>`, `<si><t>a</t></si>`)
	rows, err := ReadFirstSheet(data, testLimits)
	require.NoError(t, err)
	assert.Equal(t, []Row{
		{Num: 1, Cells: []Cell{{Col: 0, Kind: KindString, Text: "a"}, {Col: 1, Kind: KindNumber, Text: "5"}}},
		{Num: 2, Cells: []Cell{{Col: 0, Kind: KindNumber, Text: "1"}}},
	}, rows)
}

func TestReadFirstSheetOnly(t *testing.T) {
	data := xlsxtest.MustBuild(t, xlsxtest.Sheet{{"first"}}, xlsxtest.Sheet{{"second"}})
	rows, err := ReadFirstSheet(data, testLimits)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "first", rows[0].Text(0))
}

func TestRowHelpers(t *testing.T) {
	r := Row{Num: 4, Cells: []Cell{{Col: 2, Kind: KindString, Text: "  Prénom  "}}}
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
		"shared index out of range": xlsxtest.MustRaw(t, `<row r="1"><c r="A1" t="s"><v>7</v></c></row>`),
		"bad column reference":      xlsxtest.MustRaw(t, `<row r="1"><c r="ZZZZ1"><v>1</v></c></row>`),
		"bad number":                xlsxtest.MustRaw(t, `<row r="1"><c r="A1"><v>abc</v></c></row>`),
		"truncated xml":             xlsxtest.MustRaw(t, `<row r="1"><c r="A1"><v>1</v>`),
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
	big := xlsxtest.MustBuild(t, sheet)
	require.Less(t, len(big), 200_000, "the bomb compresses well")
	_, err := ReadFirstSheet(big, Limits{MaxUncompressed: 1 << 20, MaxRows: 100_000, MaxCells: 100_000})
	require.ErrorIs(t, err, ErrTooLarge)

	five := xlsxtest.MustBuild(t, xlsxtest.Sheet{{1}, {2}, {3}, {4}, {5}})
	_, err = ReadFirstSheet(five, Limits{MaxUncompressed: 1 << 20, MaxRows: 3, MaxCells: 100})
	require.ErrorIs(t, err, ErrTooManyRows)
	_, err = ReadFirstSheet(five, Limits{MaxUncompressed: 1 << 20, MaxRows: 100, MaxCells: 4})
	require.ErrorIs(t, err, ErrTooManyCells)
}

func TestSerialDate(t *testing.T) {
	d, ok := SerialDate(46387)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), d)
	_, ok = SerialDate(0)
	assert.False(t, ok)
	_, ok = SerialDate(3_000_000)
	assert.False(t, ok)
}

func TestBuildIsDeterministic(t *testing.T) {
	s := xlsxtest.Sheet{{"a", 1, 2.5, true, xlsxtest.Inline("b")}, nil, {nil, "a"}}
	first, second := xlsxtest.MustBuild(t, s), xlsxtest.MustBuild(t, s)
	assert.Equal(t, first, second)
}

func TestReadBoundsTextAndSharedStrings(t *testing.T) {
	_, err := ReadFirstSheet(xlsxtest.MustRaw(t, `<row r="1"><c r="A1"><v>1</v></c></row>`,
		strings.Repeat(`<si/>`, 20)), Limits{MaxUncompressed: 1 << 20, MaxRows: 10, MaxCells: 10})
	require.ErrorIs(t, err, ErrTooManyCells)

	long := strings.Repeat("x", maxTextBytes+1)
	lim := Limits{MaxUncompressed: 1 << 20, MaxRows: 10, MaxCells: 10}
	_, err = ReadFirstSheet(xlsxtest.MustRaw(t, `<row r="1"><c r="A1" t="s"><v>0</v></c></row>`,
		`<si><t>`+long+`</t></si>`), lim)
	require.ErrorIs(t, err, ErrInvalid)

	_, err = ReadFirstSheet(xlsxtest.MustRaw(t, `<row r="1"><c r="A1" t="inlineStr"><is><t>`+long+`</t></is></c></row>`), lim)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestNonFiniteNumbers(t *testing.T) {
	_, ok := SerialDate(math.NaN())
	assert.False(t, ok)
	_, ok = SerialDate(math.Inf(1))
	assert.False(t, ok)
	_, err := ReadFirstSheet(xlsxtest.MustRaw(t, `<row r="1"><c r="A1"><v>NaN</v></c></row>`), testLimits)
	require.ErrorIs(t, err, ErrInvalid)
}
