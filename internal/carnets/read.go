package carnets

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
)

// Kind is what a history line is, read at display time (design §4): the raw
// lines are stored, so reading them better needs no new push.
type Kind string

// Line kinds.
const (
	KindDebit    Kind = "debit"    // a dive taken from the card
	KindRecredit Kind = "recredit" // a dive given back to the card
	KindPrice    Kind = "price"    // the card's price changed
	KindComment  Kind = "comment"  // a committee comment
	KindOther    Kind = "other"    // anything else, shown as pushed
)

// euroAmount is an amount as a tooltip writes it: « 25 », « 25,50 », « 1 250,00 ».
const euroAmount = `\d+(?: \d{3})*(?:,\d+)?`

var (
	// movement is a debit, « prépaye Sortie Épave (N2) (14/06/2026) -25€ », or a
	// recredit, « Sortie Épave (14/06/2026) +25€ »: the outing is the text
	// before the last date before the amount. Tooltips are cleaned first.
	movement = regexp.MustCompile(`^(?:(?i:prépaye) )?(.+) \((\d{2}/\d{2}/\d{4})\) ([+-])(` + euroAmount + `) ?€$`)
	// priceChange is « -125,00 € -> -100,00 € ».
	priceChange = regexp.MustCompile(`(-?` + euroAmount + `) ?€ ?-> ?(-?` + euroAmount + `) ?€$`)
	// signedEuros ends a tooltip that moves money: one no form reads is unread.
	signedEuros = regexp.MustCompile(`[+-]` + euroAmount + ` ?€$`)
	// qualifier is what a title ends with besides its name: « (N2) ».
	qualifier = regexp.MustCompile(`(?:\s*\([^()]*\))+$`)
)

// Line is a history line as read (design §4).
type Line struct {
	Entry

	Kind     Kind
	Time     time.Time       // At read; zero for a comment
	Outing   string          // debit, recredit: the outing's title
	Date     string          // debit, recredit: the outing's date, DD/MM/YYYY as written
	Amount   payments.Amount // debit negative, recredit positive
	From, To payments.Amount // price change
	Unread   bool            // a money line no form reads: the card's totals are partial
	Split    payments.Amount // a dive taken from two cards of the person: the sum of its parts
	Unusual  bool            // a debit off the card's usual amount: to check, not a cause
}

// View is a card as the request page and the assistant read it.
type View struct {
	Card

	Lines      []Line // in VPDive's order, newest first
	Debits     int
	Debited    payments.Amount // the debits, as a positive sum
	Recredits  int
	Recredited payments.Amount
	Unread     int // above zero, the totals are partial
	Unusual    int
	latest     time.Time // the newest dated line
}

// Net is what the card paid for dives: debits less recredits.
func (v View) Net() payments.Amount { return v.Debited - v.Recredited }

// Partial reports totals missing unread lines: no expected balance comes from
// them.
func (v View) Partial() bool { return v.Unread > 0 }

// Block is the « Cartes VPDive » block of a request page: the requester's
// cards as the latest push gave them, read (design §6). Never shown to
// members; the assistant reads it, masked, through member_payments.
type Block struct {
	State  payments.BlockState
	Import imports.Info // the latest push
	Cards  []View       // newest first
}

// Block reads the cards of nameHash, nil when the requester is not in the
// members list.
func (s *Store) Block(ctx context.Context, nameHash []byte) (Block, error) {
	state, info, cards, err := s.NameLines(ctx, nameHash)
	if err != nil {
		return Block{}, err
	}
	return Block{State: state, Import: info, Cards: Read(cards)}, nil
}

// Read interprets the cards of one person, newest first (design §4).
func Read(cards []Card) []View {
	views := make([]View, len(cards))
	for i, c := range cards {
		views[i] = readCard(c)
	}
	markSplit(views)
	for i := range views {
		markUnusual(&views[i])
	}
	slices.SortStableFunc(views, func(a, b View) int { return b.latest.Compare(a.latest) })
	return views
}

// readCard reads each line of c and adds up its debits and recredits.
func readCard(c Card) View {
	v := View{Card: c, Lines: make([]Line, len(c.Entries))}
	for i, e := range c.Entries {
		l := readEntry(e)
		v.Lines[i] = l
		if l.Time.After(v.latest) {
			v.latest = l.Time
		}
		if l.Unread {
			v.Unread++
		}
		switch l.Kind {
		case KindDebit:
			v.Debits++
			v.Debited -= l.Amount
		case KindRecredit:
			v.Recredits++
			v.Recredited += l.Amount
		case KindPrice, KindComment, KindOther:
		}
	}
	return v
}

