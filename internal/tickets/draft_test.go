package tickets

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// answer is a model that picks ids and writes summary.
func answer(summary string, ids ...string) Chooser {
	return func(context.Context) (Suggestion, bool) { return Suggestion{KBIDs: ids, Summary: summary}, true }
}

// screen2 submits a request the model matches with two fiches and returns
// the submission and its draft token.
func (e *env) screen2(t *testing.T, captures ...Upload) (Submission, string) {
	t.Helper()
	sub := submission(t)
	sub.Captures = captures
	out, err := e.store.Submit(context.Background(), sub, answer("Carnet décompté après une sortie annulée.", "fiche-a", "fiche-b"))
	require.NoError(t, err)
	require.Empty(t, out.Ref)
	require.NotEmpty(t, out.Token)
	return sub, out.Token
}

func TestFichesLeadToScreen2(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	_, token := e.screen2(t)
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM submitted_tickets`), "a draft is invisible")
	assert.Empty(t, e.mails(t), "nothing is sent before the member decides")
	assert.Empty(t, e.recorded(), "the live board does not see a draft")

	d, err := e.store.DraftByToken(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, []string{"fiche-a", "fiche-b"}, d.KBIDs)
	assert.Equal(t, memberAddress, d.Email)
	assert.False(t, d.OpenRequest)
	assert.Empty(t, d.Ref)

	ref, err := e.store.Confirm(ctx, d.ID)
	require.NoError(t, err)
	again, err := e.store.Confirm(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, ref, again, "confirming twice files one request")
	assert.Len(t, e.mails(t), 2)

	d, err = e.store.DraftByToken(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, ref, d.Ref, "the token of a filed request shows its confirmation")
	detail, err := e.store.Detail(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, "Carnet décompté après une sortie annulée.", detail.Summary)
	assert.Equal(t, []string{"fiche-a", "fiche-b"}, detail.KBIDs)
}

func TestNoFicheConfirmsAndKeepsTheSummary(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	out, err := e.store.Submit(ctx, submission(t), answer("Ne voit plus ses réservations."))
	require.NoError(t, err)
	require.Empty(t, out.Token)
	detail, err := e.store.Detail(ctx, e.idOf(t, out.Ref))
	require.NoError(t, err)
	assert.Equal(t, "Ne voit plus ses réservations.", detail.Summary)
	assert.Equal(t, []string{}, detail.KBIDs, "the model answered: no fiche")

	rows, err := e.store.Board(ctx, Filter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "Ne voit plus ses réservations.", rows[0].Summary)
	assert.Contains(t, rows[0].Excerpt, "Mon carnet", "the excerpt stays for the full text")
}

func TestEmptySummaryIsNotStored(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	out, err := e.store.Submit(ctx, submission(t), answer(""))
	require.NoError(t, err)
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM tickets WHERE summary IS NULL AND kb_ids = '[]'`))
	rows, err := e.store.Board(ctx, Filter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Empty(t, rows[0].Summary, "the board falls back to the excerpt")
	assert.NotEmpty(t, rows[0].Excerpt)
	assert.NotEmpty(t, out.Ref)
}

func TestNoAnswerConfirmsWithoutSummary(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	silent := func(context.Context) (Suggestion, bool) { return Suggestion{}, false }
	out, err := e.store.Submit(ctx, submission(t), silent)
	require.NoError(t, err)
	detail, err := e.store.Detail(ctx, e.idOf(t, out.Ref))
	require.NoError(t, err)
	assert.Empty(t, detail.Summary)
	assert.Nil(t, detail.KBIDs, "the model did not answer")
}

