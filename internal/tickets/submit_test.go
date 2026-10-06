package tickets

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/blobs"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

func TestSubmitConfirmsAndQueuesTwoMails(t *testing.T) {
	e := newTestStore(t)
	id, ref := e.submit(t, png())
	assert.Equal(t, "CPP-0001", ref)

	status, assignee := e.status(t, id)
	assert.Equal(t, StatusTodo, status)
	assert.Empty(t, assignee)
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM attachments WHERE ticket_id = ? AND message_id IS NULL`, id))
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM events WHERE ticket_id = ? AND type = 'submitted' AND actor = 'member'`, id))
	assert.Equal(t, []Change{{Type: ChangeCreated, TicketID: id}}, e.recorded())

	msgs := e.mails(t)
	require.Len(t, msgs, 2)
	byTo := map[string]mail.Message{msgs[0].To: msgs[0], msgs[1].To: msgs[1]}
	member, club := byTo[memberAddress], byTo[clubAddress]
	assert.Equal(t, "Demande CPP-0001 bien reçue", member.Subject)
	assert.Contains(t, member.Text, "Bonjour Léa,")
	assert.Contains(t, member.Text, "Ne réponds pas à ce mail")
	token := tokenOf(t, msgs)

	assert.Equal(t, "Nouvelle demande CPP-0001", club.Subject)
	assert.Contains(t, club.Text, "Demandeur : Léa Martin")
	assert.Contains(t, club.Text, "Catégorie : "+e.store.Catalog.CategoryLabel("carnet"))
	assert.Contains(t, club.Text, "https://comite.example.org/demandes/")
	assert.NotContains(t, club.Text, token, "the club never receives the secret link")
	assert.NotContains(t, club.Text, "/suivi/")
}

func TestSubmitSealsPersonalData(t *testing.T) {
	e := newTestStore(t)
	capture := png()
	id, _ := e.submit(t, capture)
	token := tokenOf(t, e.mails(t))

	row := e.db.QueryRowContext(context.Background(), `SELECT first_name, last_name, email, fields, description, token FROM tickets WHERE id = ?`, id)
	cols := make([][]byte, 6)
	require.NoError(t, row.Scan(&cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5]))
	for _, plain := range []string{"Léa", "Martin", memberAddress, "carnet", testBody, token} {
		for _, c := range cols {
			assert.NotContains(t, string(c), plain)
		}
	}

	objects := e.objects(t)
	require.Len(t, objects, 1)
	stored, err := e.blobs.Get(context.Background(), objects[0].Key)
	require.NoError(t, err)
	assert.False(t, bytes.Contains(stored, capture.Data), "objects are sealed before upload")
	opened, err := e.keys.Open(stored)
	require.NoError(t, err)
	assert.Equal(t, capture.Data, opened)
}

