// Package campaign holds the export of VPDive's membership campaign (design
// 2026-10-20): who paid the membership and the licence, and whose file the
// club validated. Only the columns listed in findColumns are ever read: the
// export also holds the CACI, which is health data.
package campaign

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// The two columns that, together, mark the header row.
const (
	columnStatus     = "Statut"
	columnMembership = "Adhésion"
)

const headerSearchRows = 10

// Values of « Statut » in the export.
const (
	StatusPending   = "En cours"
	StatusValidated = "Validé"
)

// States of an entry VPDive writes; any other is kept as received.
const (
	StatePaid   = "Réglé"
	StateInCart = "Ajouté au panier mais pas encore payé"
)

var (
	// entryText is « <label> (<state>) - <amount> €[ - <method> (<DD/MM/YYYY>)] ».
	entryText = regexp.MustCompile(`^(.+?) \(([^()]+)\) - (-?\d[\d \x{a0}\x{202f}]*(?:,\d+)?)[ \x{a0}\x{202f}]?€(?: - (.+) \((\d\d/\d\d/\d{4})\))?$`)
	// validationText ends « le DD/MM:YYYY HH:MM par Prénom Nom »; VPDive writes
	// ':' between month and year, '/' is accepted too.
	validationText = regexp.MustCompile(`le (\d\d)/(\d\d)[:/](\d{4}) (\d\d:\d\d) par (.+)$`)
	exportedText   = regexp.MustCompile(`le (\d\d/\d\d/\d{4}) à (\d\d:\d\d)`)
	digitSpacing   = strings.NewReplacer(" ", "", "\u00a0", "", "\u202f", "")
)

// Entry is one line of an « Adhésion » or « License » cell. An unreadable
// line keeps its text in Raw and nothing else.
type Entry struct {
	Label  string          `json:"label,omitzero"`
	State  string          `json:"state,omitzero"`
	Amount payments.Amount `json:"amount,omitzero"`
	Method string          `json:"method,omitzero"`  // only for a payment recorded by hand or by VPayDive
	PaidOn time.Time       `json:"paid_on,omitzero"` // the method's date, midnight in Paris
	Raw    string          `json:"raw,omitzero"`
}

// Unreadable reports an entry outside the grammar, to check by hand.
func (e Entry) Unreadable() bool { return e.Raw != "" }

// Person is one row of the export, reduced to the read columns. It is stored
// as sealed JSON; NameKey is hashed at import and never stored.
type Person struct {
	Row     int    `json:"-"`
	NameKey string `json:"-"`

	Status               string    `json:"status"`
	LastName             string    `json:"last_name"`
	FirstName            string    `json:"first_name,omitzero"`
	ValidatedAt          time.Time `json:"validated_at,omitzero"`
	ValidatedBy          string    `json:"validated_by,omitzero"` // « Prénom Nom », as written
	ValidationUnreadable bool      `json:"validation_unreadable,omitzero"`
	Membership           []Entry   `json:"membership,omitempty"`
	Licence              []Entry   `json:"licence,omitempty"`
	Settlement           string    `json:"settlement,omitzero"` // « Règlement », as read
}

// Origin returns the row of the person in the export and their name key.
func (p Person) Origin() (int, string) { return p.Row, p.NameKey }

// Export is a parsed campaign export.
type Export struct {
	Exported time.Time // the date on row 2, indicative; zero if unreadable
	People   []Person
	Skipped  int    // rows without a last name
	FileHash []byte // HMAC of the file, set by the caller (spec §7.6)
}

// ProblemKind classifies a refused export.
type ProblemKind string

// Reasons to refuse an export.
const (
	ProblemNoHeader      ProblemKind = "no_header"
	ProblemMissingColumn ProblemKind = "missing_column"
)

// ParseError explains why a campaign export is refused; Column names the
// missing column.
type ParseError struct {
	Kind   ProblemKind
	Column string
}

func (e *ParseError) Error() string {
	if e.Kind == ProblemMissingColumn {
		return fmt.Sprintf("campaign export: missing column %q", e.Column)
	}
	return fmt.Sprintf("campaign export: no row with %q and %q in the first %d rows", columnStatus, columnMembership, headerSearchRows)
}

// columns are the positions of the read columns; -1 when an optional one is
// absent, which reads as an empty cell.
type columns struct {
	status, last, first, membership, licence int
	validation, settlement                   int
}

