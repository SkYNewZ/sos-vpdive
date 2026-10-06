package payments

import (
	"context"
	"slices"
	"strings"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
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
	ToSettle  []ToSettleLine
	Cancelled []Line
	Latest    []Line
}

// ToSettleLine is a line still to pay; a partial payment is a line to check
// (spec §7.7), with the dismissal of the resolver who checked it.
type ToSettleLine struct {
	Line

	Dismissal *Dismissal
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
	state, info, found, err := s.nameLines(ctx, nameHash)
	b.State, b.Import = state, info
	if err != nil || state != BlockLines {
		return b, err
	}
	lines := make([]Line, len(found))
	for i, r := range found {
		if lines[i], err = openLine[Line](s.keys, r.data); err != nil {
			return Block{}, err
		}
	}
	dismissed, err := dismissals(ctx, s.db)
	if err != nil {
		return Block{}, err
	}
	// Newest first; ties keep the file's order reversed, the later row first.
	slices.Reverse(lines)
	slices.SortStableFunc(lines, func(a, b Line) int { return b.Created.Compare(a.Created) })
	b.Latest = lines[:min(latestCount, len(lines))]
	for _, l := range lines {
		switch {
		case l.Balance():
			b.Balances = append(b.Balances, l)
		case l.ToSettle():
			t := ToSettleLine{Line: l}
			if l.Partial() {
				t.Dismissal = dismissal(dismissed, l.fingerprint(s.keys, nameHash))
			}
			b.ToSettle = append(b.ToSettle, t)
		case l.CancelledOuting():
			b.Cancelled = append(b.Cancelled, l)
		}
	}
	return b, nil
}
