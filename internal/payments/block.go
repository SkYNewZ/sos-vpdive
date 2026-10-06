package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// latestCount is how many of the requester's lines the block lists (spec §7.3).
const latestCount = 10

// BlockState says what the « Paiements VPDive » block of a request shows.
type BlockState string

// Block states, checked in this order.
const (
	BlockNoLines   BlockState = "no_lines"  // nothing ever imported
	BlockPurged    BlockState = "purged"    // lines deleted after 90 days without an import
	BlockNoMember  BlockState = "no_member" // the requester left the members list
	BlockAmbiguous BlockState = "ambiguous" // a name several members share, or marked so
	BlockEmpty     BlockState = "empty"     // no line for this member
	BlockLines     BlockState = "lines"
)

// Block is the « Paiements VPDive » block of a request page: what the last
// import says of the requester, never shown to members or sent to the model.
type Block struct {
	State     BlockState
	Import    imports.Info // the payments import in place
	Balances  []Line       // newest first, as every list
	ToSettle  []Line
	Cancelled []Line
	Latest    []Line
}

// Balance reports the remaining credit of a carnet or a training: due,
// negative, the credit being its absolute value (spec §7.1, §7.3).
func (l Line) Balance() bool {
	return l.State == StateDue && l.UnitPrice < 0 && (l.ProductType == TypeCard || l.ProductType == TypeTraining)
}

// ToSettle reports a line the member still has to pay.
func (l Line) ToSettle() bool {
	return (l.State == StateDue || l.State == StatePartial) && l.UnitPrice > 0
}

// CancelledOuting reports a paid line of an outing whose title says it is
// cancelled: the carnet is credited back when the outing is deleted (§7.4).
func (l Line) CancelledOuting() bool {
	return l.State == StatePaid && strings.Contains(strings.ToLower(l.Product), "annul")
}

// Prepaid reports a line settled with a carnet.
func (l Line) Prepaid() bool { return l.Method == MethodPrepaid }

// Settled is what a line cost the member: the unit price taken from a
// carnet, or the amount paid in real money (spec §7.4).
func (l Line) Settled() Amount {
	if l.Prepaid() {
		return l.UnitPrice
	}
	return l.Paid
}

// ProbableRefund reports a negative VPayDive payment outside carnets, shown
// as such until the treasurer confirms the rule (spec §14.1).
func (l Line) ProbableRefund() bool {
	return l.State == StatePaid && l.Method == MethodVPayDive && l.Paid < 0 && l.ProductType != TypeCard
}

// Block gathers the lines of nameHash, nil when the requester is not in the
// members list.
func (s *Store) Block(ctx context.Context, nameHash []byte) (Block, error) {
	var b Block
	inPlace, err := s.HasLines(ctx)
	if err != nil {
		return Block{}, err
	}
	var imported bool
	if b.Import, imported, err = s.LastImport(ctx); err != nil {
		return Block{}, err
	}
	if !inPlace {
		b.State = BlockNoLines
		if imported {
			b.State = BlockPurged
		}
		return b, nil
	}
	if nameHash == nil {
		b.State = BlockNoMember
		return b, nil
	}
	var members int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM members WHERE name_hash = ?`, nameHash).Scan(&members); err != nil {
		return Block{}, fmt.Errorf("members of a name: %w", err)
	}
	found, err := sealedLines(ctx, s.db, `SELECT name_hash, ambiguous, data FROM payment_lines WHERE name_hash = ? ORDER BY id DESC`, nameHash)
	if err != nil {
		return Block{}, err
	}
	switch {
	case members > 1 || slices.ContainsFunc(found, func(r sealedLine) bool { return r.ambiguous }):
		b.State = BlockAmbiguous
		return b, nil
	case len(found) == 0:
		b.State = BlockEmpty
		return b, nil
	}
	lines := make([]Line, len(found))
	for i, r := range found {
		if lines[i], err = openLine[Line](s.keys, r.data); err != nil {
			return Block{}, err
		}
	}
	// Newest first; read in reverse id order, so ties keep the file's order reversed.
	slices.SortStableFunc(lines, func(a, b Line) int { return b.Created.Compare(a.Created) })
	b.State = BlockLines
	b.Latest = lines[:min(latestCount, len(lines))]
	for _, l := range lines {
		switch {
		case l.Balance():
			b.Balances = append(b.Balances, l)
		case l.ToSettle():
			b.ToSettle = append(b.ToSettle, l)
		case l.CancelledOuting():
			b.Cancelled = append(b.Cancelled, l)
		}
	}
	return b, nil
}

// sealedLine is a stored payment or Mollie line before decryption.
type sealedLine struct {
	nameHash  []byte
	ambiguous bool
	data      []byte
}

// sealedLines reads stored lines; query selects name_hash, ambiguous and
// data.
func sealedLines(ctx context.Context, db *sql.DB, query string, args ...any) ([]sealedLine, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	found, err := store.Collect(rows, err, func(rows *sql.Rows) (r sealedLine, err error) {
		err = rows.Scan(&r.nameHash, &r.ambiguous, &r.data)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("read lines: %w", err)
	}
	return found, nil
}

// openLine decrypts and decodes one stored line.
func openLine[T Line | MollieLine](keys *secure.Keys, sealed []byte) (T, error) {
	var l T
	plain, err := keys.Open(sealed)
	if err != nil {
		return l, fmt.Errorf("decrypt line: %w", err)
	}
	if err := json.Unmarshal(plain, &l); err != nil {
		return l, fmt.Errorf("decode line: %w", err)
	}
	return l, nil
}
