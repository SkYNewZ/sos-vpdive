package tickets

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/blobs"
	"github.com/SkYNewZ/sos-vpdive/internal/members/memberstest"
)

func TestDeleteCaptureAndMessage(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	id, _ := e.submit(t, png())
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	require.NoError(t, e.store.MemberReply(ctx, id, "Voici mon certificat médical.", []Upload{png()}))
	d, err := e.store.Detail(ctx, id)
	require.NoError(t, err)
	formCapture, message := d.Captures[0].ID, d.Messages[0].ID
	require.Len(t, e.objects(t), 2)
	require.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM outbox WHERE message_id = ?`, message), "the club mail cites the message")

	require.NoError(t, e.apply(t, id, Command{Action: ActionDeleteCapture, AttachmentID: formCapture}))
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM attachments WHERE id = ?`, formCapture))
	assert.Len(t, e.objects(t), 1)
	require.ErrorIs(t, e.apply(t, id, Command{Action: ActionDeleteCapture, AttachmentID: formCapture}), ErrNotFound)

	require.NoError(t, e.apply(t, id, Command{Action: ActionDeleteMessage, MessageID: message}))
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM messages WHERE id = ?`, message))
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM attachments WHERE ticket_id = ?`, id))
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM outbox WHERE message_id = ?`, message), "its pending mail is gone")
	assert.Empty(t, e.objects(t))
	require.ErrorIs(t, e.apply(t, id, Command{Action: ActionDeleteMessage, MessageID: message}), ErrNotFound)

	d, err = e.store.Detail(ctx, id)
	require.NoError(t, err)
	types := make([]string, 0, len(d.Events))
	for _, ev := range d.Events {
		types = append(types, ev.Type)
	}
	assert.Equal(t, []string{"submitted", "taken", "capture_deleted", "message_deleted"}, types)
	assert.NotContains(t, e.logs.String(), "certificat")
}

func TestDeleteTicket(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	id, ref := e.submit(t, png())
	token := tokenOf(t, e.mails(t))
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	require.NoError(t, e.store.MemberReply(ctx, id, "Précision", []Upload{png()}))
	require.NoError(t, e.apply(t, id, Command{Action: ActionDelete, Actor: "bob"}))

	for _, table := range []string{"messages", "attachments", "events", "outbox"} {
		assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM `+table+` WHERE ticket_id = ?`, id), table)
	}
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM tickets`))
	assert.Empty(t, e.objects(t))
	_, err := e.store.ByToken(ctx, token)
	require.ErrorIs(t, err, ErrNotFound, "the tracking link stops working")
	assert.Equal(t, Change{Type: ChangeDeleted, TicketID: id}, e.recorded()[len(e.recorded())-1])
	logs := e.logs.String()
	assert.Contains(t, logs, `"msg":"ticket deleted"`)
	assert.Contains(t, logs, `"actor":"bob"`)
	assert.NotContains(t, logs, ref)
	assert.NotContains(t, logs, memberAddress)
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM stats_monthly`), "an open request leaves no stats")
}

func TestDeletingADoneTicketKeepsItsStats(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	e.clock.advance(5 * time.Hour)
	require.NoError(t, e.apply(t, id, Command{Action: ActionClose}))
	require.NoError(t, e.apply(t, id, Command{Action: ActionDelete}))
	var (
		month, category string
		count, hours    int
	)
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		`SELECT month, category, closed_count, hours_to_close_total FROM stats_monthly`).Scan(&month, &category, &count, &hours))
	assert.Equal(t, "2026-09", month)
	assert.Equal(t, "carnet", category)
	assert.Equal(t, 1, count)
	assert.Equal(t, 5, hours)
}

func TestDeleteRefusesAStaleVersion(t *testing.T) {
	e := newTestStore(t)
	id, _ := e.submit(t)
	stale := e.version(t, id)
	require.NoError(t, e.apply(t, id, Command{Action: ActionTake}))
	err := e.store.Apply(context.Background(), Command{Action: ActionDelete, TicketID: id, Version: stale, Actor: "alice"})
	require.ErrorIs(t, err, ErrStale)
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM tickets`))
}

func TestErase(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	memberstest.Import(t, e.members, "members_valid.xlsx")
	e.submit(t, png())
	first, _ := e.submit(t)
	require.NoError(t, e.apply(t, first, Command{Action: ActionClose}))
	other := submission(t)
	other.Email = "hugo.bernard@example.org"
	_, err := e.store.Submit(ctx, other)
	require.NoError(t, err)
	require.NoError(t, e.store.SendLinks(ctx, memberAddress))

	preview, err := e.store.PreviewErasure(ctx, " Lea.Martin@example.org")
	require.NoError(t, err)
	assert.Equal(t, Erasure{Tickets: 2, Member: true}, preview)

	done, err := e.store.Erase(ctx, " Lea.Martin@example.org", "alice")
	require.NoError(t, err)
	assert.Equal(t, preview, done)
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM tickets WHERE email_hash = ?`, e.keys.Hash(memberAddress)))
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM tickets`), "other addresses are untouched")
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM outbox WHERE recipient_hash = ?`, e.keys.Hash(memberAddress)),
		"the lost-link mail goes too")
	found, err := e.members.Lookup(ctx, memberAddress)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, e.objects(t))
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM stats_monthly`), "the closed request is still counted")
	assert.Contains(t, e.logs.String(), `"msg":"person erased"`)
	assert.NotContains(t, e.logs.String(), memberAddress)

	again, err := e.store.PreviewErasure(ctx, memberAddress)
	require.NoError(t, err)
	assert.Equal(t, Erasure{}, again)
	_, err = e.store.Erase(ctx, "two words@example.org", "alice")
	require.ErrorIs(t, err, ErrInvalid)
}

func TestReleaseMissing(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	inProgress, _ := e.submit(t)
	waiting, _ := e.submit(t)
	kept, _ := e.submit(t)
	closed, _ := e.submit(t)
	require.NoError(t, e.apply(t, inProgress, Command{Action: ActionTake, Actor: "bob"}))
	require.NoError(t, e.apply(t, waiting, Command{Action: ActionTake, Actor: "bob"}))
	require.NoError(t, e.apply(t, waiting, Command{Action: ActionWait, Actor: "bob"}))
	require.NoError(t, e.apply(t, kept, Command{Action: ActionTake, Actor: "alice"}))
	require.NoError(t, e.apply(t, closed, Command{Action: ActionClose, Actor: "bob"}))
	e.mails(t)

	e.removeAccount("bob")
	known := func(u string) bool { _, ok := e.account(u); return ok }
	require.NoError(t, e.store.ReleaseMissing(ctx, known))
	for _, id := range []int64{inProgress, waiting} {
		status, assignee := e.status(t, id)
		assert.Equal(t, StatusTodo, status)
		assert.Empty(t, assignee)
		assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM events WHERE ticket_id = ? AND type = 'released' AND actor = 'system'`, id))
	}
	status, assignee := e.status(t, kept)
	assert.Equal(t, StatusInProgress, status)
	assert.Equal(t, "alice", assignee)
	status, assignee = e.status(t, closed)
	assert.Equal(t, StatusDone, status, "done requests keep their resolver")
	assert.Equal(t, "bob", assignee)

	msgs := e.mails(t)
	require.Len(t, msgs, 2)
	for _, m := range msgs {
		assert.Equal(t, clubAddress, m.To)
		assert.Contains(t, m.Subject, "remise à traiter")
		assert.Contains(t, m.Text, "était suivie par bob")
	}
	require.NoError(t, e.store.ReleaseMissing(ctx, known))
	assert.Empty(t, e.mails(t), "a second pass changes nothing")
}

