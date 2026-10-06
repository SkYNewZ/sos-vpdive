// Package xlsx reads the first sheet of an .xlsx workbook held in memory, and
// its creation date. It streams the sheet XML, enforces size, row and cell
// limits while reading, and never writes to disk (spec §7.2). It reads values
// only: no styles, no formulas, no dates formatting.
package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// Reader errors.
var (
	ErrInvalid      = errors.New("not a readable .xlsx workbook")
	ErrTooLarge     = errors.New("workbook too large once decompressed")
	ErrTooManyRows  = errors.New("workbook has too many rows")
	ErrTooManyCells = errors.New("workbook has too many cells")
)

// Kind is the type of a cell value.
type Kind uint8

// Cell kinds. Empty cells are never returned.
const (
	KindString Kind = iota + 1
	KindNumber
	KindBool
	KindError // an error value such as #N/A, with no text: it reads as empty
)

// tagPhonetic is the phonetic-run element, whose text is never read.
const tagPhonetic = "rPh"

// maxTextBytes bounds one cell text: Excel allows 32 767 characters, at most
// 4 bytes each in UTF-8.
const maxTextBytes = 4 * 32767

var errTextTooLong = fmt.Errorf("%w: text longer than Excel allows", ErrInvalid)

// maxColumns is Excel's column limit (XFD).
const maxColumns = 16384

// Relationship types used to find the parts of the workbook.
const (
	relOfficeDocument = "/officeDocument"
	relSharedStrings  = "/sharedStrings"
	relCoreProperties = "/metadata/core-properties"
)

// Cell is one non-empty cell. Text holds the string, the number as written in
// the file, or "1"/"0" for a boolean.
type Cell struct {
	Col  int // 0-based column index
	Kind Kind
	Text string
}

// Number parses a numeric cell.
func (c Cell) Number() (float64, bool) {
	if c.Kind != KindNumber {
		return 0, false
	}
	f, err := strconv.ParseFloat(c.Text, 64)
	return f, err == nil
}

// Row holds the non-empty cells of a row in column order. Num is the 1-based
// row number from the file; rows missing from the file are simply absent.
type Row struct {
	Num   int
	Cells []Cell
}

// Cell returns the cell at column col, or false when it is empty.
func (r Row) Cell(col int) (Cell, bool) {
	for _, c := range r.Cells {
		if c.Col == col {
			return c, true
		}
	}
	return Cell{}, false
}

// Text returns the text at column col without surrounding spaces (non-breaking
// ones included), or "" when the cell is empty.
func (r Row) Text(col int) string {
	c, _ := r.Cell(col)
	return strings.TrimSpace(c.Text)
}

// Limits bound the work done on an untrusted file.
type Limits struct {
	MaxUncompressed int64 // bytes read from the archive, all parts together
	MaxRows         int
	MaxCells        int // non-empty cells in the sheet
}

// SerialDate converts an Excel serial date (1900 date system) to a UTC time.
// The fraction is the time of day, rounded to the second; the wall clock is
// the one the file was written in.
func SerialDate(serial float64) (time.Time, bool) {
	if !(serial >= 1 && serial < 2958466) { // 2958466 is 10000-01-01
		return time.Time{}, false
	}
	days := math.Floor(serial)
	seconds := math.Round((serial - days) * 86400)
	return time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, int(days)).Add(time.Duration(seconds) * time.Second), true
}

// Header maps each name of a header row, NFC-normalized and trimmed, to its
// columns in file order: an export may repeat a name.
type Header map[string][]int

// Col returns the column of the n-th (0-based) occurrence of name.
func (h Header) Col(name string, n int) (int, bool) {
	cols := h[name]
	if n >= len(cols) {
		return 0, false
	}
	return cols[n], true
}

// FindHeader returns the index in rows of the first row, numbered at most
// maxRow, that holds a cell named key, with the columns of that row.
func FindHeader(rows []Row, key string, maxRow int) (int, Header, bool) {
	for i, row := range rows {
		if row.Num > maxRow {
			break
		}
		h := Header{}
		for _, c := range row.Cells {
			name := strings.TrimSpace(norm.NFC.String(c.Text))
			h[name] = append(h[name], c.Col)
		}
		if _, ok := h[key]; ok {
			return i, h, true
		}
	}
	return 0, nil, false
}

// Created returns the creation date declared in the workbook's core
// properties, or false when there is none or it cannot be read. Files saved
// again by another program get a new one, so it is indicative only.
func Created(data []byte, lim Limits) (time.Time, bool) {
	r, err := newReader(data, lim)
	if err != nil {
		return time.Time{}, false
	}
	var pkgRels relationships
	if err := r.decode("_rels/.rels", &pkgRels); err != nil {
		return time.Time{}, false
	}
	corePath, ok := pkgRels.target("", relCoreProperties)
	if !ok {
		return time.Time{}, false
	}
	var core struct {
		Created string `xml:"http://purl.org/dc/terms/ created"`
	}
	if err := r.decode(corePath, &core); err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(core.Created))
	return t, err == nil
}