func TestSubmitTwiceWithTheSameFormKey(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	sub := submission(t)
	first, err := e.store.Submit(ctx, sub)
	require.NoError(t, err)
	second, err := e.store.Submit(ctx, sub)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM tickets`))

	ref, found, err := e.store.Resubmitted(ctx, sub.FormKey)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, first, ref)
	_, found, err = e.store.Resubmitted(ctx, "another key")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Len(t, e.mails(t), 2, "one acknowledgement and one club mail, not two of each")
}

func TestResubmittedConfirmsALeftoverDraft(t *testing.T) {
	e := newTestStore(t)
	ctx := context.Background()
	sub := submission(t)
	first, err := e.store.Submit(ctx, sub)
	require.NoError(t, err)
	id := e.idOf(t, first)
	// Simulate a crash between the draft and its confirmation.
	_, err = e.db.ExecContext(context.Background(), `UPDATE tickets SET status = 'draft', ref = NULL, submitted_at = NULL WHERE id = ?`, id)
	require.NoError(t, err)
	_, err = e.db.ExecContext(context.Background(), `DELETE FROM outbox`)
	require.NoError(t, err)

	ref, found, err := e.store.Resubmitted(ctx, sub.FormKey)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "CPP-0002", ref, "a confirmation draws a new reference")
	status, _ := e.status(t, id)
	assert.Equal(t, StatusTodo, status)
	assert.Len(t, e.mails(t), 2)
}

func TestConcurrentSubmitsWithOneFormKeyCreateOneTicket(t *testing.T) {
	e := newTestStore(t)
	sub := submission(t)
	refs := make([]string, 5)
	errs := make([]error, 5)
	var wg sync.WaitGroup
	for i := range refs {
		wg.Go(func() { refs[i], errs[i] = e.store.Submit(context.Background(), sub) })
	}
	wg.Wait()
	for i := range refs {
		require.NoError(t, errs[i])
		assert.Equal(t, refs[0], refs[i])
	}
	assert.Equal(t, 1, e.count(t, `SELECT COUNT(*) FROM tickets`))
	assert.Len(t, e.mails(t), 2)
}

func TestReferenceIsNeverReused(t *testing.T) {
	e := newTestStore(t)
	e.submit(t)
	_, second := e.submit(t)
	_, err := e.db.ExecContext(context.Background(), `DELETE FROM tickets WHERE ref = ?`, second)
	require.NoError(t, err)
	_, third := e.submit(t)
	assert.Equal(t, "CPP-0003", third)
}

func TestTicketIdIsNeverReused(t *testing.T) {
	e := newTestStore(t)
	e.submit(t)
	second, _ := e.submit(t)
	_, err := e.db.ExecContext(context.Background(), `DELETE FROM tickets WHERE id = ?`, second)
	require.NoError(t, err)
	third, _ := e.submit(t)
	assert.NotEqual(t, second, third, "a stale page must not reach another request")
}

func TestConfirmIsIdempotent(t *testing.T) {
	e := newTestStore(t)
	id, ref := e.submit(t)
	again, err := e.store.Confirm(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, ref, again)
	assert.Len(t, e.mails(t), 2)
	assert.Len(t, e.recorded(), 1)
}

// failingBlobs fails every Put after the first ok ones.
type failingBlobs struct {
	blobs.Store

	mu sync.Mutex
	ok int
}

func (f *failingBlobs) Put(ctx context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ok == 0 {
		return errors.New("bucket unreachable")
	}
	f.ok--
	return f.Store.Put(ctx, key, data)
}

func TestStorageFailureStoresNothing(t *testing.T) {
	var fb *failingBlobs
	e := newTestStore(t, func(d *Deps) {
		fb = &failingBlobs{Store: d.Blobs, ok: 1}
		d.Blobs = fb
	})
	sub := submission(t)
	sub.Captures = []Upload{png(), png()}
	_, err := e.store.Submit(context.Background(), sub)
	require.ErrorIs(t, err, ErrStorage)
	assert.Equal(t, 0, e.count(t, `SELECT COUNT(*) FROM tickets`))
	assert.Empty(t, e.objects(t), "the first upload was removed")
	assert.Empty(t, e.mails(t))
}

func TestSubmitRefusesInvalidInput(t *testing.T) {
	e := newTestStore(t)
	sub := submission(t)
	sub.FormKey = ""
	_, err := e.store.Submit(context.Background(), sub)
	require.ErrorIs(t, err, ErrInvalid)
	sub = submission(t)
	sub.Captures = []Upload{png(), png(), png(), png()}
	_, err = e.store.Submit(context.Background(), sub)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestEveryMailTemplateRenders(t *testing.T) {
	d := mailData{
		FirstName: "Léa", Ref: "CPP-0001", Category: "Carnet", Name: "Léa Martin", Link: "https://example.org/x",
		Resolver: "Alice, présidente", Excerpt: "Bonjour", Waiting: true, Former: "bob",
		Links: []refLink{{Ref: "CPP-0001", Link: "https://example.org/x"}},
	}
	for _, ev := range []mail.Event{
		mail.EventSubmitted, mail.EventNewTicket, mail.EventTaken, mail.EventReplied, mail.EventWaiting,
		mail.EventClosed, mail.EventMemberReplied, mail.EventLostLink, mail.EventReleased,
	} {
		var b bytes.Buffer
		require.NoError(t, mailTemplates.ExecuteTemplate(&b, string(ev)+".txt", d), ev)
		assert.Contains(t, b.String(), "Ne réponds pas à ce mail", ev)
		assert.NotContains(t, subject(ev, d.Ref), "Léa", "subjects never carry member text")
	}
}
