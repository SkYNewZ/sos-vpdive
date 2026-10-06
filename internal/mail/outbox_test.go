package mail

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeSender records delivered messages; when err is set, every send fails.
type fakeSender struct {
	mu    sync.Mutex
	err   error
	calls int
	sent  []Message
}

func (f *fakeSender) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeSender) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeSender) messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.sent...)
}

func (f *fakeSender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// senderFunc adapts a function to Sender.
type senderFunc func(context.Context, Message) error

func (f senderFunc) Send(ctx context.Context, m Message) error { return f(ctx, m) }

type testOutbox struct {
	*Outbox

	db    *sql.DB
	clock *testClock
}

func newTestOutbox(t *testing.T) *testOutbox {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	keys, err := secure.NewKeys(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	clock := &testClock{t: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)}
	return &testOutbox{Outbox: NewOutbox(db, keys, clock.now), db: db, clock: clock}
}

func (o *testOutbox) enqueue(t *testing.T, m Mail) int64 {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.Tx(ctx, o.db, "test.enqueue", func(ctx context.Context, tx *sql.Tx) error {
		return o.Enqueue(ctx, tx, m)
	}))
	var id int64
	require.NoError(t, o.db.QueryRowContext(ctx, `SELECT max(id) FROM outbox`).Scan(&id))
	return id
}

type outboxRow struct {
	status   string
	attempts int
	next     int64
	giveUp   int64
}

func (o *testOutbox) row(t *testing.T, id int64) outboxRow {
	t.Helper()
	var r outboxRow
	require.NoError(t, o.db.QueryRowContext(context.Background(),
		`SELECT status, attempts, next_attempt_at, give_up_at FROM outbox WHERE id = ?`, id).
		Scan(&r.status, &r.attempts, &r.next, &r.giveUp))
	return r
}

func (o *testOutbox) count(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, o.db.QueryRowContext(context.Background(), `SELECT count(*) FROM outbox`).Scan(&n))
	return n
}

// untilNextAttempt moves the clock to the next attempt of mail id.
func (o *testOutbox) untilNextAttempt(t *testing.T, id int64) {
	t.Helper()
	o.clock.advance(time.Unix(o.row(t, id).next, 0).Sub(o.clock.now()))
}

// insertTicket adds a minimal confirmed request and one committee message,
// with placeholder sealed columns: the outbox only needs their ids.
func (o *testOutbox) insertTicket(t *testing.T) (ticketID, messageID int64) {
	t.Helper()
	ctx := context.Background()
	res, err := o.db.ExecContext(ctx,
		`INSERT INTO tickets (ref, token_hash, token, form_key_hash, email_hash, category, status,
		                      created_at, submitted_at, updated_at, first_name, last_name, email, fields, description)
		 VALUES ('CPP-0001', X'01', X'02', X'03', X'04', 'autre', 'todo', 1, 2, 3, X'10', X'11', X'12', X'13', X'14')`)
	require.NoError(t, err)
	ticketID, err = res.LastInsertId()
	require.NoError(t, err)
	res, err = o.db.ExecContext(ctx,
		`INSERT INTO messages (ticket_id, author_type, author, internal, created_at, body)
		 VALUES (?, 'admin', 'alice', 0, 1, X'00')`, ticketID)
	require.NoError(t, err)
	messageID, err = res.LastInsertId()
	require.NoError(t, err)
	return ticketID, messageID
}

const witnessToken = "witness-token-123"

func sampleMail() Mail {
	return Mail{
		Event:   EventSubmitted,
		To:      " Lea.Martin@Example.org ",
		Subject: "Ta demande CPP-0042 est bien reçue",
		Text:    "Bonjour Léa,\n\nSuis ta demande ici : https://sos.example.org/suivi/" + witnessToken,
	}
}

func TestRetryDelay(t *testing.T) {
	want := []time.Duration{
		time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour,
		24 * time.Hour, 24 * time.Hour, 24 * time.Hour,
	}
	for i, d := range want {
		assert.Equal(t, d, retryDelay(i+1), "after attempt %d", i+1)
	}
}

func TestEventLabels(t *testing.T) {
	for _, e := range []Event{
		EventSubmitted, EventNewTicket, EventTaken, EventReplied, EventWaiting,
		EventClosed, EventMemberReplied, EventLostLink, EventReleased,
	} {
		assert.NotEqual(t, string(e), e.Label(), e)
	}
}