// ReadFirstSheet returns the rows of the workbook's first sheet.
func ReadFirstSheet(data []byte, lim Limits) ([]Row, error) {
	r, err := newReader(data, lim)
	if err != nil {
		return nil, err
	}
	var pkgRels relationships
	if err := r.decode("_rels/.rels", &pkgRels); err != nil {
		return nil, err
	}
	wbPath, ok := pkgRels.target("", relOfficeDocument)
	if !ok {
		return nil, fmt.Errorf("%w: no workbook part", ErrInvalid)
	}
	var wb workbook
	if err := r.decode(wbPath, &wb); err != nil {
		return nil, err
	}
	if len(wb.Sheets) == 0 {
		return nil, fmt.Errorf("%w: no sheet", ErrInvalid)
	}
	var wbRels relationships
	if err := r.decode(path.Join(path.Dir(wbPath), "_rels", path.Base(wbPath)+".rels"), &wbRels); err != nil {
		return nil, err
	}
	sheetPath, ok := wbRels.byID(wbPath, wb.Sheets[0].RID)
	if !ok {
		return nil, fmt.Errorf("%w: first sheet not found", ErrInvalid)
	}
	var shared []string
	if ssPath, ok := wbRels.target(wbPath, relSharedStrings); ok {
		if shared, err = r.sharedStrings(ssPath, lim); err != nil {
			return nil, err
		}
	}
	return r.sheet(sheetPath, shared, lim)
}

// newReader opens the archive and checks the sizes its headers declare.
func newReader(data []byte, lim Limits) (*reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	var declared uint64
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		declared += f.UncompressedSize64
		files[f.Name] = f
	}
	if lim.MaxUncompressed < 0 || declared > uint64(lim.MaxUncompressed) {
		return nil, ErrTooLarge
	}
	return &reader{files: files, left: lim.MaxUncompressed}, nil
}

type relationships struct {
	Items []struct {
		ID     string `xml:"Id,attr"`
		Type   string `xml:"Type,attr"`
		Target string `xml:"Target,attr"`
	} `xml:"Relationship"`
}

// find returns the zip path of the first relationship accepted by match.
func (rs relationships) find(base string, match func(id, typ string) bool) (string, bool) {
	for _, it := range rs.Items {
		if match(it.ID, it.Type) {
			return resolve(base, it.Target), true
		}
	}
	return "", false
}

func (rs relationships) target(base, typeSuffix string) (string, bool) {
	return rs.find(base, func(_, typ string) bool { return strings.HasSuffix(typ, typeSuffix) })
}

func (rs relationships) byID(base, id string) (string, bool) {
	return rs.find(base, func(rid, _ string) bool { return rid == id })
}

// resolve turns a relationship target into a zip path, relative to the part
// that declares it unless it is absolute.
func resolve(base, target string) string {
	if abs, ok := strings.CutPrefix(target, "/"); ok {
		return abs
	}
	return path.Join(path.Dir(base), target)
}

type workbook struct {
	Sheets []struct {
		RID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
	} `xml:"sheets>sheet"`
}

// reader opens parts of the archive against a shared decompression budget.
type reader struct {
	files map[string]*zip.File
	left  int64
}

// budgeted counts the bytes actually decompressed, whatever the zip headers
// declare, and fails with ErrTooLarge once the budget is spent.
type budgeted struct {
	src io.Reader
	r   *reader
}

func (b budgeted) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	b.r.left -= int64(n)
	if b.r.left < 0 {
		return n, ErrTooLarge
	}
	return n, err
}

// open runs fn on the decompressed content of the named part.
func (r *reader) open(name string, fn func(io.Reader) error) (err error) {
	f, ok := r.files[name]
	if !ok {
		return fmt.Errorf("%w: missing part %s", ErrInvalid, name)
	}
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("%w: open %s: %w", ErrInvalid, name, err)
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("%w: close %s: %w", ErrInvalid, name, cerr)
		}
	}()
	return fn(budgeted{src: rc, r: r})
}

func (r *reader) decode(name string, v any) error {
	return r.open(name, func(src io.Reader) error {
		if err := xml.NewDecoder(src).Decode(v); err != nil {
			return xmlError(name, err)
		}
		return nil
	})
}

func xmlError(name string, err error) error {
	if errors.Is(err, ErrTooLarge) {
		return ErrTooLarge
	}
	return fmt.Errorf("%w: %s: %w", ErrInvalid, name, err)
}

// tokens feeds fn with the XML tokens of the named part until EOF.
func (r *reader) tokens(name string, fn func(xml.Token) error) error {
	return r.open(name, func(src io.Reader) error {
		d := xml.NewDecoder(src)
		for {
			tok, err := d.Token()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return xmlError(name, err)
			}
			if err := fn(tok); err != nil {
				return err
			}
		}
	})
}

// appendText adds element text to b, within the maxTextBytes cap.
func appendText(b *strings.Builder, data []byte) error {
	if b.Len()+len(data) > maxTextBytes {
		return errTextTooLong
	}
	b.Write(data)
	return nil
}

