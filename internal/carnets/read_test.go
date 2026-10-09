package carnets

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/payments"
)

// debit is a dive of amount euros taken from a card; outing carries its date.
func debit(at, outing, amount string) Entry {
	return Entry{Action: "prépaye", At: at, By: "Hugo BERNARD", Detail: "prépaye " + outing + " -" + amount + "€"}
}

// recredit is a dive of amount euros given back to a card.
func recredit(at, outing, amount string) Entry {
	return Entry{Action: "prépaye", At: at, By: "Léa MARTIN", Detail: outing + " +" + amount + "€"}
}

// Design §4: the five forms, and the lines no form reads. Review Focus 2.
func TestReadEntryForms(t *testing.T) {
	const at = "2026-06-02T18:30:00+02:00"
	for name, tc := range map[string]struct {
		entry Entry
		want  Line
	}{
		"debit": {Entry{Action: "prépaye", At: at, Detail: "prépaye Sortie Épave (N2) (14/06/2026) -25€"},
			Line{Kind: KindDebit, Outing: "Sortie Épave (N2)", Date: "14/06/2026", Amount: -2500}},
		"recredit": {Entry{Action: "prépaye", At: at, Detail: "Sortie Épave (14/06/2026) +25€"},
			Line{Kind: KindRecredit, Outing: "Sortie Épave", Date: "14/06/2026", Amount: 2500}},
		"capitalised tooltip": {Entry{Action: "prépaye", At: at, Detail: "Prépaye Sortie Cap Garonne (13/06/2026) -30€"},
			Line{Kind: KindDebit, Outing: "Sortie Cap Garonne", Date: "13/06/2026", Amount: -3000}},
		"cents and a space": {Entry{Action: "prépaye", At: at, Detail: "prépaye Sortie (14/06/2026) -25,50 €"},
			Line{Kind: KindDebit, Outing: "Sortie", Date: "14/06/2026", Amount: -2550}},
		"no-break space, capital, double space": {Entry{Action: "Prépaye", At: at, Detail: "prépaye Sortie  Épave (14/06/2026) -25\u00a0€"},
			Line{Kind: KindDebit, Outing: "Sortie Épave", Date: "14/06/2026", Amount: -2500}},
		"decomposed accent": {Entry{Action: "pre\u0301paye", At: at, Detail: "pre\u0301paye Sortie (14/06/2026) -25€"},
			Line{Kind: KindDebit, Outing: "Sortie", Date: "14/06/2026", Amount: -2500}},
		"price change": {Entry{Action: "Modification du prix", At: at, Detail: "-125,00 € -> -100,00 €"},
			Line{Kind: KindPrice, From: -12500, To: -10000}},
		"comment":         {Entry{Action: "Commentaire", Detail: "rattrapage"}, Line{Kind: KindComment}},
		"other":           {Entry{Action: "Ajout au panier", At: at}, Line{Kind: KindOther}},
		"other with text": {Entry{Action: "Payer", At: at, Detail: "Paiement en ligne"}, Line{Kind: KindOther}},
		"prépaye, unread": {Entry{Action: "prépaye", At: at, Detail: "prépaye Sortie Levant -30€"}, Line{Kind: KindOther, Unread: true}},
		"money, unread":   {Entry{Action: "Rembourser", At: at, Detail: "Remboursement partiel +30€"}, Line{Kind: KindOther, Unread: true}},
	} {
		got := readEntry(tc.entry)
		if tc.entry.At != "" {
			assert.True(t, time.Date(2026, 6, 2, 16, 30, 0, 0, time.UTC).Equal(got.Time), name)
		} else {
			assert.True(t, got.Time.IsZero(), name)
		}
		got.Time, tc.want.Entry = time.Time{}, tc.entry
		assert.Equal(t, tc.want, got, name)
	}
}

