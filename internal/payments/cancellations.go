package payments

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Outing is a cancelled outing whose paid lines wait for its deletion in
// VPDive (spec §7.4). It is identified by its title and its « Du ».
type Outing struct {
	Title    string
	Starts   time.Time // zero when the export gives no date
	Persons  int       // distinct payers: a dive paid on two carnets counts once
	Lines    int
	ByCarnet Amount // « Prix unitaire » of the « Prépayé » lines
	ByMoney  Amount // « Montant paiement » of the other lines
}

// Cancellations is the treasurer's work list of cancelled outings.
type Cancellations struct {
	InPlace bool         // false when no payment line is in place
	Import  imports.Info // the payments import in place
	Outings []Outing     // oldest first, unknown dates last
	Lines   int
	Persons int // inscriptions: distinct payers per outing, summed
}

// Cancellations lists the cancelled outings still holding paid lines,
// computed from the lines in place, names never read.
//
// ponytail: decrypts every line at each call (a few thousand, a few ms); store
// a clear "cancelled outing" flag at import if it ever shows in traces.
func (s *Store) Cancellations(ctx context.Context) (Cancellations, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name_hash, data FROM payment_lines ORDER BY id`)
	type row struct{ nameHash, data []byte }
	found, err := store.Collect(rows, err, func(rows *sql.Rows) (r row, err error) {
		err = rows.Scan(&r.nameHash, &r.data)
		return r, err
	})
	if err != nil {
		return Cancellations{}, fmt.Errorf("payment lines: %w", err)
	}
	var c Cancellations
	if len(found) == 0 {
		return c, nil
	}
	c.InPlace = true
	if c.Import, _, err = s.LastImport(ctx); err != nil {
		return Cancellations{}, err
	}
	type key struct {
		title  string
		starts int64
	}
	index := map[key]int{}
	payers := map[key]map[string]bool{}
	for _, r := range found {
		l, err := s.open(r.data)
		if err != nil {
			return Cancellations{}, err
		}
		if !l.CancelledOuting() {
			continue
		}
		k := key{l.Product, l.Starts.Unix()}
		i, ok := index[k]
		if !ok {
			i = len(c.Outings)
			index[k], payers[k] = i, map[string]bool{}
			c.Outings = append(c.Outings, Outing{Title: l.Product, Starts: l.Starts})
		}
		o := &c.Outings[i]
		o.Lines++
		if l.Prepaid() {
			o.ByCarnet += l.UnitPrice
		} else {
			o.ByMoney += l.Paid
		}
		payers[k][string(r.nameHash)] = true
		o.Persons = len(payers[k])
	}
	slices.SortFunc(c.Outings, func(a, b Outing) int {
		switch {
		case a.Starts.IsZero() != b.Starts.IsZero():
			if a.Starts.IsZero() {
				return 1
			}
			return -1
		case !a.Starts.Equal(b.Starts):
			return a.Starts.Compare(b.Starts)
		}
		return strings.Compare(a.Title, b.Title)
	})
	for _, o := range c.Outings {
		c.Lines += o.Lines
		c.Persons += o.Persons
	}
	return c, nil
}
