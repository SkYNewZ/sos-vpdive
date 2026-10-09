// Package carnets holds the carnet carts the external script pushes from
// VPDive's payments page (design 2026-10-09): their reading, the holder
// found through the members list, their storage like the payment lines and
// their interpretation for the request page and the committee assistant.
package carnets

import (
	"encoding/json"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// Actions the reading knows, as VPDive writes them.
const (
	actionPrepaid = "prépaye"
	actionComment = "Commentaire"
)

// decimal is the amount of a cart as pushed: a dot decimal, maybe without a
// fraction.
var decimal = regexp.MustCompile(`^-?\d+(?:\.\d+)?$`)

// Entry is a line of a cart's history, as VPDive shows it.
type Entry struct {
	Action string `json:"action"`
	At     string `json:"at"`     // RFC 3339; empty for a comment
	By     string `json:"by"`     // « Prénom Nom », as written
	Detail string `json:"detail"` // the tooltip, or the text of a comment
}

// Card is a carnet cart. It is stored as sealed JSON; its holder is the
// name_hash column, never in the data.
type Card struct {
	Title   string          `json:"title"`
	Status  string          `json:"status,omitempty"`
	Method  string          `json:"method,omitempty"`
	Amount  payments.Amount `json:"amount"`  // negative while credit is left
	Entries []Entry         `json:"entries"` // newest first, as VPDive lists them

	member  string // « NOM Prénom » as pushed: never stored
	nameKey string // the holder Resolve found: hashed at import, never stored
	rank    int    // place of the cart in the push, from 1
}

// Origin returns the place of the card in the push and its holder's name key.
func (c Card) Origin() (int, string) { return c.rank, c.nameKey }

// isCard reports a carnet: an « avoir » cart of negative amount, or one a
// dive was taken from. Purchases and cancelled purchases are not: the
// payments export has them.
func (c Card) isCard() bool {
	return c.Amount < 0 || slices.ContainsFunc(c.Entries, func(e Entry) bool { return isPrepaid(e.Action) })
}

// Export is a pushed list of carts, read and validated.
type Export struct {
	From, To time.Time // first and last day of the window, midnight in Paris
	Cards    []Card    // the cards; after Resolve, those of exactly one member
	Read     int       // carts in the push
	Skipped  int       // carts that are no card
	ToCheck  int       // cards Resolve could not attach to one member
	FileHash []byte    // HMAC of the body, set by the caller (spec §7.6)
}

// resolve keeps, in push order, the cards whose holder known designates and
// counts the others in ToCheck.
func (e *Export) resolve(known func(nameKey string) bool) {
	kept := e.Cards[:0]
	for _, c := range e.Cards {
		if c.nameKey = holder(c.member, known); c.nameKey == "" {
			e.ToCheck++
			continue
		}
		kept = append(kept, c)
	}
	e.Cards = kept
}

// ProblemKind classifies a refused push.
type ProblemKind string

// Reasons to refuse a push. The pushed route answers invalid_carnets for all
// of them.
const (
	ProblemUnreadable ProblemKind = "unreadable" // not JSON of the expected shape
	ProblemWindow     ProblemKind = "window"     // from or to missing, unreadable or reversed
	ProblemCart       ProblemKind = "cart"       // a cart without member or title, or its amount no dot decimal
	ProblemEntries    ProblemKind = "entries"    // no entry, an entry without action, or a line outside comments not dated in RFC 3339
)

// ParseError refuses a push. Cart is the rank of the cart in cause, from 1,
// or 0 for the whole push. It quotes nothing the push holds.
type ParseError struct {
	Kind ProblemKind
	Cart int
}

func (e *ParseError) Error() string {
	if e.Cart > 0 {
		return "carnets refused: " + string(e.Kind) + " (cart " + strconv.Itoa(e.Cart) + ")"
	}
	return "carnets refused: " + string(e.Kind)
}

// Parse reads a pushed list of carts (design §2). The window is made of
// Paris days; the carts that are no card count in Skipped. Unknown fields
// are ignored.
func Parse(data []byte, paris *time.Location) (*Export, error) {
	var doc struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Carts []struct {
			Member  string  `json:"member"`
			Title   string  `json:"title"`
			Status  string  `json:"status"`
			Method  string  `json:"method"`
			Amount  string  `json:"amount"`
			Entries []Entry `json:"entries"`
		} `json:"carts"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, &ParseError{Kind: ProblemUnreadable}
	}
	from, errFrom := time.ParseInLocation(time.DateOnly, doc.From, paris)
	to, errTo := time.ParseInLocation(time.DateOnly, doc.To, paris)
	if errFrom != nil || errTo != nil || to.Before(from) {
		return nil, &ParseError{Kind: ProblemWindow}
	}
	exp := &Export{From: from, To: to, Read: len(doc.Carts)}
	for i, c := range doc.Carts {
		amount, ok := parseAmount(strings.TrimSpace(c.Amount))
		if strings.TrimSpace(c.Member) == "" || strings.TrimSpace(c.Title) == "" || !ok {
			return nil, &ParseError{Kind: ProblemCart, Cart: i + 1}
		}
		if !validEntries(c.Entries) {
			return nil, &ParseError{Kind: ProblemEntries, Cart: i + 1}
		}
		card := Card{Title: c.Title, Status: c.Status, Method: c.Method, Amount: amount, Entries: c.Entries, member: c.Member, rank: i + 1}
		if !card.isCard() {
			exp.Skipped++
			continue
		}
		exp.Cards = append(exp.Cards, card)
	}
	return exp, nil
}

// validEntries reports a history VPDive could show: at least one line, each
// with an action, each dated in RFC 3339 but the comments. An empty history
// is refused: replacing the cards with it would erase theirs.
func validEntries(es []Entry) bool {
	if len(es) == 0 {
		return false
	}
	for _, e := range es {
		if clean(e.Action) == "" {
			return false
		}
		if isComment(e.Action) {
			continue
		}
		if _, err := time.Parse(time.RFC3339, e.At); err != nil {
			return false
		}
	}
	return true
}

// parseAmount reads a dot decimal as hundredths, rounded half away from zero.
func parseAmount(s string) (payments.Amount, bool) {
	if !decimal.MatchString(s) {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(r.Mul(r, big.NewRat(100, 1)).FloatString(0), 10, 64)
	return payments.Amount(n), err == nil
}

// holder is the name key of the one member a split of name designates
// (design §2): « W1 … Wn » gives n−1 splits into last (W1…Wk) and first
// names (Wk+1…Wn), keyed by secure.NameKey, so case and accents play no
// role. "" when no split is known, or when two splits give two names.
func holder(name string, known func(nameKey string) bool) string {
	words := strings.Fields(name)
	found := ""
	for k := 1; k < len(words); k++ {
		key := secure.NameKey(strings.Join(words[:k], " "), strings.Join(words[k:], " "))
		if !known(key) || key == found {
			continue
		}
		if found != "" {
			return ""
		}
		found = key
	}
	return found
}

// clean is a text as compared: composed (NFC), white space runs as one
// space, trimmed. A no-break space is white space.
func clean(s string) string { return strings.Join(strings.Fields(norm.NFC.String(s)), " ") }

// isComment reports the action of a committee comment.
func isComment(action string) bool { return strings.EqualFold(clean(action), actionComment) }

// isPrepaid reports the action of a dive taken from, or given back to, a card.
func isPrepaid(action string) bool { return strings.EqualFold(clean(action), actionPrepaid) }
