package tickets

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

func refs(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Ref
	}
	return out
}

func TestBoardFilters(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	todo, _ := e.submit(t)    // CPP-0001
	mine, _ := e.submit(t)    // CPP-0002
	waiting, _ := e.submit(t) // CPP-0003
	draft, _ := e.submit(t)   // CPP-0004, turned back into a draft below
	require.NoError(t, e.apply(t, mine, Command{Action: ActionTake, Actor: "alice"}))
	require.NoError(t, e.apply(t, waiting, Command{Action: ActionTake, Actor: "bob"}))
	require.NoError(t, e.apply(t, waiting, Command{Action: ActionWait, Actor: "bob"}))
	require.NoError(t, e.apply(t, todo, Command{Action: ActionCategory, Category: "bug"}))
	// A draft never shows.
	_, err := e.db.ExecContext(ctx, `UPDATE tickets SET status = 'draft', ref = NULL, submitted_at = NULL, assignee = NULL, closed_at = NULL WHERE id = ?`, draft)
	require.NoError(t, err)
	_, last := e.submit(t) // CPP-0005, done below
	require.NoError(t, e.apply(t, e.idOf(t, last), Command{Action: ActionClose}))

	for _, c := range []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"default: todo and in progress", Filter{}, []string{"CPP-0001", "CPP-0002"}},
		{"waiting", Filter{Statuses: []Status{StatusWaiting}}, []string{"CPP-0003"}},
		{"every open", Filter{Statuses: []Status{StatusTodo, StatusInProgress, StatusWaiting}}, []string{"CPP-0001", "CPP-0002", "CPP-0003"}},
		{"done, drafts excluded", Filter{Statuses: []Status{StatusDone, StatusDraft}}, []string{"CPP-0005"}},
		{"nobody", Filter{Statuses: []Status{StatusTodo, StatusInProgress, StatusWaiting}, Assignee: AssigneeNobody}, []string{"CPP-0001"}},
		{"bob's", Filter{Statuses: []Status{StatusTodo, StatusInProgress, StatusWaiting}, Assignee: "bob"}, []string{"CPP-0003"}},
		{"bug category", Filter{Category: "bug"}, []string{"CPP-0001"}},
		{"removed category", Filter{Category: "gone"}, []string{}},
	} {
		rows, err := e.store.Board(ctx, c.filter)
		require.NoError(t, err, c.name)
		assert.Equal(t, c.want, refs(rows), c.name)
	}
}

