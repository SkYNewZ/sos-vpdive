// Package members holds the whitelist: the VPDive members export, its import
// with preview and confirmation, and the lookup used by the public form
// (spec §3.6, §7.2).
package members

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// Columns read from the export. Every other column is ignored at reading
// time and never stored (birth dates, addresses, phones, emergency contacts,
// CACI, comments).
const (
	ColumnEmail     = "Email"
	ColumnLastName  = "Nom"
	ColumnFirstName = "Prénom"
	columnSeasons   = "Année(s)"
	columnLicence   = "Expire le"
)

const (
	headerSearchRows = 10
	exportDateRow    = 2
)

var exportDatePattern = regexp.MustCompile(`(\d{2}/\d{2}/\d{4}) à (\d{2}:\d{2})`)

// Member is one account of the export, reduced to the kept columns.
type Member struct {
	Row            int
	LastName       string
	FirstName      string
	Email          string  // normalized
	Seasons        *string // nil when the export has no "Année(s)" column
	LicenceExpires string  // YYYY-MM-DD, or "" when empty or unreadable
}

// Export is a parsed members export.
type Export struct {
	ExportedAt time.Time // zero when row 2 holds no readable export date
	Members    []Member
	Skipped    int    // rows without an email
	FileHash   []byte // HMAC of the file, set by the caller (spec §7.6)
}

// ProblemKind classifies a refused export.
type ProblemKind string

// Reasons to refuse an export.
const (
	ProblemNoHeader       ProblemKind = "no_header"
	ProblemMissingColumn  ProblemKind = "missing_column"
	ProblemDuplicateEmail ProblemKind = "duplicate_email"
	ProblemInvalidEmail   ProblemKind = "invalid_email"
)

// ParseError explains why an export is refused. Rows are file row numbers.
type ParseError struct {
	Kind   ProblemKind
	Column string
	Rows   []int
}

func (e *ParseError) Error() string {
	switch e.Kind {
	case ProblemNoHeader:
		return fmt.Sprintf("members export: no %q column in the first %d rows", ColumnEmail, headerSearchRows)
	case ProblemMissingColumn:
		return fmt.Sprintf("members export: missing column %q", e.Column)
	case ProblemDuplicateEmail, ProblemInvalidEmail:
		return fmt.Sprintf("members export: %s on rows %v", e.Kind, e.Rows)
	}
	return fmt.Sprintf("members export: %s", e.Kind)
}

// Parse reads a members export. The header row is the first of the first ten
// rows holding an "Email" cell; columns are found by name, first occurrence
// only (the second "Nom"/"Prénom" pair is the emergency contact). The whole
// file is validated before anything is returned. loc is the zone of the
// export date written in row 2.
func Parse(rows []xlsx.Row, loc *time.Location) (*Export, error) {
	hi, header, ok := xlsx.FindHeader(rows, ColumnEmail, headerSearchRows)
	if !ok {
		return nil, &ParseError{Kind: ProblemNoHeader}
	}
	cols := map[string]int{}
	for _, name := range []string{ColumnEmail, ColumnLastName, ColumnFirstName} {
		col, ok := header.Col(name, 0)
		if !ok {
			return nil, &ParseError{Kind: ProblemMissingColumn, Column: name}
		}
		cols[name] = col
	}
	seasonsCol, hasSeasons := header.Col(columnSeasons, 0)
	licenceCol, hasLicence := header.Col(columnLicence, 0)

	exp := &Export{ExportedAt: exportDate(rows, loc)}
	firstRow := map[string]int{}
	var invalid, duplicates []int
	for _, row := range rows[hi+1:] {
		raw := row.Text(cols[ColumnEmail])
		if raw == "" {
			exp.Skipped++
			continue
		}
		email, err := secure.NormalizeEmail(raw)
		if err != nil {
			invalid = append(invalid, row.Num)
			continue
		}
		if first, dup := firstRow[email]; dup {
			duplicates = append(duplicates, first, row.Num)
			continue
		}
		firstRow[email] = row.Num
		m := Member{
			Row:       row.Num,
			LastName:  row.Text(cols[ColumnLastName]),
			FirstName: row.Text(cols[ColumnFirstName]),
			Email:     email,
		}
		if hasSeasons {
			s := seasons(row, seasonsCol)
			m.Seasons = &s
		}
		if hasLicence {
			m.LicenceExpires = licenceDate(row, licenceCol)
		}
		exp.Members = append(exp.Members, m)
	}
	if len(invalid) > 0 {
		return nil, &ParseError{Kind: ProblemInvalidEmail, Rows: invalid}
	}
	if len(duplicates) > 0 {
		slices.Sort(duplicates)
		return nil, &ParseError{Kind: ProblemDuplicateEmail, Rows: slices.Compact(duplicates)}
	}
	return exp, nil
}

// AmbiguousGroups counts names shared by several accounts once normalized:
// payments cannot be attributed to such names (spec §7.3).
func AmbiguousGroups(ms []Member) (groups, accounts int) {
	count := map[string]int{}
	for _, m := range ms {
		key := secure.NameKey(m.LastName, m.FirstName)
		if key == "|" { // no name at all: nothing to attribute a payment to
			continue
		}
		count[key]++
	}
	for _, n := range count {
		if n > 1 {
			groups++
			accounts += n
		}
	}
	return groups, accounts
}

func exportDate(rows []xlsx.Row, loc *time.Location) time.Time {
	for _, row := range rows {
		if row.Num != exportDateRow {
			continue
		}
		for _, c := range row.Cells {
			m := exportDatePattern.FindStringSubmatch(c.Text)
			if m == nil {
				continue
			}
			if t, err := time.ParseInLocation("02/01/2006 15:04", m[1]+" "+m[2], loc); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// seasons reads "Année(s)": a number when there is one season, else a list.
func seasons(row xlsx.Row, col int) string {
	c, ok := row.Cell(col)
	if !ok {
		return ""
	}
	if f, ok := c.Number(); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	var parts []string
	for p := range strings.SplitSeq(c.Text, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

// licenceDate reads "Expire le": text DD/MM/YYYY or a native Excel date.
// An unreadable value is left empty (spec §7.2).
func licenceDate(row xlsx.Row, col int) string {
	c, ok := row.Cell(col)
	if !ok {
		return ""
	}
	if f, ok := c.Number(); ok {
		if d, ok := xlsx.SerialDate(f); ok {
			return d.Format(time.DateOnly)
		}
		return ""
	}
	d, err := time.Parse("02/01/2006", strings.TrimSpace(c.Text))
	if err != nil {
		return ""
	}
	return d.Format(time.DateOnly)
}
