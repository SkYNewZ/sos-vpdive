// Package payments holds the VPDive payments export: its import, the
// « Paiements VPDive » block of a ticket and the cancelled outings still to
// delete (spec §7.1, §7.3, §7.4). Lines are stored without names and
// attributed at display time, through the members list.
package payments

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// ImportLimits are the workbook limits applied to an upload (spec §7.2):
// 50 MiB decompressed, 20 000 rows.
func ImportLimits() xlsx.Limits {
	return xlsx.Limits{MaxUncompressed: 50 << 20, MaxRows: 20_000, MaxCells: 1_000_000}
}

// Columns read from the export. Commentaire, Civilité, Adresse, Code postal,
// Ville and every other column are never read.
const (
	ColumnLastName  = "Nom"
	ColumnFirstName = "Prénom"
	ColumnUnitPrice = "Prix unitaire"
	ColumnQuantity  = "Quantité"
	ColumnPaid      = "Montant paiement"
	ColumnDiscount  = "Montant réduc."
	ColumnState     = "État"
	ColumnProduct   = "Produit/Événement"
	ColumnCreated   = "Créé le"
	columnMethod    = "Methode de paiement"
	columnType      = "Type de produit"
	columnStarts    = "Du"
	columnPaidAt    = "Date paiement"
	columnRental    = "Materiel" // second occurrence: the rental amount
)

// Values of the export the tool reads (spec §7.3).
const (
	StatePaid      = "Payé"
	StateDue       = "À payer"
	StateCancelled = "Annulé"
	StatePartial   = "Paiement partiel"
	MethodPrepaid  = "Prépayé"
	MethodVPayDive = "vpaydive"
	TypeCard       = "Carte"
	TypeTraining   = "Formation"
)

const (
	headerSearchRows = 10
	// maxEmptyProductPercent is the share of empty titles above which the
	// file is the known export bug, not an isolated gap.
	maxEmptyProductPercent = 5
)

var (
	numberText   = regexp.MustCompile(`^[+-]?\d+(?:[.,]\d+)?$`)
	digitSpacing = strings.NewReplacer(" ", "", "\u00a0", "", "\u202f", "")
)

// Line is one line of the export, reduced to the kept columns. It is stored
// as sealed JSON; NameKey is hashed at import and never stored.
type Line struct {
	Row     int    `json:"-"`
	NameKey string `json:"-"`

	UnitPrice   Amount    `json:"unit_price"`
	Quantity    Amount    `json:"quantity"`
	Paid        Amount    `json:"paid"`
	Discount    Amount    `json:"discount"`
	Rental      Amount    `json:"rental,omitzero"`
	State       string    `json:"state"`
	Method      string    `json:"method,omitzero"`
	ProductType string    `json:"product_type,omitzero"`
	Product     string    `json:"product,omitzero"`
	Starts      time.Time `json:"starts,omitzero"`  // « Du »: the outing's start
	PaidAt      time.Time `json:"paid_at,omitzero"` // « Date paiement »
	Created     time.Time `json:"created"`
}

// Export is a parsed payments export.
type Export struct {
	Created       time.Time // workbook creation date, indicative; zero if absent
	Lines         []Line
	Skipped       int            // lines without a name
	UnknownStates map[string]int // states other than the four known ones
	PeriodFrom    time.Time      // earliest and latest « Créé le »
	PeriodTo      time.Time
}

// ProblemKind classifies a refused export.
type ProblemKind string

// Reasons to refuse an export.
const (
	ProblemNoHeader      ProblemKind = "no_header"
	ProblemMissingColumn ProblemKind = "missing_column"
	ProblemInvalidNumber ProblemKind = "invalid_number"
	ProblemInvalidDate   ProblemKind = "invalid_date"
	ProblemEmptyProduct  ProblemKind = "empty_product"
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
		return fmt.Sprintf("payments export: no %q column in the first %d rows", ColumnCreated, headerSearchRows)
	case ProblemMissingColumn:
		return fmt.Sprintf("payments export: missing column %q", e.Column)
	case ProblemInvalidNumber, ProblemInvalidDate:
		return fmt.Sprintf("payments export: %s on rows %v", e.Kind, e.Rows)
	case ProblemEmptyProduct:
		return fmt.Sprintf("payments export: %q empty on more than %d %% of the lines", ColumnProduct, maxEmptyProductPercent)
	}
	return fmt.Sprintf("payments export: %s", e.Kind)
}

// columns are the positions of the read columns; -1 when an optional one is
// absent, which reads as an empty cell.
type columns struct {
	last, first, unitPrice, quantity, paid, discount, state, product, created int
	method, productType, starts, paidAt, rental                               int
}