func TestEnqueueSealsAndDelivers(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	id := o.enqueue(t, sampleMail())

	var sealed, hash []byte
	require.NoError(t, o.db.QueryRowContext(ctx,
		`SELECT recipient || subject || body, recipient_hash FROM outbox WHERE id = ?`, id).Scan(&sealed, &hash))
	for _, witness := range []string{"lea.martin", "CPP-0042", witnessToken, "Léa"} {
		assert.NotContains(t, string(sealed), witness)
	}
	assert.Equal(t, o.keys.Hash("lea.martin@example.org"), hash)

	s := &fakeSender{}
	sent, err := o.SendDue(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, 1, sent)
	m := sampleMail()
	assert.Equal(t, []Message{{Channel: ChannelEmail, To: "lea.martin@example.org", Subject: m.Subject, Text: m.Text}}, s.messages())
	r := o.row(t, id)
	assert.Equal(t, "sent", r.status)
	assert.Equal(t, 1, r.attempts)

	sent, err = o.SendDue(ctx, s)
	require.NoError(t, err)
	assert.Zero(t, sent)
	assert.Equal(t, 1, s.callCount(), "a sent mail is never sent again")
}

func TestTemporaryFailureFollowsSchedule(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	s := &fakeSender{}
	s.fail(errors.New("dial tcp: connection refused"))
	id := o.enqueue(t, sampleMail())

	_, err := o.SendDue(ctx, s)
	require.NoError(t, err)
	r := o.row(t, id)
	assert.Equal(t, "pending", r.status)
	assert.Equal(t, 1, r.attempts)
	assert.Equal(t, o.clock.now().Add(time.Minute).Unix(), r.next)

	_, err = o.SendDue(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, 1, s.callCount(), "not due before its next attempt")

	o.untilNextAttempt(t, id)
	_, err = o.SendDue(ctx, s)
	require.NoError(t, err)
	r = o.row(t, id)
	assert.Equal(t, 2, r.attempts)
	assert.Equal(t, o.clock.now().Add(5*time.Minute).Unix(), r.next)
}

func TestThreeDayOutageThenDelivery(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	s := &fakeSender{}
	s.fail(errors.New("dial tcp: connection refused"))
	id := o.enqueue(t, sampleMail())

	start := o.clock.now()
	for o.clock.now().Sub(start) < 72*time.Hour {
		_, err := o.SendDue(ctx, s)
		require.NoError(t, err)
		require.Equal(t, "pending", o.row(t, id).status)
		o.untilNextAttempt(t, id)
	}
	s.fail(nil)
	sent, err := o.SendDue(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, 1, sent)
	assert.Equal(t, "sent", o.row(t, id).status)
}

