package tickets

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (e *env) removeAccount(username string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.accounts, username)
}

func (e *env) version(t *testing.T, id int64) int64 {
	t.Helper()
	var v int64
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT version FROM tickets WHERE id = ?`, id).Scan(&v))
	return v
}

// force puts a request in a status by SQL, respecting the table's CHECKs.
func (e *env) force(t *testing.T, id int64, s Status, assignee string) {
	t.Helper()
	var closed any
	if s == StatusDone {
		closed = e.clock.now().Unix()
	}
	_, err := e.db.ExecContext(context.Background(), `UPDATE tickets SET status = ?, assignee = ?, closed_at = ?, version = version + 1 WHERE id = ?`,
		s, nullString(assignee), closed, id)
	require.NoError(t, err)
}

// apply runs cmd on id with its current version.
func (e *env) apply(t *testing.T, id int64, cmd Command) error {
	t.Helper()
	cmd.TicketID, cmd.Version = id, e.version(t, id)
	if cmd.Actor == "" {
		cmd.Actor = "alice"
	}
	return e.store.Apply(context.Background(), cmd)
}

func TestMatrix(t *testing.T) {
	// Spec §8.1, written out rather than derived from allowedFrom.
	allowed := map[Action][]Status{
		ActionTake:       {StatusTodo},
		ActionReassign:   {StatusInProgress, StatusWaiting},
		ActionUnassign:   {StatusInProgress, StatusWaiting},
		ActionWait:       {StatusInProgress},
		ActionResume:     {StatusWaiting},
		ActionReply:      {StatusInProgress, StatusWaiting},
		ActionReplyWait:  {StatusInProgress, StatusWaiting},
		ActionReplyClose: {StatusInProgress, StatusWaiting},
		ActionNote:       {StatusInProgress, StatusWaiting},
		ActionCategory:   {StatusTodo, StatusInProgress, StatusWaiting},
		ActionClose:      {StatusTodo, StatusInProgress, StatusWaiting},
	}
	e := newTestStore(t)
	for action, from := range allowed {
		for _, status := range []Status{StatusTodo, StatusInProgress, StatusWaiting, StatusDone} {
			id, _ := e.submit(t)
			assignee := "alice"
			if status == StatusTodo {
				assignee = ""
			}
			e.force(t, id, status, assignee)
			err := e.apply(t, id, Command{Action: action, Actor: "bob", Assignee: "bob", Body: "Réponse du comité", Category: "autre"})
			if slices.Contains(from, status) {
				require.NoError(t, err, "%s from %s", action, status)
			} else {
				require.ErrorIs(t, err, ErrNotAllowed, "%s from %s", action, status)
			}
		}
	}
	require.ErrorIs(t, e.store.Apply(context.Background(), Command{Action: "launch"}), ErrInvalid)
}

func TestTakeAssignsAndMailsTheMember(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	e.mails(t)
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake, Actor: "alice"}))
	status, assignee := e.status(t, id)
	assert.Equal(t, StatusInProgress, status)
	assert.Equal(t, "alice", assignee)
	msgs := e.mails(t)
	require.Len(t, msgs, 1)
	assert.Equal(t, memberAddress, msgs[0].To)
	assert.Contains(t, msgs[0].Text, "Alice, présidente s'occupe de ta demande CPP-0001")
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM events WHERE ticket_id = ? AND type = 'taken' AND actor = 'alice'`, id))
	assert.Equal(t, Change{Type: ChangeUpdated, TicketID: id}, e.recorded()[1])
}

func TestConcurrentTakeHasOneWinner(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	version := e.version(t, id)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, who := range []string{"alice", "bob"} {
		wg.Go(func() {
			errs[i] = e.store.Apply(context.Background(), Command{Action: ActionTake, TicketID: id, Version: version, Actor: who})
		})
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, ErrStale)
		}
	}
	assert.Equal(t, 1, wins)
	_, assignee := e.status(t, id)
	assert.Contains(t, []string{"alice", "bob"}, assignee)
}