// Parse reads a payments export (spec §7.3). The header row is the first of
// the first ten rows holding a "Créé le" cell; columns are found by name. The
// whole file is validated before anything is returned. created is the
// workbook creation date; dates are read in loc.
func Parse(rows []xlsx.Row, created time.Time, loc *time.Location) (*Export, error) {
	hi, header, ok := xlsx.FindHeader(rows, ColumnCreated, headerSearchRows)
	if !ok {
		return nil, &ParseError{Kind: ProblemNoHeader}
	}
	cols, err := findColumns(header)
	if err != nil {
		return nil, err
	}
	exp := &Export{Created: created, UnknownStates: map[string]int{}}
	var badNumbers, badDates []int
	emptyProducts := 0
	for _, row := range rows[hi+1:] {
		l, numbersOK, dateOK := readLine(row, cols, loc)
		if l.NameKey == "|" { // neither last nor first name
			exp.Skipped++
			continue
		}
		if !numbersOK {
			badNumbers = append(badNumbers, row.Num)
		}
		if !dateOK {
			badDates = append(badDates, row.Num)
		}
		if l.Product == "" {
			emptyProducts++
		}
		switch l.State {
		case StatePaid, StateDue, StateCancelled, StatePartial:
		default:
			exp.UnknownStates[l.State]++
		}
		if dateOK && (exp.PeriodFrom.IsZero() || l.Created.Before(exp.PeriodFrom)) {
			exp.PeriodFrom = l.Created
		}
		if dateOK && l.Created.After(exp.PeriodTo) {
			exp.PeriodTo = l.Created
		}
		exp.Lines = append(exp.Lines, l)
	}
	switch {
	case len(badNumbers) > 0:
		return nil, &ParseError{Kind: ProblemInvalidNumber, Rows: badNumbers}
	case len(badDates) > 0:
		return nil, &ParseError{Kind: ProblemInvalidDate, Rows: badDates}
	case emptyProducts*100 > maxEmptyProductPercent*len(exp.Lines):
		return nil, &ParseError{Kind: ProblemEmptyProduct}
	}
	return exp, nil
}

func findColumns(h xlsx.Header) (columns, error) {
	required := func(name string) (int, error) {
		col, ok := h.Col(name, 0)
		if !ok {
			return 0, &ParseError{Kind: ProblemMissingColumn, Column: name}
		}
		return col, nil
	}
	optional := func(name string, n int) int {
		col, ok := h.Col(name, n)
		if !ok {
			return -1
		}
		return col
	}
	var c columns
	for _, r := range []struct {
		dst  *int
		name string
	}{
		{&c.last, ColumnLastName}, {&c.first, ColumnFirstName}, {&c.unitPrice, ColumnUnitPrice},
		{&c.quantity, ColumnQuantity}, {&c.paid, ColumnPaid}, {&c.discount, ColumnDiscount},
		{&c.state, ColumnState}, {&c.product, ColumnProduct}, {&c.created, ColumnCreated},
	} {
		col, err := required(r.name)
		if err != nil {
			return columns{}, err
		}
		*r.dst = col
	}
	c.method, c.productType = optional(columnMethod, 0), optional(columnType, 0)
	c.starts, c.paidAt, c.rental = optional(columnStarts, 0), optional(columnPaidAt, 0), optional(columnRental, 1)
	return c, nil
}

// readLine reads one row; numbersOK and createdOK report whether its
// required numbers and its creation date could be read.
func readLine(row xlsx.Row, c columns, loc *time.Location) (l Line, numbersOK, createdOK bool) {
	l = Line{
		Row:         row.Num,
		NameKey:     secure.NameKey(row.Text(c.last), row.Text(c.first)),
		State:       row.Text(c.state),
		Product:     row.Text(c.product),
		Method:      row.Text(c.method),
		ProductType: row.Text(c.productType),
	}
	numbersOK = true
	for _, n := range []struct {
		dst *Amount
		col int
	}{
		{&l.UnitPrice, c.unitPrice}, {&l.Quantity, c.quantity}, {&l.Paid, c.paid}, {&l.Discount, c.discount},
	} {
		v, ok := amountAt(row, n.col)
		*n.dst, numbersOK = v, numbersOK && ok
	}
	l.Rental, _ = amountAt(row, c.rental) // optional: unreadable reads as none
	l.Created, createdOK = dateAt(row, c.created, loc, "02/01/2006 15:04:05")
	l.Starts, _ = dateAt(row, c.starts, loc, "02/01/2006 15:04", "02/01/2006")
	l.PaidAt, _ = dateAt(row, c.paidAt, loc, "02/01/2006 15:04", "02/01/2006")
	return l, numbersOK, createdOK
}

// amountAt reads the amount at col; an empty or absent cell is 0.
func amountAt(row xlsx.Row, col int) (Amount, bool) {
	c, ok := row.Cell(col)
	if !ok {
		return 0, true
	}
	return amount(c)
}

// amount reads a number cell, or text holding a number (decimal comma or
// point, spaces as thousands separators), as an exact count of hundredths,
// rounded half away from zero.
func amount(c xlsx.Cell) (Amount, bool) {
	text := strings.TrimSpace(c.Text)
	switch c.Kind {
	case xlsx.KindNumber:
	case xlsx.KindString:
		text = strings.Replace(digitSpacing.Replace(text), ",", ".", 1)
		if !numberText.MatchString(text) {
			return 0, false
		}
	case xlsx.KindBool:
		return 0, false
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return 0, false
	}
	hundredths, err := strconv.ParseInt(r.Mul(r, big.NewRat(100, 1)).FloatString(0), 10, 64)
	if err != nil {
		return 0, false
	}
	return Amount(hundredths), true
}

// dateAt reads a native Excel date or text in one of layouts, as a wall clock
// in loc. Empty, absent or unreadable gives false.
func dateAt(row xlsx.Row, col int, loc *time.Location, layouts ...string) (time.Time, bool) {
	c, ok := row.Cell(col)
	if !ok {
		return time.Time{}, false
	}
	if f, ok := c.Number(); ok {
		d, ok := xlsx.SerialDate(f)
		if !ok {
			return time.Time{}, false
		}
		return time.Date(d.Year(), d.Month(), d.Day(), d.Hour(), d.Minute(), d.Second(), 0, loc), true
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, strings.TrimSpace(c.Text), loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
