package tickets

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func categoryAnswer(category, source string) Chooser {
	return func(context.Context) (Suggestion, bool) {
		return Suggestion{SuggestedCategory: category, CategorySource: source, Summary: "Synthetic summary"}, true
	}
}

func TestCategorySuggestionIsSealedAndDoesNotReclassify(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	sub := submission(t)
	out, err := e.store.Submit(ctx, sub, categoryAnswer("paiement", "carnet"))
	require.NoError(t, err)
	assert.NotEmpty(t, out.Ref)
	assert.Empty(t, out.Token)
	id := e.idOf(t, out.Ref)
	detail, err := e.store.Detail(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "carnet", detail.Category)
	assert.Equal(t, "paiement", detail.SuggestedCategory)
	assert.Equal(t, sub.Fields, detail.Fields)
	var raw []byte
	require.NoError(t, e.db.QueryRowContext(ctx, `SELECT suggested_category FROM tickets WHERE id=?`, id).Scan(&raw))
	assert.NotEqual(t, []byte("paiement"), raw)
	opened, err := e.keys.OpenString(raw)
	require.NoError(t, err)
	assert.Equal(t, "paiement", opened)
	again, err := e.store.Submit(ctx, sub, categoryAnswer("autre", "carnet"))
	require.NoError(t, err)
	assert.Equal(t, out.Ref, again.Ref)
	detail, err = e.store.Detail(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "paiement", detail.SuggestedCategory)
}

func TestCategorySuggestionLateDeliveryVersionsTicket(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	id, _ := e.submit(t)
	before := e.version(t, id)
	e.mails(t)
	var updated int64
	require.NoError(t, e.db.QueryRowContext(ctx, `SELECT updated_at FROM tickets WHERE id=?`, id).Scan(&updated))
	e.clock.advance(time.Hour)
	sg := Suggestion{SuggestedCategory: "paiement", CategorySource: "carnet", KBIDs: []string{"fiche-a"}, Summary: "Late summary"}
	_, err := e.store.suggested(ctx, id, sg)
	require.NoError(t, err)
	assert.Equal(t, before+1, e.version(t, id))
	detail, err := e.store.Detail(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "paiement", detail.SuggestedCategory)
	assert.Equal(t, "Late summary", detail.Summary)
	assert.Equal(t, []string{"fiche-a"}, detail.KBIDs)
	assert.Empty(t, e.mails(t))
	changes := e.recorded()
	assert.Equal(t, ChangeUpdated, changes[len(changes)-1].Type)
	var afterUpdated int64
	require.NoError(t, e.db.QueryRowContext(ctx, `SELECT updated_at FROM tickets WHERE id=?`, id).Scan(&afterUpdated))
	assert.Equal(t, updated, afterUpdated)
	err = e.store.Apply(ctx, Command{TicketID: id, Version: before, Action: ActionTake, Actor: "alice"})
	require.ErrorIs(t, err, ErrStale)
	_, err = e.store.suggested(ctx, id, Suggestion{SuggestedCategory: "autre", CategorySource: "carnet"})
	require.NoError(t, err)
	assert.Equal(t, before+1, e.version(t, id))
}

func TestCategorySuggestionRejectsInvalidDestination(t *testing.T) {
	for _, tc := range []struct{ name, category, source string }{
		{"unknown", "invented", "carnet"}, {"committee only", "bug", "carnet"}, {"different source", "paiement", "compte"}, {"same destination", "carnet", "carnet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestStore(t)
			ctx := context.Background()
			out, err := e.store.Submit(ctx, submission(t), categoryAnswer(tc.category, tc.source))
			require.NoError(t, err)
			detail, err := e.store.Detail(ctx, e.idOf(t, out.Ref))
			require.NoError(t, err)
			assert.Empty(t, detail.SuggestedCategory)
			assert.Equal(t, "Synthetic summary", detail.Summary)
			assert.NotNil(t, detail.KBIDs)
		})
	}
}

func TestCategorySuggestionFollowsTicketDeletion(t *testing.T) {
	for _, how := range []string{"delete", "erase", "abandon", "purge"} {
		t.Run(how, func(t *testing.T) {
			e := newTestStore(t)
			ctx := context.Background()
			chooser := categoryAnswer("paiement", "carnet")
			if how == "abandon" || how == "purge" {
				chooser = func(context.Context) (Suggestion, bool) {
					return Suggestion{SuggestedCategory: "paiement", CategorySource: "carnet", KBIDs: []string{"fiche-a"}}, true
				}
			}
			out, err := e.store.Submit(ctx, submission(t), chooser)
			require.NoError(t, err)
			switch how {
			case "delete":
				require.NoError(t, e.apply(t, e.idOf(t, out.Ref), Command{Action: ActionDelete}))
			case "erase":
				_, err = e.store.Erase(ctx, memberAddress, "alice")
				require.NoError(t, err)
			case "abandon":
				_, err = e.store.Abandon(ctx, out.Token)
				require.NoError(t, err)
			case "purge":
				e.clock.advance(25 * time.Hour)
				require.NoError(t, e.store.Purge(ctx))
			}
			assert.Zero(t, e.count(t, `SELECT count(*) FROM tickets`))
		})
	}
}