func TestStaleVersionHasNoEffect(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	e.mails(t)
	stale := e.version(t, id) - 1
	err := e.store.Apply(context.Background(), Command{Action: ActionReplyClose, TicketID: id, Version: stale, Actor: "alice", Body: "Réglé"})
	require.ErrorIs(t, err, ErrStale)
	status, _ := e.status(t, id)
	assert.Equal(t, StatusInProgress, status)
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM messages WHERE ticket_id = ?`, id))
	assert.Empty(t, e.mails(t))
}

func TestInvariantIsEnforcedByTheDatabase(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	_, err := e.db.ExecContext(context.Background(), `UPDATE tickets SET assignee = 'alice' WHERE id = ?`, id)
	require.Error(t, err, "a todo request cannot have a resolver")
	_, err = e.db.ExecContext(context.Background(), `UPDATE tickets SET status = 'in_progress' WHERE id = ?`, id)
	require.Error(t, err, "an in-progress request needs a resolver")
}

func TestReplyCombinedWithAStatusSendsOneMail(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	e.mails(t)

	require.NoError(t, e.apply(t, id, Command{Action: ActionReplyWait, Body: "Peux-tu envoyer une capture ?"}))
	status, _ := e.status(t, id)
	assert.Equal(t, StatusWaiting, status)
	msgs := e.mails(t)
	require.Len(t, msgs, 1)
	assert.Equal(t, "Réponse à ta demande CPP-0001", msgs[0].Subject)
	assert.Contains(t, msgs[0].Text, "Peux-tu envoyer une capture ?")
	assert.Contains(t, msgs[0].Text, "Une réponse de ta part est attendue")

	// Already waiting: reply_wait keeps it waiting.
	require.NoError(t, e.apply(t, id, Command{Action: ActionReplyWait, Body: "Relance"}))
	status, _ = e.status(t, id)
	assert.Equal(t, StatusWaiting, status)
	assert.Len(t, e.mails(t), 1)

	require.NoError(t, e.apply(t, id, Command{Action: ActionReplyClose, Body: "C'est corrigé."}))
	status, _ = e.status(t, id)
	assert.Equal(t, StatusDone, status)
	msgs = e.mails(t)
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Text, "Ta demande est maintenant réglée")
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM tickets WHERE id = ? AND closed_at IS NOT NULL`, id))
}

func TestNotesNeverReachTheMember(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	e.mails(t)
	require.NoError(t, e.apply(t, id, Command{Action: ActionNote, Body: "Note interne secrète"}))
	assert.Empty(t, e.mails(t), "a note sends no mail")
	require.NoError(t, e.apply(t, id, Command{Action: ActionReply, Body: "Bonjour"}))
	for _, m := range e.mails(t) {
		assert.NotContains(t, m.Text, "Note interne secrète")
	}
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM messages WHERE ticket_id = ? AND internal = 1`, id))
}

func TestReassignUnassignCategoryAndClose(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	require.ErrorIs(t, e.apply(t, id, Command{Action: ActionReassign, Assignee: "zoe"}), ErrInvalid, "unknown account")
	require.ErrorIs(t, e.apply(t, id, Command{Action: ActionReassign, Assignee: "alice"}), ErrInvalid, "same resolver")
	require.NoError(t, e.apply(t, id, Command{Action: ActionReassign, Assignee: "bob"}))
	_, assignee := e.status(t, id)
	assert.Equal(t, "bob", assignee)

	require.ErrorIs(t, e.apply(t, id, Command{Action: ActionCategory, Category: "inconnue"}), ErrInvalid)
	require.NoError(t, e.apply(t, id, Command{Action: ActionCategory, Category: "bug"}), "committee-only categories are allowed")
	require.NoError(t, e.apply(t, id, Command{Action: ActionCategory, Category: "carnet"}), "and back")

	require.NoError(t, e.apply(t, id, Command{Action: ActionUnassign}))
	status, assignee := e.status(t, id)
	assert.Equal(t, StatusTodo, status)
	assert.Empty(t, assignee)

	e.mails(t)
	require.NoError(t, e.apply(t, id, Command{Action: ActionClose, Actor: "bob"}))
	status, assignee = e.status(t, id)
	assert.Equal(t, StatusDone, status)
	assert.Equal(t, "bob", assignee, "closing an unassigned request assigns the closer")
	msgs := e.mails(t)
	require.Len(t, msgs, 1)
	assert.Equal(t, "Demande CPP-0001 réglée", msgs[0].Subject)
	assert.Equal(t, 2, e.count(t, `SELECT COUNT(*) FROM events WHERE ticket_id = ? AND type = 'category_changed'`, id))
}

func TestMemberReplyOnWaitingResumesAndAlertsTheClub(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	require.NoError(t, e.apply(t, id, Command{Action: ActionWait}))
	e.mails(t)

	require.NoError(t, e.store.MemberReply(context.Background(), id, "Voici la capture.", []Upload{png()}))
	status, assignee := e.status(t, id)
	assert.Equal(t, StatusInProgress, status)
	assert.Equal(t, "alice", assignee)
	msgs := e.mails(t)
	require.Len(t, msgs, 1)
	assert.Equal(t, clubAddress, msgs[0].To)
	assert.Equal(t, "Nouvelle réponse sur la demande CPP-0001", msgs[0].Subject)
	assert.NotContains(t, msgs[0].Text, "Voici la capture.", "club mails carry no member text")
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM attachments WHERE ticket_id = ? AND message_id IS NOT NULL`, id))
	assert.Equal(t, ChangeReplied, e.recorded()[len(e.recorded())-1].Type)
}

