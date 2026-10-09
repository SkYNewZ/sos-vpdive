package carnets

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
)

func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	return loc
}

// fixture is testdata/fixtures/carnets_valid.json: the cards of the members
// of members_valid.xlsx, a purchase, and two holders no member matches.
func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(memberstest.FixturePath("carnets_valid.json"))
	require.NoError(t, err)
	return data
}

// pushOf is a push of carts over the fixture's window.
func pushOf(t *testing.T, carts ...map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"from": "2024-09-02", "to": "2026-09-02", "carts": carts})
	require.NoError(t, err)
	return data
}

// cart is a card of member; no entries encode as null.
func cart(member, amount string, entries ...map[string]any) map[string]any {
	return map[string]any{"member": member, "title": "Carte 10 plongées niveau 1 et 2", "status": "Reste à payer",
		"method": "", "amount": amount, "entries": entries}
}

// line is an entry written by Hugo Bernard.
func line(action, at, detail string) map[string]any {
	return map[string]any{"action": action, "at": at, "by": "Hugo BERNARD", "detail": detail}
}

// added is the first line of every VPDive cart.
func added() map[string]any { return line("Ajout au panier", "2026-05-01T09:00:00+02:00", "") }

func TestParseValidPush(t *testing.T) {
	exp, err := Parse(fixture(t), paris(t))
	require.NoError(t, err)
	assert.True(t, time.Date(2024, 9, 2, 0, 0, 0, 0, paris(t)).Equal(exp.From))
	assert.True(t, time.Date(2026, 9, 2, 0, 0, 0, 0, paris(t)).Equal(exp.To))
	assert.Equal(t, 7, exp.Read)
	assert.Equal(t, 1, exp.Skipped, "the purchase of a card is no card")
	require.Len(t, exp.Cards, 6)
	first := exp.Cards[0]
	assert.Equal(t, "Carte 10 plongées niveau 1 et 2", first.Title)
	assert.Equal(t, "Reste à payer", first.Status)
	assert.Equal(t, payments.Amount(-12000), first.Amount)
	assert.Equal(t, "BERNARD Hugo", first.member)
	assert.Len(t, first.Entries, 8)
	assert.Equal(t, Entry{Action: "Commentaire", Detail: "Débit de la plongée de nuit à revoir avec le trésorier"}, first.Entries[0])
	assert.Equal(t, 1, first.rank)
	assert.Equal(t, 7, exp.Cards[5].rank, "ranks count every cart, the skipped one included")
}

// Design §2: a cart is a card when its amount is negative or a dive was
// taken from it.
func TestParseSortsCardsFromPurchases(t *testing.T) {
	debit := line("prépaye", "2026-06-02T18:30:00+02:00", "prépaye Sortie Épave (14/06/2026) -25€")
	paid := line("Payer", "2026-05-01T09:05:00+02:00", "Paiement en ligne")
	exp, err := Parse(pushOf(t,
		cart("BERNARD Hugo", "-75", added()),
		cart("BERNARD Hugo", "0", debit, added()),
		cart("BERNARD Hugo", "300", paid, added()),
		cart("BERNARD Hugo", "0", line("Annuler", "2026-05-02T09:00:00+02:00", ""), added()),
	), paris(t))
	require.NoError(t, err)
	require.Len(t, exp.Cards, 2, "credit left, and a used-up card a dive was taken from")
	assert.Equal(t, payments.Amount(-7500), exp.Cards[0].Amount)
	assert.Equal(t, 2, exp.Cards[1].rank)
	assert.Equal(t, 2, exp.Skipped, "a purchase and a cancelled purchase")
}

func TestParseAcceptsUndatedCommentsAndPlainAmounts(t *testing.T) {
	exp, err := Parse(pushOf(t,
		cart("BERNARD Hugo", "-75", line("Commentaire", "", "rattrapage"), added()),
		cart("BERNARD Hugo", " -75.5 ", added()),
	), paris(t))
	require.NoError(t, err)
	require.Len(t, exp.Cards, 2)
	assert.Equal(t, payments.Amount(-7500), exp.Cards[0].Amount)
	assert.Equal(t, payments.Amount(-7550), exp.Cards[1].Amount)
}