func TestReadTotalsAndPartialTotals(t *testing.T) {
	v := Read([]Card{{Title: "Carte 10", Amount: -5000, Entries: []Entry{
		debit("2026-06-03T10:00:00+02:00", "Sortie A (14/06/2026)", "25"),
		recredit("2026-06-02T10:00:00+02:00", "Sortie B (07/06/2026)", "25"),
		debit("2026-06-01T10:00:00+02:00", "Sortie B (07/06/2026)", "25"),
	}}})[0]
	assert.Equal(t, 2, v.Debits)
	assert.Equal(t, payments.Amount(5000), v.Debited)
	assert.Equal(t, 1, v.Recredits)
	assert.Equal(t, payments.Amount(2500), v.Recredited)
	assert.Equal(t, payments.Amount(2500), v.Net())
	assert.False(t, v.Partial())

	unread := Read([]Card{{Title: "Carte 10", Entries: []Entry{
		{Action: "prépaye", At: "2026-06-01T10:00:00+02:00", Detail: "prépaye Sortie Levant -30€"},
	}}})[0]
	assert.Equal(t, 1, unread.Unread)
	assert.True(t, unread.Partial(), "a money line no form reads makes the totals partial")
}

// Design §4: a dive taken from two cards of the person is marked with the
// sum of its parts and left out of the usual amount.
func TestReadSplitDiveIsNotUnusual(t *testing.T) {
	views := Read([]Card{
		{Title: "Carte 10", Entries: []Entry{
			debit("2026-08-20T18:00:00+02:00", "Sortie Épave (29/08/2026)", "10"),
			debit("2026-07-01T18:00:00+02:00", "Sortie Levant (10/07/2026)", "25"),
			debit("2026-06-01T18:00:00+02:00", "Sortie Garonne (13/06/2026)", "25"),
		}},
		{Title: "Carte 5", Entries: []Entry{debit("2026-08-21T08:00:00+02:00", "sortie  ÉPAVE (29/08/2026)", "15")}},
	})
	require.Len(t, views, 2)
	assert.Equal(t, "Carte 5", views[0].Title, "newest first")
	assert.Equal(t, payments.Amount(2500), views[0].Lines[0].Split, "10 € + 15 €, titles compared normalised")
	assert.Equal(t, payments.Amount(2500), views[1].Lines[0].Split)
	assert.Zero(t, views[0].Unusual+views[1].Unusual, "the 10 € part is not an unusual amount")
	assert.Zero(t, views[1].Lines[1].Split)
}

// Review Focus 1: a dive debited on A, given back on A and debited on B
// moved; it is not split.
func TestReadMovedDiveIsNotSplit(t *testing.T) {
	views := Read([]Card{
		{Title: "Carte A", Entries: []Entry{
			recredit("2026-06-02T10:00:00+02:00", "Sortie Épave (14/06/2026)", "25"),
			debit("2026-06-01T10:00:00+02:00", "Sortie Épave (14/06/2026)", "25"),
		}},
		{Title: "Carte B", Entries: []Entry{debit("2026-06-03T10:00:00+02:00", "Sortie Épave (14/06/2026)", "25")}},
	})
	require.Len(t, views, 2)
	for _, v := range views {
		for _, l := range v.Lines {
			assert.Zero(t, l.Split, v.Title)
		}
	}
}

// The debit tooltip may carry a qualifier the recredit lacks: the dive still
// moved.
func TestReadMovedDiveWithAsymmetricQualifierIsNotSplit(t *testing.T) {
	views := Read([]Card{
		{Title: "Carte A", Entries: []Entry{
			recredit("2026-06-02T10:00:00+02:00", "Sortie Épave (14/06/2026)", "25"),
			debit("2026-06-01T10:00:00+02:00", "Sortie Épave (N2) (14/06/2026)", "25"),
		}},
		{Title: "Carte B", Entries: []Entry{debit("2026-06-03T10:00:00+02:00", "Sortie Épave (N2) (14/06/2026)", "25")}},
	})
	require.Len(t, views, 2)
	for _, v := range views {
		for _, l := range v.Lines {
			assert.Zero(t, l.Split, v.Title)
		}
	}
}

