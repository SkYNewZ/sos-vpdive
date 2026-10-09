package payments

import (
	"context"
	"slices"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
)

// MollieBlock is the « Encaissements Mollie » block of a request page: what
// Mollie collected from the requester and whether VPDive settled it (spec
// §7.5). Never shown to members. The committee assistant reads it, masked,
// through its member_payments and member_outings tools.
type MollieBlock struct {
	State    BlockState
	Import   imports.Info    // the VPayDive import in place
	Payments []MolliePayment // newest first
}

// MolliePayment is one payment rebuilt from the export: the lines of one
// person at one minute (spec §7.5).
type MolliePayment struct {
	PaidAt time.Time
	Method string // the method of its first line
	Total  Amount
	Lines  []CollectedLine // in the order of the file
}

// CollectedLine is a line of a Mollie payment; a line not settled in VPDive
// is a line to check, with the dismissal of the resolver who checked it.
type CollectedLine struct {
	MollieLine

	Dismissal *Dismissal
}

// Negative reports a refund or a discount: the export does not tell which
// (spec §14.1).
func (l MollieLine) Negative() bool { return l.Amount < 0 }

// Block gathers the Mollie payments of nameHash, nil when the requester is
// not in the members list.
func (s *MollieStore) Block(ctx context.Context, nameHash []byte) (MollieBlock, error) {
	var b MollieBlock
	state, info, found, err := s.NameLines(ctx, nameHash)
	b.State, b.Import = state, info
	if err != nil || state != BlockLines {
		return b, err
	}
	dismissed, err := dismissals(ctx, s.db)
	if err != nil {
		return MollieBlock{}, err
	}
	lines := make([]CollectedLine, len(found))
	for i, l := range found {
		lines[i].MollieLine = l
		if l.Unsettled() {
			lines[i].Dismissal = dismissal(dismissed, l.fingerprint(s.keys, nameHash))
		}
	}
	// Newest first; read in id order, so the lines of a payment keep the file's order.
	slices.SortStableFunc(lines, func(a, b CollectedLine) int { return b.PaidAt.Compare(a.PaidAt) })
	for _, l := range lines {
		if n := len(b.Payments); n > 0 && b.Payments[n-1].PaidAt.Equal(l.PaidAt) {
			p := &b.Payments[n-1]
			p.Total += l.Amount
			p.Lines = append(p.Lines, l)
			continue
		}
		b.Payments = append(b.Payments, MolliePayment{PaidAt: l.PaidAt, Method: l.Method, Total: l.Amount, Lines: []CollectedLine{l}})
	}
	return b, nil
}