// Design §2: every refusal names at most the rank of the cart.
func TestParseRefusals(t *testing.T) {
	untitled := cart("BERNARD Hugo", "-75", added())
	untitled["title"] = " "
	absent := cart("BERNARD Hugo", "-75")
	delete(absent, "entries")
	empty := cart("BERNARD Hugo", "-75")
	empty["entries"] = []any{}
	for name, tc := range map[string]struct {
		data []byte
		want ParseError
	}{
		"not JSON":             {[]byte(`{"from": "2024-09-02"`), ParseError{Kind: ProblemUnreadable}},
		"amount as a number":   {[]byte(`{"from": "2024-09-02", "to": "2026-09-02", "carts": [{"member": "X Y", "title": "T", "amount": -75}]}`), ParseError{Kind: ProblemUnreadable}},
		"no window":            {[]byte(`{"carts": []}`), ParseError{Kind: ProblemWindow}},
		"reversed window":      {[]byte(`{"from": "2026-09-02", "to": "2024-09-02", "carts": []}`), ParseError{Kind: ProblemWindow}},
		"no member":            {pushOf(t, cart(" ", "-75", added())), ParseError{Kind: ProblemCart, Cart: 1}},
		"no title":             {pushOf(t, cart("BERNARD Hugo", "-75", added()), untitled), ParseError{Kind: ProblemCart, Cart: 2}},
		"decimal comma":        {pushOf(t, cart("BERNARD Hugo", "-75,50", added())), ParseError{Kind: ProblemCart, Cart: 1}},
		"amount in words":      {pushOf(t, cart("BERNARD Hugo", "moins 75", added())), ParseError{Kind: ProblemCart, Cart: 1}},
		"entries absent":       {pushOf(t, absent), ParseError{Kind: ProblemEntries, Cart: 1}},
		"entries null":         {pushOf(t, cart("BERNARD Hugo", "-75")), ParseError{Kind: ProblemEntries, Cart: 1}},
		"entries empty":        {pushOf(t, empty), ParseError{Kind: ProblemEntries, Cart: 1}},
		"entry without action": {pushOf(t, cart("BERNARD Hugo", "-75", line(" ", "2026-05-01T09:00:00+02:00", ""))), ParseError{Kind: ProblemEntries, Cart: 1}},
		"French date":          {pushOf(t, cart("BERNARD Hugo", "-75", line("Ajout au panier", "01/05/2026 09:00", ""))), ParseError{Kind: ProblemEntries, Cart: 1}},
		"dated line, no date":  {pushOf(t, cart("BERNARD Hugo", "-75", line("Ajout au panier", "", ""))), ParseError{Kind: ProblemEntries, Cart: 1}},
	} {
		_, err := Parse(tc.data, paris(t))
		var pe *ParseError
		require.ErrorAs(t, err, &pe, name)
		assert.Equal(t, tc.want, *pe, name)
		assert.NotContains(t, pe.Error(), "Hugo", "a refusal quotes nothing the push holds")
	}
}

// Design §2: every split of « W1 … Wn » into last and first names is tried;
// exactly one name a member bears attaches the card. Review Focus 3.
func TestHolderIsTheOneMemberASplitNames(t *testing.T) {
	members := map[string]bool{"martin|jeanne": true, "martinjean|paul": true, "martin|jeanpaul": true, "bernard|hugo": true}
	known := func(key string) bool { return members[key] }
	for name, want := range map[string]string{
		"MARTIN Jeanne":     "martin|jeanne",
		"MARTIN JEANNE":     "martin|jeanne", // the case plays no role
		"MARTIN - Jeanne":   "martin|jeanne", // two splits, one name
		"  BERNARD   Hugo ": "bernard|hugo",
		"MARTIN JEAN Paul":  "", // two members, two names
		"DURAND Noé":        "", // nobody
		"LEROY":             "", // one word: no split
		" ":                 "",
	} {
		assert.Equal(t, want, holder(name, known), name)
	}
}

func TestResolveKeepsTheCardsOfOneMember(t *testing.T) {
	exp, err := Parse(fixture(t), paris(t))
	require.NoError(t, err)
	members := map[string]bool{"bernard|hugo": true, "martin|lea": true, "petit|chloe": true, "leroy|": true}
	exp.resolve(func(key string) bool { return members[key] })
	assert.Equal(t, 2, exp.ToCheck, "INCONNU Paul matches nobody; LEROY, one word, has no split")
	holders := make([]string, len(exp.Cards))
	for i, c := range exp.Cards {
		_, holders[i] = c.Origin()
	}
	assert.Equal(t, []string{"bernard|hugo", "bernard|hugo", "martin|lea", "petit|chloe"}, holders, "push order kept")
}
