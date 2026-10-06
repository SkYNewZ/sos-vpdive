package payments

import (
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// columnMollieAmount finds the header of the VPayDive export: no other export
// has it.
const columnMollieAmount = "Montant Panier"

// Values of « Payé » in the VPayDive export: what VPDive recorded of a
// payment Mollie collected (spec §7.5).
const (
	SettledYes = "Oui"
	SettledNo  = "Non"
)

// MollieLine is one line of the VPayDive export, a cart item paid through
// Mollie, reduced to the kept columns (spec §7.5). It is stored as sealed
// JSON; NameKey is hashed at import and never stored.
type MollieLine struct {
	Row     int    `json:"-"`
	NameKey string `json:"-"`

	Product string    `json:"product,omitzero"` // « Type Panier »
	Service string    `json:"service,omitzero"` // « Prestation »: the outing or rental
	Starts  time.Time `json:"starts,omitzero"`  // « Date Début »
	Amount  Amount    `json:"amount"`           // « Montant Panier »
	Settled string    `json:"settled"`          // « Payé », as read
	PaidAt  time.Time `json:"paid_at"`          // « Date paiement », to the minute
	Method  string    `json:"method,omitzero"`  // « Méthode »
}

// Unsettled reports a line Mollie collected that VPDive did not mark paid:
// the member may pay twice (spec §7.7).
func (l MollieLine) Unsettled() bool { return l.Settled == SettledNo }

// MollieExport is a parsed VPayDive export.
type MollieExport struct {
	Created        time.Time // workbook creation date, indicative; zero if absent
	Lines          []MollieLine
	Skipped        int            // lines without a name
	UnknownSettled map[string]int // « Payé » values other than Oui and Non
	PeriodFrom     time.Time      // earliest and latest « Date paiement »
	PeriodTo       time.Time
	FileHash       []byte // HMAC of the file, set by the caller (spec §7.6)
}

// ToCheck counts the lines of the « Paiements à vérifier » page (spec §7.7).
func (e *MollieExport) ToCheck() int {
	n := 0
	for _, l := range e.Lines {
		if l.Unsettled() {
			n++
		}
	}
	return n
}

// mollieColumns are the positions of the read columns; -1 when an optional
// one is absent. Date Fin, Mode Facturation, the commission, net and transfer
// columns and Statut API are never read.
type mollieColumns struct {
	last, first, product, amount, settled, paidAt int
	service, starts, method                       int
}

// ParseMollie reads a VPayDive export (spec §7.5). The header row is the
// first of the first ten rows holding a "Montant Panier" cell; columns are
// found by name. The whole file is validated before anything is returned.
// created is the workbook creation date; dates are read in loc.
func ParseMollie(rows []xlsx.Row, created time.Time, loc *time.Location) (*MollieExport, error) {
	hi, header, ok := xlsx.FindHeader(rows, columnMollieAmount, headerSearchRows)
	if !ok {
		return nil, &ParseError{Kind: ProblemNoHeader, Column: columnMollieAmount}
	}
	cols, err := findMollieColumns(header)
	if err != nil {
		return nil, err
	}
	exp := &MollieExport{Created: created, UnknownSettled: map[string]int{}}
	var badNumbers, badDates []int
	for _, row := range rows[hi+1:] {
		l, amountOK, paidAtOK := readMollieLine(row, cols, loc)
		if l.NameKey == "|" { // neither last nor first name
			exp.Skipped++
			continue
		}
		if !amountOK {
			badNumbers = append(badNumbers, row.Num)
		}
		if !paidAtOK {
			badDates = append(badDates, row.Num)
			continue
		}
		if l.Settled != SettledYes && l.Settled != SettledNo {
			exp.UnknownSettled[l.Settled]++
		}
		if exp.PeriodFrom.IsZero() || l.PaidAt.Before(exp.PeriodFrom) {
			exp.PeriodFrom = l.PaidAt
		}
		if l.PaidAt.After(exp.PeriodTo) {
			exp.PeriodTo = l.PaidAt
		}
		exp.Lines = append(exp.Lines, l)
	}
	switch {
	case len(badNumbers) > 0:
		return nil, &ParseError{Kind: ProblemInvalidNumber, Rows: badNumbers}
	case len(badDates) > 0:
		return nil, &ParseError{Kind: ProblemInvalidDate, Column: "Date paiement", Rows: badDates}
	}
	return exp, nil
}

func findMollieColumns(h xlsx.Header) (mollieColumns, error) {
	var c mollieColumns
	for _, r := range []struct {
		dst  *int
		name string
	}{
		{&c.last, "Nom"}, {&c.first, "Prénom"}, {&c.product, "Type Panier"},
		{&c.amount, columnMollieAmount}, {&c.settled, "Payé"}, {&c.paidAt, "Date paiement"},
	} {
		col, ok := h.Col(r.name, 0)
		if !ok {
			return mollieColumns{}, &ParseError{Kind: ProblemMissingColumn, Column: r.name}
		}
		*r.dst = col
	}
	c.service, c.starts, c.method = optionalCol(h, "Prestation", 0), optionalCol(h, "Date Début", 0), optionalCol(h, "Méthode", 0)
	return c, nil
}

// readMollieLine reads one named row; amountOK and paidAtOK report whether
// its amount and its payment date could be read.
func readMollieLine(row xlsx.Row, c mollieColumns, loc *time.Location) (l MollieLine, amountOK, paidAtOK bool) {
	l = MollieLine{
		Row:     row.Num,
		NameKey: secure.NameKey(row.Text(c.last), row.Text(c.first)),
		Product: row.Text(c.product),
		Service: row.Text(c.service),
		Settled: row.Text(c.settled),
		Method:  row.Text(c.method),
	}
	l.Amount, amountOK = amountAt(row, c.amount)
	l.PaidAt, paidAtOK = dateAt(row, c.paidAt, loc, "02/01/2006 15:04", "02/01/2006 15:04:05")
	l.Starts, _ = dateAt(row, c.starts, loc, "02/01/2006", "02/01/2006 15:04")
	return l, amountOK, paidAtOK
}