// Parse reads a campaign export. The header row is the first of the first
// ten rows holding both « Statut » and « Adhésion »; columns are found by
// name. Nothing in a row refuses the file: an unreadable entry or validation
// is flagged instead. Dates are read in loc.
func Parse(rows []xlsx.Row, loc *time.Location) (*Export, error) {
	hi, header, ok := findHeader(rows)
	if !ok {
		return nil, &ParseError{Kind: ProblemNoHeader}
	}
	c, err := findColumns(header)
	if err != nil {
		return nil, err
	}
	exp := &Export{}
	for _, row := range rows[:hi] {
		if row.Num == 2 && len(row.Cells) > 0 {
			exp.Exported = exportDate(row.Cells[0].Text, loc)
		}
	}
	for _, row := range rows[hi+1:] {
		last := row.Text(c.last)
		if last == "" {
			exp.Skipped++
			continue
		}
		p := Person{
			Row:        row.Num,
			NameKey:    secure.NameKey(last, row.Text(c.first)),
			Status:     row.Text(c.status),
			LastName:   last,
			FirstName:  row.Text(c.first),
			Membership: entries(row.Text(c.membership), loc),
			Licence:    entries(row.Text(c.licence), loc),
			Settlement: row.Text(c.settlement),
		}
		p.ValidatedAt, p.ValidatedBy, p.ValidationUnreadable = validation(row.Text(c.validation), loc)
		exp.People = append(exp.People, p)
	}
	return exp, nil
}

// findHeader is xlsx.FindHeader on « Adhésion », skipping rows without
// « Statut ».
func findHeader(rows []xlsx.Row) (int, xlsx.Header, bool) {
	for start := 0; start < len(rows); {
		i, h, ok := xlsx.FindHeader(rows[start:], columnMembership, headerSearchRows)
		if !ok {
			break
		}
		if _, ok := h[columnStatus]; ok {
			return start + i, h, true
		}
		start += i + 1
	}
	return 0, nil, false
}

// findColumns lists every column read. Âge, Section, CACI, Attestation
// prolongation, Licence (the number), Carte, Formation and the documents
// never are.
func findColumns(h xlsx.Header) (columns, error) {
	var c columns
	for _, r := range []struct {
		dst  *int
		name string
	}{
		{&c.status, columnStatus}, {&c.last, "Nom"}, {&c.first, "Prénom"},
		{&c.membership, columnMembership}, {&c.licence, "License"},
	} {
		col, ok := h.Col(r.name, 0)
		if !ok {
			return columns{}, &ParseError{Kind: ProblemMissingColumn, Column: r.name}
		}
		*r.dst = col
	}
	c.validation, c.settlement = optionalCol(h, "Status de validation"), optionalCol(h, "Règlement")
	return c, nil
}

func optionalCol(h xlsx.Header, name string) int {
	col, ok := h.Col(name, 0)
	if !ok {
		return -1
	}
	return col
}

// entries reads an « Adhésion » or « License » cell, one entry per line.
func entries(text string, loc *time.Location) []Entry {
	var out []Entry
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, entry(line, loc))
		}
	}
	return out
}

func entry(line string, loc *time.Location) Entry {
	m := entryText.FindStringSubmatch(line)
	if m == nil {
		return Entry{Raw: line}
	}
	amount, ok := payments.Hundredths(strings.Replace(digitSpacing.Replace(m[3]), ",", ".", 1))
	if !ok {
		return Entry{Raw: line}
	}
	e := Entry{Label: m[1], State: m[2], Amount: amount, Method: m[4]}
	if m[5] != "" {
		paidOn, err := time.ParseInLocation("02/01/2006", m[5], loc)
		if err != nil {
			return Entry{Raw: line}
		}
		e.PaidOn = paidOn
	}
	return e
}

// validation reads the date and author of « Status de validation ».
// Unreadable text gives unreadable only: it holds a name erasure could not
// find, so it is never kept.
func validation(text string, loc *time.Location) (at time.Time, by string, unreadable bool) {
	if text == "" {
		return time.Time{}, "", false
	}
	m := validationText.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, "", true
	}
	at, err := time.ParseInLocation("02/01/2006 15:04", m[1]+"/"+m[2]+"/"+m[3]+" "+m[4], loc)
	by = strings.TrimSpace(m[5])
	if err != nil || by == "" {
		return time.Time{}, "", true
	}
	return at, by, false
}

// exportDate reads « … le DD/MM/YYYY à HH:MM »; zero when unreadable.
func exportDate(text string, loc *time.Location) time.Time {
	m := exportedText.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	t, err := time.ParseInLocation("02/01/2006 15:04", m[1]+" "+m[2], loc)
	if err != nil {
		return time.Time{}
	}
	return t
}
