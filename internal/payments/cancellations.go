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
	Imported bool         // an import is in place, possibly with no line
	Purged   bool         // its lines were deleted after 90 days without an import
	Import   imports.Info // the latest payments import
	Outings  []Outing     // oldest first, unknown dates last
	Lines    int
	Persons  int // inscriptions: distinct payers per outing, summed
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
	var (
		c        Cancellations
		imported bool
	)
	if c.Import, imported, err = s.LastImport(ctx); err != nil {
		return Cancellations{}, err
	}
	c.Purged = imported && len(found) == 0 && s.Expired(c.Import)
	c.Imported = imported && !c.Purged
	type key struct {
		title  string
		starts int64
	}
	index := map[key]int{}
	var payers []map[string]bool // by outing index
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
			i, index[k] = len(c.Outings), len(c.Outings)
			c.Outings = append(c.Outings, Outing{Title: l.Product, Starts: l.Starts})
			payers = append(payers, map[string]bool{})
		}
		o := &c.Outings[i]
		o.Lines++
		if l.Prepaid() {
			o.ByCarnet += l.UnitPrice
		} else {
			o.ByMoney += l.Paid
		}
		payers[i][string(r.nameHash)] = true
	}
	for i := range c.Outings {
		c.Outings[i].Persons = len(payers[i])
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