func TestGivesUpAfterSevenDays(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	s := &fakeSender{}
	s.fail(errors.New("451 try again later"))
	id := o.enqueue(t, sampleMail())

	start := o.clock.now()
	for range 50 {
		_, err := o.SendDue(ctx, s)
		require.NoError(t, err)
		if o.row(t, id).status != "pending" {
			break
		}
		o.untilNextAttempt(t, id)
	}
	assert.Equal(t, "failed", o.row(t, id).status)
	elapsed := o.clock.now().Sub(start)
	assert.Equal(t, 7*24*time.Hour, elapsed, "the last wait ends with the window, not a day later")

	failed, err := o.Failed(ctx)
	require.NoError(t, err)
	require.Len(t, failed, 1)
	assert.Equal(t, Failed{
		ID: id, Event: EventSubmitted, To: "lea.martin@example.org",
		CreatedAt: start, FailedAt: o.clock.now(),
	}, failed[0])
	n, err := o.FailedCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestSendThatOutlastsTheWindowFailsTheMail(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	id := o.enqueue(t, sampleMail())
	o.clock.advance(7*24*time.Hour - time.Minute)
	s := &fakeSender{}
	s.fail(errors.New("451 try again later"))
	slow := senderFunc(func(ctx context.Context, m Message) error {
		o.clock.advance(2 * time.Minute) // the relay answers after the window closed
		return s.Send(ctx, m)
	})

	_, err := o.SendDue(ctx, slow)
	require.NoError(t, err)
	r := o.row(t, id)
	assert.Equal(t, "failed", r.status, "not postponed past the window")
	assert.Equal(t, 1, r.attempts)
	assert.Equal(t, 1, s.callCount())
}

func TestPermanentFailureFailsAtOnceAndNamesTheRequest(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	ticketID, _ := o.insertTicket(t)
	s := &fakeSender{}
	s.fail(fmt.Errorf("%w: 550 no such user", ErrPermanent))
	id := o.enqueue(t, Mail{TicketID: ticketID, Event: EventTaken, To: "lea.martin@example.org", Subject: "s", Text: "t"})

	_, err := o.SendDue(ctx, s)
	require.NoError(t, err)
	r := o.row(t, id)
	assert.Equal(t, "failed", r.status)
	assert.Equal(t, 1, r.attempts)
	failed, err := o.Failed(ctx)
	require.NoError(t, err)
	require.Len(t, failed, 1)
	assert.Equal(t, "CPP-0001", failed[0].Ref)
	assert.Equal(t, ticketID, failed[0].TicketID)
}

func TestRetryOpensANewWindow(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	s := &fakeSender{}
	s.fail(fmt.Errorf("%w: 550 no such user", ErrPermanent))
	id := o.enqueue(t, sampleMail())
	_, err := o.SendDue(ctx, s)
	require.NoError(t, err)

	require.ErrorIs(t, o.Retry(ctx, id+1), ErrNotFound)
	o.clock.advance(time.Hour)
	require.NoError(t, o.Retry(ctx, id))
	r := o.row(t, id)
	assert.Equal(t, outboxRow{
		status: "pending", attempts: 0,
		next: o.clock.now().Unix(), giveUp: o.clock.now().Add(7 * 24 * time.Hour).Unix(),
	}, r)
	select {
	case <-o.wake[ChannelEmail]:
	default:
		t.Fatal("Retry did not wake the worker")
	}
	require.ErrorIs(t, o.Retry(ctx, id), ErrNotFound, "a pending mail is not retried")

	s.fail(nil)
	sent, err := o.SendDue(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, 1, sent)
}

func TestDeletingAMessageOrARequestDropsItsMails(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	ticketID, messageID := o.insertTicket(t)
	o.enqueue(t, Mail{TicketID: ticketID, MessageID: messageID, Event: EventReplied,
		To: "lea.martin@example.org", Subject: "Réponse à ta demande CPP-0001", Text: "witness reply"})
	o.enqueue(t, Mail{TicketID: ticketID, Event: EventTaken,
		To: "lea.martin@example.org", Subject: "Ta demande CPP-0001 est prise en charge", Text: "t"})
	s := &fakeSender{}
	s.fail(errors.New("smtp down"))
	_, err := o.SendDue(ctx, s)
	require.NoError(t, err)

	_, err = o.db.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, messageID)
	require.NoError(t, err)
	assert.Equal(t, 1, o.count(t), "the mail citing the message is gone")
	_, err = o.db.ExecContext(ctx, `DELETE FROM tickets WHERE id = ?`, ticketID)
	require.NoError(t, err)
	assert.Zero(t, o.count(t), "the mails of the request are gone")

	s.fail(nil)
	o.clock.advance(time.Hour)
	sent, err := o.SendDue(ctx, s)
	require.NoError(t, err)
	assert.Zero(t, sent)
	assert.Empty(t, s.messages())
}

func TestOutboxIdIsNeverReused(t *testing.T) {
	o := newTestOutbox(t)
	first := o.enqueue(t, sampleMail())
	_, err := o.db.ExecContext(context.Background(), `DELETE FROM outbox WHERE id = ?`, first)
	require.NoError(t, err)
	assert.NotEqual(t, first, o.enqueue(t, sampleMail()), "an in-flight send must not finish another mail")
}

func TestMailDeletedDuringABatchIsNotSent(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	first := o.enqueue(t, sampleMail())
	second := o.enqueue(t, sampleMail())
	calls := 0
	s := senderFunc(func(ctx context.Context, _ Message) error {
		calls++
		_, err := o.db.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, second)
		return err
	})
	sent, err := o.SendDue(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, 1, sent)
	assert.Equal(t, 1, calls, "the deleted mail never reaches the sender")
	assert.Equal(t, "sent", o.row(t, first).status)
}

func TestExpiredMailFailsWithoutSending(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	s := &fakeSender{}
	id := o.enqueue(t, sampleMail())
	o.clock.advance(8 * 24 * time.Hour)
	sent, err := o.SendDue(ctx, s)
	require.NoError(t, err)
	assert.Zero(t, sent)
	assert.Zero(t, s.callCount())
	assert.Equal(t, "failed", o.row(t, id).status)
}