// sharedStrings reads the shared string table. Rich text runs are joined;
// phonetic runs (<rPh>) are skipped.
func (r *reader) sharedStrings(name string, lim Limits) ([]string, error) {
	var out []string
	var cur strings.Builder
	inItem, inText, phonetic := false, false, 0
	err := r.tokens(name, func(tok xml.Token) error {
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inItem = true
				cur.Reset()
			case tagPhonetic:
				phonetic++
			case "t":
				inText = inItem && phonetic == 0
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				if len(out) >= lim.MaxCells {
					return ErrTooManyCells
				}
				out = append(out, cur.String())
				inItem = false
			case tagPhonetic:
				phonetic--
			case "t":
				inText = false
			}
		case xml.CharData:
			if inText {
				return appendText(&cur, t)
			}
		}
		return nil
	})
	return out, err
}

// sheetState accumulates the cell being read.
type sheetState struct {
	rows     []Row
	cells    int
	lastRow  int
	lastCol  int
	inRow    bool
	col      int
	typ      string
	text     strings.Builder
	inValue  bool
	inInline bool
	phonetic int
}

func (r *reader) sheet(name string, shared []string, lim Limits) ([]Row, error) {
	st := &sheetState{}
	err := r.tokens(name, func(tok xml.Token) error {
		switch t := tok.(type) {
		case xml.StartElement:
			return st.start(t, lim)
		case xml.EndElement:
			return st.end(t.Name.Local, shared, lim)
		case xml.CharData:
			if st.inValue || (st.inInline && st.phonetic == 0) {
				return appendText(&st.text, t)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st.rows, nil
}

func (st *sheetState) start(t xml.StartElement, lim Limits) error {
	switch t.Name.Local {
	case "row":
		if len(st.rows) >= lim.MaxRows {
			return ErrTooManyRows
		}
		num := st.lastRow + 1
		if ref := attr(t, "r"); ref != "" {
			n, err := strconv.Atoi(ref)
			if err != nil || n <= 0 {
				return fmt.Errorf("%w: row number %q", ErrInvalid, ref)
			}
			num = n
		}
		st.rows = append(st.rows, Row{Num: num})
		st.lastRow, st.lastCol, st.inRow = num, -1, true
	case "c":
		if !st.inRow {
			return fmt.Errorf("%w: cell outside a row", ErrInvalid)
		}
		col := st.lastCol + 1
		if ref := attr(t, "r"); ref != "" {
			c, err := columnIndex(ref)
			if err != nil {
				return err
			}
			col = c
		}
		st.lastCol, st.col, st.typ = col, col, attr(t, "t")
		st.text.Reset()
	case "v":
		st.inValue = true
	case "is":
		st.inInline = true
	case tagPhonetic:
		st.phonetic++
	}
	return nil
}

func (st *sheetState) end(name string, shared []string, lim Limits) error {
	switch name {
	case "v":
		st.inValue = false
	case "is":
		st.inInline = false
	case tagPhonetic:
		st.phonetic--
	case "row":
		st.inRow = false
	case "c":
		c, ok, err := makeCell(st.col, st.typ, st.text.String(), shared)
		if err != nil || !ok {
			return err
		}
		st.cells++
		if st.cells > lim.MaxCells {
			return ErrTooManyCells
		}
		row := &st.rows[len(st.rows)-1]
		row.Cells = append(row.Cells, c)
	}
	return nil
}

func makeCell(col int, typ, raw string, shared []string) (Cell, bool, error) {
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || i < 0 || i >= len(shared) {
			return Cell{}, false, fmt.Errorf("%w: shared string index %q", ErrInvalid, raw)
		}
		return Cell{Col: col, Kind: KindString, Text: shared[i]}, shared[i] != "", nil
	case "inlineStr", "str", "d":
		return Cell{Col: col, Kind: KindString, Text: raw}, raw != "", nil
	case "b":
		raw = strings.TrimSpace(raw)
		return Cell{Col: col, Kind: KindBool, Text: raw}, raw != "", nil
	case "e":
		return Cell{Col: col, Kind: KindError}, true, nil
	case "", "n":
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return Cell{}, false, nil
		}
		if f, err := strconv.ParseFloat(raw, 64); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return Cell{}, false, fmt.Errorf("%w: number %q", ErrInvalid, raw)
		}
		return Cell{Col: col, Kind: KindNumber, Text: raw}, true, nil
	default:
		return Cell{}, false, fmt.Errorf("%w: cell type %q", ErrInvalid, typ)
	}
}

// columnIndex reads the letters of a cell reference: "C12" → 2.
func columnIndex(ref string) (int, error) {
	n, i := 0, 0
	for ; i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z'; i++ {
		n = n*26 + int(ref[i]-'A'+1)
		if n > maxColumns {
			return 0, fmt.Errorf("%w: column in %q", ErrInvalid, ref)
		}
	}
	if i == 0 {
		return 0, fmt.Errorf("%w: cell reference %q", ErrInvalid, ref)
	}
	return n - 1, nil
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name && a.Name.Space == "" {
			return a.Value
		}
	}
	return ""
}