func TestBoardRowContent(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	sub := submission(t)
	sub.Description = strings.Repeat("Ligne de description assez longue.\r\n", 10)
	out, err := e.store.Submit(ctx, sub, nil)
	require.NoError(t, err)
	ref := out.Ref
	id := e.idOf(t, ref)

	rows, err := e.store.Board(ctx, Filter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	r := rows[0]
	assert.Equal(t, "Léa", r.FirstName)
	assert.Equal(t, "Martin", r.LastName)
	assert.Equal(t, "carnet", r.Category)
	assert.NotContains(t, r.Excerpt, "\n")
	assert.Len(t, []rune(r.Excerpt), 141, "140 runes and an ellipsis")
	assert.False(t, r.MemberRepliedLast)
	assert.Equal(t, e.clock.now(), r.SubmittedAt)

	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	require.NoError(t, e.store.MemberReply(ctx, id, "Une précision.", nil))
	rows, err = e.store.Board(ctx, Filter{})
	require.NoError(t, err)
	assert.True(t, rows[0].MemberRepliedLast)
	require.NoError(t, e.apply(t, id, Command{Action: ActionNote, Body: "Une note ne compte pas."}))
	rows, err = e.store.Board(ctx, Filter{})
	require.NoError(t, err)
	assert.True(t, rows[0].MemberRepliedLast, "notes are ignored")
	require.NoError(t, e.apply(t, id, Command{Action: ActionReply, Body: "Réponse."}))
	rows, err = e.store.Board(ctx, Filter{})
	require.NoError(t, err)
	assert.False(t, rows[0].MemberRepliedLast)
}

func TestDetailAndTrackingToken(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	id, ref := e.submit(t, png())
	token := tokenOf(t, e.mails(t))
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	require.NoError(t, e.apply(t, id, Command{Action: ActionNote, Body: "Vérifier la sortie du 12."}))
	require.NoError(t, e.apply(t, id, Command{Action: ActionReplyWait, Body: "Quelle sortie ?"}))
	require.NoError(t, e.store.MemberReply(ctx, id, "Celle du 12.", []Upload{png()}))

	d, err := e.store.ByToken(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, ref, d.Ref)
	assert.Equal(t, StatusInProgress, d.Status)
	assert.Equal(t, "alice", d.Assignee)
	assert.Equal(t, memberAddress, d.Email)
	assert.Equal(t, testBody, d.Description)
	assert.Equal(t, "carnet", d.Fields.Category)
	assert.Equal(t, e.version(t, id), d.Version)
	require.Len(t, d.Captures, 1)
	require.Len(t, d.Messages, 3)
	assert.True(t, d.Messages[0].Internal)
	assert.Equal(t, "alice", d.Messages[1].Author)
	assert.True(t, d.Messages[2].FromMember)
	require.Len(t, d.Messages[2].Captures, 1)
	assert.Equal(t, "image/png", d.Messages[2].Captures[0].MIME)

	public := d.Public()
	require.Len(t, public, 2)
	for _, m := range public {
		assert.NotContains(t, m.Body, "Vérifier la sortie")
	}
	types := make([]string, 0, len(d.Events))
	for _, ev := range d.Events {
		types = append(types, ev.Type)
		assert.NotEmpty(t, e.store.Describe(ev))
	}
	assert.Equal(t, []string{"submitted", "taken", "waiting", "member_replied"}, types)

	_, err = e.store.ByToken(ctx, "unknown-token")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = e.store.Detail(ctx, 999)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = e.db.ExecContext(ctx, `UPDATE tickets SET status = 'draft', ref = NULL, submitted_at = NULL, assignee = NULL WHERE id = ?`, id)
	require.NoError(t, err)
	_, err = e.store.ByToken(ctx, token)
	require.ErrorIs(t, err, ErrNotFound, "a draft has no tracking page")
}

func TestCanReply(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	id, _ := e.submit(t)
	d, err := e.store.Detail(ctx, id)
	require.NoError(t, err)
	assert.True(t, e.store.CanReply(d))
	require.NoError(t, e.store.MemberClose(ctx, id))
	d, err = e.store.Detail(ctx, id)
	require.NoError(t, err)
	assert.True(t, e.store.CanReply(d))
	e.clock.advance(ReplyWindow + 1)
	assert.False(t, e.store.CanReply(d))
}

func TestOthers(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	first, _ := e.submit(t)
	e.submit(t)
	third, _ := e.submit(t)
	require.NoError(t, e.apply(t, first, Command{Action: ActionClose}))
	sub := submission(t)
	sub.Email = "someone.else@example.org"
	_, err := e.store.Submit(ctx, sub, nil)
	require.NoError(t, err)

	rows, err := e.store.Others(ctx, third)
	require.NoError(t, err)
	assert.Equal(t, []string{"CPP-0002", "CPP-0001"}, refs(rows), "open first, then done; never itself nor another address")
}

func TestCapture(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	capture := png()
	id, _ := e.submit(t, capture)
	d, err := e.store.Detail(ctx, id)
	require.NoError(t, err)
	attID := d.Captures[0].ID

	data, mime, err := e.store.Capture(ctx, id, attID)
	require.NoError(t, err)
	assert.Equal(t, capture.Data, data)
	assert.Equal(t, "image/png", mime)

	_, _, err = e.store.Capture(ctx, id+1, attID)
	require.ErrorIs(t, err, ErrNotFound, "a capture is reachable through its own request only")

	for _, o := range e.objects(t) {
		require.NoError(t, e.blobs.Delete(ctx, o.Key))
	}
	_, _, err = e.store.Capture(ctx, id, attID)
	require.ErrorIs(t, err, ErrStorage)
}

func TestDescribeUsesLabels(t *testing.T) {
	e := newTestStore(t)
	assert.Equal(t, "Réassignée à Bob (Trésorier)", e.store.Describe(Event{Type: string(eventReassigned), Data: map[string]string{dataTo: "bob"}}))
	assert.Equal(t, "Catégorie changée : "+e.store.Catalog.CategoryLabel("carnet")+" → "+e.store.Catalog.CategoryLabel("bug"),
		e.store.Describe(Event{Type: string(eventCategoryChanged), Data: map[string]string{dataFrom: "carnet", dataTo: "bug"}}))
	assert.Equal(t, "Rouverte par une réponse de l'adhérent : À traiter",
		e.store.Describe(Event{Type: string(eventReopened), Data: map[string]string{dataStatus: "todo"}}))
	assert.Contains(t, e.store.Describe(Event{Type: string(eventCategoryChanged), Data: map[string]string{dataFrom: "gone", dataTo: "bug"}}), "gone (retiré)")
}

func TestSendLinks(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	_, first := e.submit(t)
	_, second := e.submit(t)
	tokens := trackingPattern.FindAllStringSubmatch(joinTexts(e.mails(t)), -1)
	require.Len(t, tokens, 2)

	require.NoError(t, e.store.SendLinks(ctx, "  LEA.Martin@example.org "))
	msgs := e.mails(t)
	require.Len(t, msgs, 1)
	assert.Equal(t, memberAddress, msgs[0].To)
	assert.Equal(t, "Tes liens de suivi", msgs[0].Subject)
	for _, tok := range tokens {
		assert.Contains(t, msgs[0].Text, tok[0], "the original links")
	}
	assert.Contains(t, msgs[0].Text, first)
	assert.Contains(t, msgs[0].Text, second)
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM outbox WHERE event = 'lost_link' AND ticket_id IS NULL`))

	require.NoError(t, e.store.SendLinks(ctx, "unknown@example.org"))
	require.NoError(t, e.store.SendLinks(ctx, "not an address"))
	assert.Empty(t, e.mails(t))
}

func joinTexts(msgs []mail.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Text)
	}
	return b.String()
}