// Two cards each keeping a net debit of one dive split it, whatever the
// qualifier of each title.
func TestReadSplitDiveWithAsymmetricQualifier(t *testing.T) {
	views := Read([]Card{
		{Title: "Carte A", Entries: []Entry{debit("2026-06-01T10:00:00+02:00", "Sortie Épave (N2) (14/06/2026)", "10")}},
		{Title: "Carte B", Entries: []Entry{debit("2026-06-03T10:00:00+02:00", "Sortie Épave (14/06/2026)", "15")}},
	})
	require.Len(t, views, 2)
	for _, v := range views {
		assert.Equal(t, payments.Amount(2500), v.Lines[0].Split, v.Title)
	}
}

// Design §4: the most frequent debit amount is the card's reference when it
// comes twice or more and alone on top.
func TestReadUnusualAmounts(t *testing.T) {
	unusual := func(amounts ...string) []bool {
		entries := make([]Entry, len(amounts))
		for i, a := range amounts {
			entries[i] = debit("2026-06-01T10:00:00+02:00", "Sortie "+strconv.Itoa(i)+" (14/06/2026)", a)
		}
		v := Read([]Card{{Title: "Carte 10", Entries: entries}})[0]
		out := make([]bool, len(v.Lines))
		for i, l := range v.Lines {
			out[i] = l.Unusual
		}
		return out
	}
	assert.Equal(t, []bool{false, true, false}, unusual("30", "2", "30"), "30 twice and alone on top: 2 is unusual")
	assert.Equal(t, []bool{false, false, true, false}, unusual("25", "25", "50", "25"), "50 on a card of 25")
	assert.Equal(t, []bool{false, false, false, false}, unusual("30", "25", "30", "25"), "a tie: no reference")
	assert.Equal(t, []bool{false, false}, unusual("30", "25"), "no amount comes twice")
	assert.Equal(t, []bool{false}, unusual("2"), "a single debit says nothing")
}

// Review Focus 5: two cards of one product stay two, newest first by their
// dated lines.
func TestReadSortsCardsByTheirNewestDatedLine(t *testing.T) {
	views := Read([]Card{
		{Title: "Carte 10 plongées", Amount: -10000, Entries: []Entry{
			{Action: "Commentaire", Detail: "renouvelée"},
			{Action: "Ajout au panier", At: "2025-03-01T09:00:00+01:00"},
		}},
		{Title: "Carte 10 plongées", Amount: -30000, Entries: []Entry{{Action: "Ajout au panier", At: "2026-03-01T09:00:00+01:00"}}},
	})
	require.Len(t, views, 2)
	assert.Equal(t, payments.Amount(-30000), views[0].Amount, "an undated comment dates nothing")
}

// Design §6: the block's states are those of the payments block.
func TestBlockStates(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	hugo := f.nameHash("Bernard", "Hugo")
	b, err := f.store.Block(ctx, hugo)
	require.NoError(t, err)
	assert.Equal(t, payments.BlockNoLines, b.State)

	require.NoError(t, f.push(t, fixture(t)))
	b, err = f.store.Block(ctx, hugo)
	require.NoError(t, err)
	assert.Equal(t, payments.BlockLines, b.State)
	require.Len(t, b.Cards, 2)
	assert.Equal(t, "Carte 5 plongées niveau 1 et 2", b.Cards[0].Title, "newest first")
	ten := b.Cards[1]
	assert.Equal(t, 4, ten.Debits)
	assert.Equal(t, payments.Amount(4200), ten.Net())
	assert.Equal(t, 1, ten.Unusual, "the 2 € night dive")
	for name, tc := range map[string]struct {
		hash []byte
		want payments.BlockState
	}{
		"homonyms":   {f.nameHash("Martin", "Léa"), payments.BlockAmbiguous},
		"no card":    {f.nameHash("Durand", "Noé"), payments.BlockEmpty},
		"not listed": {nil, payments.BlockNoMember},
	} {
		got, err := f.store.Block(ctx, tc.hash)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got.State, name)
	}

	f.clock.t = f.clock.t.AddDate(0, 0, 91)
	require.NoError(t, f.store.Purge(ctx))
	b, err = f.store.Block(ctx, hugo)
	require.NoError(t, err)
	assert.Equal(t, payments.BlockPurged, b.State)
}