func TestDeleteRecipient(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	o.enqueue(t, sampleMail())
	o.enqueue(t, Mail{Event: EventLostLink, To: "lea.martin@example.org", Subject: "Tes liens de suivi", Text: "t"})
	o.enqueue(t, Mail{Event: EventNewTicket, To: "club@example.org", Subject: "Nouvelle demande CPP-0042", Text: "t"})

	require.NoError(t, store.Tx(ctx, o.db, "test.erase", func(ctx context.Context, tx *sql.Tx) error {
		return o.DeleteRecipient(ctx, tx, " LEA.martin@example.org")
	}))
	assert.Equal(t, 1, o.count(t))
}

func TestPurgeKeepsThirtyDays(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	o.enqueue(t, sampleMail())
	o.enqueue(t, Mail{Event: EventNewTicket, To: "club@example.org", Subject: "s", Text: "t"})
	s := senderFunc(func(_ context.Context, m Message) error {
		if m.To == "club@example.org" {
			return fmt.Errorf("%w: 550 mailbox unavailable", ErrPermanent)
		}
		return nil
	})
	_, err := o.SendDue(ctx, s)
	require.NoError(t, err)

	o.clock.advance(30*24*time.Hour - time.Second)
	require.NoError(t, o.Purge(ctx))
	assert.Equal(t, 2, o.count(t))
	o.clock.advance(2 * time.Second)
	require.NoError(t, o.Purge(ctx))
	assert.Zero(t, o.count(t))
}

func TestSendSpanCarriesNoPersonalData(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	o := newTestOutbox(t)
	id := o.enqueue(t, sampleMail())
	_, err := o.SendDue(context.Background(), &fakeSender{})
	require.NoError(t, err)

	found := false
	for _, s := range rec.Ended() {
		for _, a := range s.Attributes() {
			for _, witness := range []string{"lea.martin", witnessToken, "CPP-0042"} {
				assert.NotContains(t, a.Value.String(), witness)
			}
		}
		if s.Name() == "outbox.send" {
			found = true
			assert.Contains(t, s.Attributes(), attribute.Int64("outbox.id", id))
		}
	}
	assert.True(t, found, "one span per sent mail")
}