func TestAbandonCountsADeflection(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	_, token := e.screen2(t, png())
	require.Len(t, e.objects(t), 1)

	ref, err := e.store.Abandon(ctx, token)
	require.NoError(t, err)
	assert.Empty(t, ref)
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM tickets`))
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM attachments`))
	assert.Empty(t, e.objects(t), "its captures are gone")
	assert.Empty(t, e.mails(t))
	var category, kbIDs string
	require.NoError(t, e.db.QueryRowContext(ctx, `SELECT category, kb_ids FROM deflections`).Scan(&category, &kbIDs))
	assert.Equal(t, "carnet", category)
	assert.JSONEq(t, `["fiche-a", "fiche-b"]`, kbIDs)

	ref, err = e.store.Abandon(ctx, token)
	require.NoError(t, err, "a double tap is not an error")
	assert.Empty(t, ref)
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM deflections`), "counted once")
	_, err = e.store.DraftByToken(ctx, token)
	require.ErrorIs(t, err, ErrDraftGone)

	counts, err := e.store.DeflectionCounts(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"fiche-a": 1, "fiche-b": 1}, counts)
}

func TestConfirmDraft(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	_, token := e.screen2(t)
	ref, err := e.store.ConfirmDraft(ctx, token)
	require.NoError(t, err)
	again, err := e.store.ConfirmDraft(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, ref, again, "a double tap shows the same confirmation")
	assert.Len(t, e.mails(t), 2)
	_, err = e.store.ConfirmDraft(ctx, "unknown-token")
	require.ErrorIs(t, err, ErrDraftGone)

	_, stale := e.screen2(t)
	e.clock.advance(24 * time.Hour)
	_, err = e.store.ConfirmDraft(ctx, stale)
	require.ErrorIs(t, err, ErrDraftGone, "the token dies with the draft")
}

func TestAbandonRefusesAnExpiredDraft(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	_, token := e.screen2(t)
	e.clock.advance(24 * time.Hour)
	_, err := e.store.Abandon(ctx, token)
	require.ErrorIs(t, err, ErrDraftGone, "the token dies with the draft (spec §3.2)")
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM deflections`), "an expired screen counts nothing")
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM tickets`), "the daily purge removes it")
}

func TestAbandonKeepsAFiledRequest(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	_, token := e.screen2(t)
	d, err := e.store.DraftByToken(ctx, token)
	require.NoError(t, err)
	ref, err := e.store.Confirm(ctx, d.ID)
	require.NoError(t, err)

	got, err := e.store.Abandon(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, ref, got)
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM submitted_tickets`))
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM deflections`))
}

func TestResubmissionOnScreen2DrawsANewToken(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	sub, first := e.screen2(t)

	out, found, err := e.store.Resubmitted(ctx, sub.FormKey)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, out.Token)
	assert.NotEqual(t, first, out.Token)
	_, err = e.store.DraftByToken(ctx, first)
	require.ErrorIs(t, err, ErrDraftGone, "the lost page's token no longer works")
	d, err := e.store.DraftByToken(ctx, out.Token)
	require.NoError(t, err)
	assert.Equal(t, []string{"fiche-a", "fiche-b"}, d.KBIDs)
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM tickets`))

	again, err := e.store.Submit(ctx, sub, answer("ignored", "fiche-c"))
	require.NoError(t, err, "the same form posted again, as after a lost response")
	assert.NotEmpty(t, again.Token)
	d, err = e.store.DraftByToken(ctx, again.Token)
	require.NoError(t, err)
	assert.Equal(t, []string{"fiche-a", "fiche-b"}, d.KBIDs, "no second draft, no new answer")
}

func TestDraftExpiresAfter24Hours(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	sub, token := e.screen2(t)
	e.clock.advance(24 * time.Hour)
	_, err := e.store.DraftByToken(ctx, token)
	require.ErrorIs(t, err, ErrDraftGone)

	out, found, err := e.store.Resubmitted(ctx, sub.FormKey)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "CPP-0001", out.Ref, "a form sent again after a day is filed as it was")
}

func TestAnswerAfterAConcurrentConfirmIsKept(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	sub := submission(t)
	// A double tap confirms the draft while the model is still answering.
	slow := func(ctx context.Context) (Suggestion, bool) {
		out, found, err := e.store.Resubmitted(ctx, sub.FormKey)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "CPP-0001", out.Ref)
		return Suggestion{KBIDs: []string{"fiche-a"}, Summary: "Résumé tardif."}, true
	}
	out, err := e.store.Submit(ctx, sub, slow)
	require.NoError(t, err)
	assert.Equal(t, Outcome{Ref: "CPP-0001"}, out)
	detail, err := e.store.Detail(ctx, e.idOf(t, out.Ref))
	require.NoError(t, err)
	assert.Equal(t, "Résumé tardif.", detail.Summary)
	assert.Equal(t, []string{"fiche-a"}, detail.KBIDs)
	id := e.idOf(t, out.Ref)
	assert.Equal(t, []Change{{Type: ChangeCreated, TicketID: id}, {Type: ChangeUpdated, TicketID: id}}, e.recorded(),
		"the board that showed the request without its summary is told to refresh it")
}

func TestDraftKnowsAnOpenRequestOfItsAddress(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	id, _ := e.submit(t)
	_, token := e.screen2(t)
	d, err := e.store.DraftByToken(ctx, token)
	require.NoError(t, err)
	assert.True(t, d.OpenRequest)

	e.force(t, id, StatusDone, "alice")
	d, err = e.store.DraftByToken(ctx, token)
	require.NoError(t, err)
	assert.False(t, d.OpenRequest, "a done request is not in progress")
}