func TestDismissCategorySuggestionPreservesTicket(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	out, err := e.store.Submit(ctx, submission(t), categoryAnswer("paiement", "carnet"))
	require.NoError(t, err)
	id := e.idOf(t, out.Ref)
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	require.NoError(t, e.apply(t, id, Command{Action: ActionNote, Body: "Internal note"}))
	require.NoError(t, e.apply(t, id, Command{Action: ActionReassign, Assignee: "bob"}))
	before, err := e.store.Detail(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "paiement", before.SuggestedCategory, "unrelated actions preserve advice")
	e.mails(t)
	require.NoError(t, e.apply(t, id, Command{Action: Action("dismiss_category_suggestion")}))
	after, err := e.store.Detail(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, before.Category, after.Category)
	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.Assignee, after.Assignee)
	assert.Equal(t, before.Version+1, after.Version)
	assert.Empty(t, after.SuggestedCategory)
	assert.Equal(t, 1, e.count(t, `SELECT category_suggestion_resolved FROM tickets WHERE id=?`, id))
	assert.Equal(t, before.Events, after.Events)
	assert.Empty(t, e.mails(t))
	err = e.store.Apply(ctx, Command{TicketID: id, Version: before.Version, Actor: "alice", Action: Action("dismiss_category_suggestion")})
	require.ErrorIs(t, err, ErrStale)
	require.ErrorIs(t, e.apply(t, id, Command{Action: Action("dismiss_category_suggestion")}), ErrInvalid)
}

func TestDismissCategorySuggestionRefusals(t *testing.T) {
	for _, state := range []string{"draft", "closed", "absent"} {
		t.Run(state, func(t *testing.T) {
			e := newTestStore(t)
			ctx := context.Background()
			var id int64
			switch state {
			case "draft":
				var err error
				id, err = e.store.draft(ctx, submission(t))
				require.NoError(t, err)
			default:
				id, _ = e.submit(t)
			}
			want := ErrInvalid
			if state == "draft" {
				want = ErrNotFound
			}
			if state == "closed" {
				require.NoError(t, e.apply(t, id, Command{Action: ActionClose}))
				want = ErrNotAllowed
			}
			require.ErrorIs(t, e.apply(t, id, Command{Action: Action("dismiss_category_suggestion")}), want)
		})
	}
}

func TestCategoryActionsResolveSuggestion(t *testing.T) {
	for _, action := range []Action{ActionCategory, Action("dismiss_category_suggestion"), ActionClose, ActionReplyClose} {
		t.Run(string(action), func(t *testing.T) {
			e := newTestStore(t)
			ctx := context.Background()
			out, err := e.store.Submit(ctx, submission(t), categoryAnswer("paiement", "carnet"))
			require.NoError(t, err)
			id := e.idOf(t, out.Ref)
			if action == ActionReplyClose {
				require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
			}
			require.NoError(t, e.apply(t, id, Command{Action: action, Category: "paiement", Body: "Resolved"}))
			detail, err := e.store.Detail(ctx, id)
			require.NoError(t, err)
			assert.Empty(t, detail.SuggestedCategory)
			assert.Equal(t, 1, e.count(t, `SELECT category_suggestion_resolved FROM tickets WHERE id=?`, id))
			if action == ActionCategory {
				assert.Equal(t, "paiement", detail.Category)
				assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM events WHERE ticket_id=? AND type='category_changed'`, id))
			}
			if action == ActionClose || action == ActionReplyClose {
				require.NoError(t, e.store.MemberReply(ctx, id, "More information", nil))
				detail, err = e.store.Detail(ctx, id)
				require.NoError(t, err)
				assert.True(t, detail.Status.Open())
				assert.Empty(t, detail.SuggestedCategory)
			}
		})
	}
}

func TestResolvedCategorySuggestionNeverReturns(t *testing.T) {
	for _, scenario := range []string{"round trip", "close", "reply close"} {
		t.Run(scenario, func(t *testing.T) {
			e := newTestStore(t)
			ctx := context.Background()
			started := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 1)
			sub := submission(t)
			go func() {
				_, err := e.store.Submit(ctx, sub, func(context.Context) (Suggestion, bool) {
					close(started)
					<-release
					return Suggestion{SuggestedCategory: "paiement", CategorySource: "carnet", KBIDs: []string{"fiche-a"}, Summary: "Late summary"}, true
				})
				done <- err
			}()
			<-started
			var id int64
			require.NoError(t, e.db.QueryRowContext(ctx, `SELECT max(id) FROM tickets`).Scan(&id))
			_, err := e.store.Confirm(ctx, id)
			require.NoError(t, err)
			switch scenario {
			case "round trip":
				require.NoError(t, e.apply(t, id, Command{Action: ActionCategory, Category: "paiement"}))
				require.NoError(t, e.apply(t, id, Command{Action: ActionCategory, Category: "carnet"}))
			case "close":
				require.NoError(t, e.apply(t, id, Command{Action: ActionClose}))
			case "reply close":
				require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
				require.NoError(t, e.apply(t, id, Command{Action: ActionReplyClose, Body: "Resolved"}))
			}
			close(release)
			select {
			case err = <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("late submission did not finish")
			}
			detail, err := e.store.Detail(ctx, id)
			require.NoError(t, err)
			assert.Empty(t, detail.SuggestedCategory)
			assert.Equal(t, "Late summary", detail.Summary)
			assert.Equal(t, []string{"fiche-a"}, detail.KBIDs)
			if !detail.Status.Open() {
				require.NoError(t, e.store.MemberReply(ctx, id, "Follow-up", nil))
				detail, err = e.store.Detail(ctx, id)
				require.NoError(t, err)
				assert.Empty(t, detail.SuggestedCategory)
			}
		})
	}
}