func TestPurge(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	draft, _ := e.submit(t, png())
	_, err := e.db.ExecContext(ctx, `UPDATE tickets SET status = 'draft', ref = NULL, submitted_at = NULL WHERE id = ?`, draft)
	require.NoError(t, err)
	open, _ := e.submit(t)
	closed, _ := e.submit(t)
	require.NoError(t, e.apply(t, closed, Command{Action: ActionClose}))

	e.clock.advance(23 * time.Hour)
	require.NoError(t, e.store.Purge(ctx))
	assert.Equal(t, 3, e.count(t, `SELECT COUNT(*) FROM tickets`), "a draft lives 24 hours")

	e.clock.advance(2 * time.Hour)
	require.NoError(t, e.store.Purge(ctx))
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM tickets WHERE id = ?`, draft))
	assert.Empty(t, e.objects(t), "the draft's captures are deleted")

	e.clock.advance(363 * 24 * time.Hour) // just under RetentionDays after closing
	require.NoError(t, e.store.Purge(ctx))
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM tickets WHERE id = ?`, closed))

	e.clock.advance(2 * 24 * time.Hour)
	require.NoError(t, e.store.Purge(ctx))
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM tickets WHERE id = ?`, closed))
	assert.Equal(t, 1, e.count(t, `SELECT closed_count FROM stats_monthly WHERE month = '2026-09' AND category = 'carnet'`))
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM tickets WHERE id = ?`, open), "open requests are never deleted")

	idle, err := e.store.Idle(ctx)
	require.NoError(t, err)
	require.Len(t, idle, 1)
	assert.Equal(t, open, idle[0].ID)
	status, _ := e.status(t, open)
	assert.Equal(t, StatusTodo, status, "nor closed")
}

func TestIdleIgnoresRecentActivity(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	id, _ := e.submit(t)
	e.clock.advance(364 * 24 * time.Hour)
	require.NoError(t, e.store.MemberReply(ctx, id, "Toujours là ?", nil))
	e.clock.advance(2 * 24 * time.Hour)
	idle, err := e.store.Idle(ctx)
	require.NoError(t, err)
	assert.Empty(t, idle)
}

// agedBlobs reports chosen modification times, to test the 24-hour grace.
type agedBlobs struct {
	blobs.Store

	modified map[string]time.Time
}

func (a *agedBlobs) List(ctx context.Context) ([]blobs.Object, error) {
	list, err := a.Store.List(ctx)
	for i, o := range list {
		if at, ok := a.modified[o.Key]; ok {
			list[i].Modified = at
		}
	}
	return list, err
}

func TestSweepOrphans(t *testing.T) {
	aged := &agedBlobs{modified: map[string]time.Time{}}
	e := newTestStore(t, func(d *Deps) {
		aged.Store = d.Blobs
		d.Blobs = aged
	})
	ctx := context.Background()
	e.submit(t, png())
	used := e.objects(t)[0].Key
	require.NoError(t, e.blobs.Put(ctx, "orphan-old-object", []byte("x")))
	require.NoError(t, e.blobs.Put(ctx, "orphan-recent-object", []byte("y")))
	now := e.clock.now()
	aged.modified[used] = now.Add(-48 * time.Hour)
	aged.modified["orphan-old-object"] = now.Add(-25 * time.Hour)
	aged.modified["orphan-recent-object"] = now.Add(-23 * time.Hour)

	require.NoError(t, e.store.SweepOrphans(ctx))
	objects := e.objects(t)
	keys := make([]string, 0, len(objects))
	for _, o := range objects {
		keys = append(keys, o.Key)
	}
	assert.ElementsMatch(t, []string{used, "orphan-recent-object"}, keys)
}
