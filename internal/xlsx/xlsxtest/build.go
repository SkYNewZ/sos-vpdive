// Package xlsxtest builds small .xlsx workbooks for tests and for the
// synthetic fixtures in testdata/fixtures. Output is deterministic.
package xlsxtest

import (
	"archive/zip"
	"bytes"
	"fmt"
	"html"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Inline is a string cell written inline (t="inlineStr") instead of through
// the shared strings table.
type Inline string

// Sheet lists rows; a nil row leaves a gap in the row numbers. In a row, a nil
// cell leaves its column empty. Cells are string, Inline, int, float64 or
// bool; any other value is written as its fmt.Sprint string.
type Sheet [][]any

const (
	xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
	mainNS    = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	pkgRelNS  = "http://schemas.openxmlformats.org/package/2006/relationships"
	docRelNS  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
)

// modified is fixed so that generated fixtures are byte-for-byte stable.
var modified = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Raw returns a one-sheet workbook whose <sheetData> content is sheetData and
// whose shared strings are the given <si> elements, for edge cases.
func Raw(tb testing.TB, sheetData string, sharedItems ...string) []byte {
	tb.Helper()
	b, err := assemble([]string{sheetData}, sharedItems, time.Time{})
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// Build returns a workbook whose sheets appear in the given order.
func Build(tb testing.TB, sheets ...Sheet) []byte {
	tb.Helper()
	return BuildCreated(tb, time.Time{}, sheets...)
}

// BuildCreated is Build with core properties declaring the creation date
// created; a zero created writes none.
func BuildCreated(tb testing.TB, created time.Time, sheets ...Sheet) []byte {
	tb.Helper()
	var shared []string
	index := map[string]int{}
	sheetXML := make([]string, len(sheets))
	for i, s := range sheets {
		var b strings.Builder
		for r, row := range s {
			if row == nil {
				continue
			}
			fmt.Fprintf(&b, `<row r="%d">`, r+1)
			for c, v := range row {
				if v == nil {
					continue
				}
				ref := colName(c) + strconv.Itoa(r+1)
				switch v := v.(type) {
				case Inline:
					fmt.Fprintf(&b, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, html.EscapeString(string(v)))
				case int:
					fmt.Fprintf(&b, `<c r="%s"><v>%d</v></c>`, ref, v)
				case float64:
					fmt.Fprintf(&b, `<c r="%s"><v>%s</v></c>`, ref, strconv.FormatFloat(v, 'f', -1, 64))
				case bool:
					fmt.Fprintf(&b, `<c r="%s" t="b"><v>%s</v></c>`, ref, boolDigit(v))
				default:
					text := fmt.Sprint(v)
					idx, ok := index[text]
					if !ok {
						idx = len(shared)
						index[text] = idx
						shared = append(shared, `<si><t xml:space="preserve">`+html.EscapeString(text)+`</t></si>`)
					}
					fmt.Fprintf(&b, `<c r="%s" t="s"><v>%d</v></c>`, ref, idx)
				}
			}
			b.WriteString(`</row>`)
		}
		sheetXML[i] = b.String()
	}
	b, err := assemble(sheetXML, shared, created)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

func assemble(sheets, sharedItems []string, created time.Time) ([]byte, error) {
	var types, wbSheets, wbRels, pkgRels strings.Builder
	for i := range sheets {
		n := i + 1
		fmt.Fprintf(&types, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, n)
		fmt.Fprintf(&wbSheets, `<sheet name="Feuille %d" sheetId="%d" r:id="rId%d"/>`, n, n, n+1)
		fmt.Fprintf(&wbRels, `<Relationship Id="rId%d" Type="%s/worksheet" Target="worksheets/sheet%d.xml"/>`, n+1, docRelNS, n)
	}
	if !created.IsZero() {
		types.WriteString(`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>`)
		pkgRels.WriteString(`<Relationship Id="rId2" Type="` + pkgRelNS + `/metadata/core-properties" Target="docProps/core.xml"/>`)
	}
	parts := append(make([]struct{ name, body string }, 0, 6+len(sheets)), []struct{ name, body string }{
		{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/>` +
			types.String() + `</Types>`},
		{"_rels/.rels", `<Relationships xmlns="` + pkgRelNS + `">` +
			`<Relationship Id="rId1" Type="` + docRelNS + `/officeDocument" Target="xl/workbook.xml"/>` + pkgRels.String() + `</Relationships>`},
		{"xl/workbook.xml", `<workbook xmlns="` + mainNS + `" xmlns:r="` + docRelNS + `"><sheets>` + wbSheets.String() + `</sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<Relationships xmlns="` + pkgRelNS + `">` +
			`<Relationship Id="rId1" Type="` + docRelNS + `/sharedStrings" Target="sharedStrings.xml"/>` +
			wbRels.String() + `</Relationships>`},
		{"xl/sharedStrings.xml", fmt.Sprintf(`<sst xmlns="%s" count="%d" uniqueCount="%d">`, mainNS, len(sharedItems), len(sharedItems)) +
			strings.Join(sharedItems, "") + `</sst>`},
	}...)
	if !created.IsZero() {
		parts = append(parts, struct{ name, body string }{"docProps/core.xml",
			`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" ` +
				`xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">` +
				`<dcterms:created xsi:type="dcterms:W3CDTF">` + created.Format(time.RFC3339) + `</dcterms:created></cp:coreProperties>`})
	}
	for i, s := range sheets {
		parts = append(parts, struct{ name, body string }{
			fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1),
			`<worksheet xmlns="` + mainNS + `"><sheetData>` + s + `</sheetData></worksheet>`,
		})
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: p.name, Method: zip.Deflate, Modified: modified})
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", p.name, err)
		}
		if _, err := w.Write([]byte(xmlHeader + p.body)); err != nil {
			return nil, fmt.Errorf("write %s: %w", p.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close zip: %w", err)
	}
	return buf.Bytes(), nil
}

// colName turns a 0-based column index into its letters: 0 → A, 26 → AA.
func colName(i int) string {
	name := ""
	for n := i + 1; n > 0; n = (n - 1) / 26 {
		name = string(rune('A'+(n-1)%26)) + name
	}
	return name
}

func boolDigit(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