func TestRunDeliversOnWakeAndStops(t *testing.T) {
	o := newTestOutbox(t)
	s := &fakeSender{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.Run(ctx, s, slog.New(slog.DiscardHandler))
	}()

	o.enqueue(t, sampleMail())
	o.Wake()
	require.Eventually(t, func() bool { return len(s.messages()) == 1 }, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestRunSendsMailsWhileAnAlertHangs(t *testing.T) {
	o := newTestOutbox(t)
	ticketID, _ := o.insertTicket(t)
	alertID := o.enqueue(t, pushAlert(ticketID, ChannelWebPush)) // first in line
	mailID := o.enqueue(t, sampleMail())
	release := make(chan struct{})
	answer := sync.OnceFunc(func() { close(release) })
	mails := &fakeSender{}
	router := Router{ChannelEmail: mails, ChannelWebPush: senderFunc(func(context.Context, Message) error {
		<-release // a push service that does not answer
		return nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.Run(ctx, router, slog.New(slog.DiscardHandler))
	}()
	t.Cleanup(func() { answer(); cancel(); <-done })

	require.Eventually(t, func() bool { return len(mails.messages()) == 1 }, 2*time.Second, 10*time.Millisecond,
		"the mail does not wait for the alert")
	assert.Equal(t, "sent", o.row(t, mailID).status)
	answer()
	require.Eventually(t, func() bool { return o.row(t, alertID).status == "sent" }, 2*time.Second, 10*time.Millisecond)
}

func TestOutcomeIsRecordedWhenShutdownCancelsDuringSend(t *testing.T) {
	o := newTestOutbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &fakeSender{}
	id := o.enqueue(t, sampleMail())

	_, err := o.SendDue(ctx, senderFunc(func(ctx context.Context, m Message) error {
		cancel() // SIGTERM while the relay is accepting the mail
		return s.Send(ctx, m)
	}))
	require.NoError(t, err)
	assert.Equal(t, "sent", o.row(t, id).status)

	_, err = o.SendDue(context.Background(), s)
	require.NoError(t, err)
	assert.Equal(t, 1, s.callCount(), "no duplicate at the next start")
}

func TestPostponedMailLogsTheFailureStageOnly(t *testing.T) {
	o := newTestOutbox(t)
	_, smtpSender := startSMTP(t, config.SMTPImplicit, fakeOptions{authCode: 535})
	o.enqueue(t, sampleMail())
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	_, err := o.sendDue(context.Background(), ChannelEmail, smtpSender, logger)
	require.NoError(t, err)
	assert.Contains(t, logs.String(), "stage=smtp_auth")
	assert.NotContains(t, logs.String(), "example.org")
}

// pushAlert is a committee alert as tickets queues it.
func pushAlert(ticketID int64, ch Channel) Mail {
	return Mail{TicketID: ticketID, Event: EventNewTicket, Channel: ch, Subject: "Nouvelle demande", Text: "CPP-0001 · Autre"}
}

func TestPushAlertGetsOneAttemptWithinTheHour(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	ticketID, _ := o.insertTicket(t)
	id := o.enqueue(t, pushAlert(ticketID, ChannelWebPush))
	var (
		channel string
		hash    []byte
	)
	require.NoError(t, o.db.QueryRowContext(ctx, `SELECT channel, recipient_hash FROM outbox WHERE id = ?`, id).Scan(&channel, &hash))
	assert.Equal(t, "webpush", channel)
	assert.Empty(t, hash, "an alert has no recipient: erasure by address never matches it")
	assert.Equal(t, o.clock.now().Add(time.Hour).Unix(), o.row(t, id).giveUp)

	s := &fakeSender{}
	s.fail(errors.New("dial tcp: connection refused"))
	_, err := o.SendDue(ctx, s)
	require.NoError(t, err)
	r := o.row(t, id)
	assert.Equal(t, "failed", r.status, "no retry for a push alert")
	assert.Equal(t, 1, r.attempts)

	s.fail(nil)
	late := o.enqueue(t, pushAlert(ticketID, ChannelPushover))
	o.clock.advance(time.Hour + time.Minute)
	_, err = o.SendDue(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, "failed", o.row(t, late).status, "an alert an hour late is not sent")
	assert.Equal(t, 1, s.callCount())

	fresh := o.enqueue(t, pushAlert(ticketID, ChannelPushover))
	_, err = o.SendDue(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, "sent", o.row(t, fresh).status)
	assert.Equal(t, []Message{{Channel: ChannelPushover, TicketID: ticketID, Subject: "Nouvelle demande", Text: "CPP-0001 · Autre"}}, s.messages())

	failed, err := o.Failed(ctx)
	require.NoError(t, err)
	assert.Empty(t, failed, "the Envois page lists mails only")
	n, err := o.FailedCount(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
	require.ErrorIs(t, o.Retry(ctx, id), ErrNotFound, "an alert is never sent again")

	_, err = o.db.ExecContext(ctx, `DELETE FROM tickets WHERE id = ?`, ticketID)
	require.NoError(t, err)
	assert.Zero(t, o.count(t), "alerts leave with their request")
}

func TestEnqueueChecksTheRecipientOfTheChannel(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	withTo := pushAlert(0, ChannelPushover)
	withTo.To = "club@example.org"
	noTo := sampleMail()
	noTo.To = ""
	for _, m := range []Mail{withTo, noTo, {Event: EventNewTicket, Channel: "sms", Subject: "s", Text: "t"}} {
		err := store.Tx(ctx, o.db, "test.enqueue", func(ctx context.Context, tx *sql.Tx) error {
			return o.Enqueue(ctx, tx, m)
		})
		require.Error(t, err, m.Channel)
	}
	assert.Zero(t, o.count(t))
}

func TestRouterSendsThroughTheChannelSender(t *testing.T) {
	o := newTestOutbox(t)
	ctx := context.Background()
	mails, alerts := &fakeSender{}, &fakeSender{}
	router := Router{ChannelEmail: mails, ChannelPushover: alerts}
	mailID := o.enqueue(t, sampleMail())
	alertID := o.enqueue(t, pushAlert(0, ChannelPushover))
	offID := o.enqueue(t, pushAlert(0, ChannelWebPush))

	sent, err := o.SendDue(ctx, router)
	require.NoError(t, err)
	assert.Equal(t, 2, sent)
	assert.Equal(t, 1, mails.callCount())
	assert.Equal(t, 1, alerts.callCount())
	assert.Equal(t, "sent", o.row(t, mailID).status)
	assert.Equal(t, "sent", o.row(t, alertID).status)
	assert.Equal(t, "failed", o.row(t, offID).status, "a channel configured away fails at once")
	require.ErrorIs(t, router.Send(ctx, Message{Channel: ChannelWebPush}), ErrPermanent)
}