// readEntry reads one history line (design §4, table « Ligne »).
func readEntry(e Entry) Line {
	l := Line{Entry: e, Kind: KindOther}
	if t, err := time.Parse(time.RFC3339, e.At); err == nil {
		l.Time = t // a comment has none
	}
	detail := clean(e.Detail)
	switch {
	case isComment(e.Action):
		l.Kind = KindComment
	case isPrepaid(e.Action):
		l.Unread = !readMovement(&l, detail)
	default:
		l.Unread = !readPrice(&l, detail) && signedEuros.MatchString(detail)
	}
	return l
}

// readMovement reads a debit or a recredit into l; false when detail is
// neither.
func readMovement(l *Line, detail string) bool {
	m := movement.FindStringSubmatch(detail)
	if m == nil {
		return false
	}
	n, ok := frenchAmount(m[4])
	if !ok {
		return false
	}
	l.Kind, l.Outing, l.Date, l.Amount = KindRecredit, m[1], m[2], n
	if m[3] == "-" {
		l.Kind, l.Amount = KindDebit, -n
	}
	return true
}

// readPrice reads a price change into l; false when detail is none.
func readPrice(l *Line, detail string) bool {
	m := priceChange.FindStringSubmatch(detail)
	if m == nil {
		return false
	}
	from, okFrom := frenchAmount(m[1])
	to, okTo := frenchAmount(m[2])
	if !okFrom || !okTo {
		return false
	}
	l.Kind, l.From, l.To = KindPrice, from, to
	return true
}

// frenchAmount reads « 25 », « -25,50 » or « 1 250,00 » as hundredths.
func frenchAmount(s string) (payments.Amount, bool) {
	return parseAmount(strings.ReplaceAll(strings.Replace(s, ",", ".", 1), " ", ""))
}

// markSplit marks the debits of a dive taken from two cards or more of the
// person (design §4): an outing, by normalised title and date, with a net
// debit on each card. A dive given back on a card and debited on another
// moved: it is not split. Each part carries the sum of the parts.
func markSplit(views []View) {
	type outing struct{ title, date string }
	// ponytail: two same-day outings differing only by a qualifier merge into
	// one key.
	key := func(l Line) outing {
		return outing{qualifier.ReplaceAllString(strings.ToLower(clean(l.Outing)), ""), l.Date}
	}
	net := map[outing][]payments.Amount{} // per outing, the net debit of each card
	for i, v := range views {
		for _, l := range v.Lines {
			if l.Kind != KindDebit && l.Kind != KindRecredit {
				continue
			}
			k := key(l)
			if net[k] == nil {
				net[k] = make([]payments.Amount, len(views))
			}
			net[k][i] -= l.Amount
		}
	}
	for i := range views {
		for j, l := range views[i].Lines {
			if l.Kind != KindDebit {
				continue
			}
			parts := net[key(l)]
			if parts[i] <= 0 {
				continue
			}
			var sum payments.Amount
			cards := 0
			for _, p := range parts {
				if p > 0 {
					sum, cards = sum+p, cards+1
				}
			}
			if cards > 1 {
				views[i].Lines[j].Split = sum
			}
		}
	}
}

// markUnusual flags the debits off the card's usual amount (design §4): the
// most frequent amount of its debits not split over two cards, when it comes
// twice or more and alone on top.
//
// ponytail: a heuristic; it says nothing of a card with a single debit, and a
// price grid per product would replace it if the need shows.
func markUnusual(v *View) {
	counts := map[payments.Amount]int{}
	for _, l := range v.Lines {
		if l.Kind == KindDebit && l.Split == 0 {
			counts[l.Amount]++
		}
	}
	var usual payments.Amount
	top, tie := 0, false
	for a, n := range counts {
		switch {
		case n > top:
			usual, top, tie = a, n, false
		case n == top:
			tie = true
		}
	}
	if top < 2 || tie {
		return
	}
	for i := range v.Lines {
		if l := &v.Lines[i]; l.Kind == KindDebit && l.Split == 0 && l.Amount != usual {
			l.Unusual = true
			v.Unusual++
		}
	}
}