func TestMemberReplyReopensWithinFourteenDays(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()

	inTime, _ := e.submit(t)
	require.NoError(t, e.apply(t, inTime, Command{Action: ActionTake}))
	require.NoError(t, e.apply(t, inTime, Command{Action: ActionClose}))
	late, _ := e.submit(t)
	require.NoError(t, e.apply(t, late, Command{Action: ActionTake}))
	require.NoError(t, e.apply(t, late, Command{Action: ActionClose}))

	e.mails(t)
	e.clock.advance(ReplyWindow - time.Second)
	require.NoError(t, e.store.MemberReply(ctx, inTime, "Finalement non, c'est toujours faux.", nil))
	status, assignee := e.status(t, inTime)
	assert.Equal(t, StatusInProgress, status, "back to its resolver")
	assert.Equal(t, "alice", assignee)
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM tickets WHERE id = ? AND closed_at IS NOT NULL`, inTime))
	msgs := e.mails(t)
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Text, "qui est rouverte")

	e.clock.advance(2 * time.Second)
	require.ErrorIs(t, e.store.MemberReply(ctx, late, "Trop tard ?", nil), ErrNotAllowed)
}

func TestReopenedWithoutAResolverGoesBackToTodo(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()

	closedByMember, _ := e.submit(t)
	require.NoError(t, e.store.MemberClose(ctx, closedByMember))
	status, assignee := e.status(t, closedByMember)
	assert.Equal(t, StatusDone, status)
	assert.Empty(t, assignee)
	require.NoError(t, e.store.MemberReply(ctx, closedByMember, "Ça recommence.", nil))
	status, _ = e.status(t, closedByMember)
	assert.Equal(t, StatusTodo, status)

	removed, _ := e.submit(t)
	require.NoError(t, e.apply(t, removed, Command{Action: ActionTake, Actor: "bob"}))
	require.NoError(t, e.apply(t, removed, Command{Action: ActionClose, Actor: "bob"}))
	e.removeAccount("bob")
	require.NoError(t, e.store.MemberReply(ctx, removed, "Ça recommence aussi.", nil))
	status, assignee = e.status(t, removed)
	assert.Equal(t, StatusTodo, status)
	assert.Empty(t, assignee)
}

func TestMemberCaptureLimits(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	id, _ := e.submit(t, png(), png(), png())
	require.ErrorIs(t, e.store.MemberReply(ctx, id, "Quatre captures", []Upload{png(), png(), png(), png()}), ErrTooManyCaptures)
	require.NoError(t, e.store.MemberReply(ctx, id, "Trois de plus", []Upload{png(), png(), png()}))
	require.NoError(t, e.store.MemberReply(ctx, id, "Encore trois", []Upload{png(), png(), png()}))
	require.ErrorIs(t, e.store.MemberReply(ctx, id, "La onzième", []Upload{png(), png()}), ErrTooManyCaptures)
	require.NoError(t, e.store.MemberReply(ctx, id, "La dixième", []Upload{png()}))
	assert.Len(t, e.objects(t), 10, "refused replies leave no object behind")
}

func TestMemberClose(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	id, _ := e.submit(t)
	e.mails(t)
	require.NoError(t, e.store.MemberClose(ctx, id))
	assert.Empty(t, e.mails(t), "closing by the member sends no mail")
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM events WHERE ticket_id = ? AND type = 'closed_by_member' AND actor = 'member'`, id))
	require.ErrorIs(t, e.store.MemberClose(ctx, id), ErrNotAllowed)
	require.ErrorIs(t, e.store.MemberClose(ctx, 999), ErrNotFound)
}

func TestReplyMailQuotesTheStartOnly(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	e.mails(t)
	long := strings.Repeat("Une phrase de dix mots pour remplir la réponse longue. ", 40)
	require.NoError(t, e.apply(t, id, Command{Action: ActionReply, Body: long}))
	msgs := e.mails(t)
	require.Len(t, msgs, 1)
	assert.Equal(t, mail.Message{To: memberAddress, Subject: "Réponse à ta demande CPP-0001", Text: msgs[0].Text}, msgs[0])
	assert.Contains(t, msgs[0].Text, "…")
	assert.NotContains(t, msgs[0].Text, long)
}
