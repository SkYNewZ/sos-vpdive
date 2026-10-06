package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

const (
	// pollInterval bounds the wait of a retry that falls due without Wake.
	pollInterval = 30 * time.Second
)

// retrySchedule is the wait after the n-th failed attempt (spec §6); the last
// entry repeats until the 7-day window ends.
var retrySchedule = [...]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour, 24 * time.Hour}

// retryDelay is the wait after the attempts-th failed attempt, attempts >= 1.
func retryDelay(attempts int) time.Duration {
	return retrySchedule[min(attempts, len(retrySchedule))-1]
}

// Run sends due mails until ctx ends, one loop per channel: at start, on
// Wake and every 30 s. Logs carry the outbox id and event, never the
// recipient or the body.
func (o *Outbox) Run(ctx context.Context, s Sender, logger *slog.Logger) {
	var wg sync.WaitGroup
	for ch, wake := range o.wake {
		wg.Go(func() { o.run(ctx, ch, wake, s, logger) })
	}
	wg.Wait()
}

func (o *Outbox) run(ctx context.Context, ch Channel, wake <-chan struct{}, s Sender, logger *slog.Logger) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if _, err := o.sendDue(ctx, ch, s, logger); err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "outbox pass failed", "channel", string(ch), "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

// SendDue sends every due pending mail once, channel after channel; tests
// call it directly.
func (o *Outbox) SendDue(ctx context.Context, s Sender) (int, error) {
	sent := 0
	for _, ch := range channels {
		n, err := o.sendDue(ctx, ch, s, slog.New(slog.DiscardHandler))
		sent += n
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// Router delivers each message through the sender of its channel.
type Router map[Channel]Sender

// errChannelOff reports an alert whose channel was configured away after
// it was queued.
var errChannelOff = fmt.Errorf("%w: channel not configured", ErrPermanent)

// Send implements Sender.
func (r Router) Send(ctx context.Context, m Message) error {
	s, ok := r[m.Channel]
	if !ok {
		return errChannelOff
	}
	return s.Send(ctx, m)
}

// queued is a pending mail, still sealed.
type queued struct {
	id       int64
	ticketID int64
	event    Event
	channel  Channel
	attempts int
	giveUpAt int64
	to       []byte
	subject  []byte
	body     []byte
}

// sendDue picks one due mail of ch at a time, right before sending it, so a
// mail deleted meanwhile is never sent. Every outcome moves the row out of "due".
func (o *Outbox) sendDue(ctx context.Context, ch Channel, s Sender, logger *slog.Logger) (int, error) {
	sent := 0
	for ctx.Err() == nil { // on shutdown the rest stays pending for the next start
		q, err := o.nextDue(ctx, ch)
		if errors.Is(err, sql.ErrNoRows) {
			return sent, nil
		}
		if err != nil {
			return sent, err
		}
		ok, err := o.deliver(ctx, s, logger, q)
		if err != nil {
			return sent, err
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

func (o *Outbox) nextDue(ctx context.Context, ch Channel) (q queued, err error) {
	var event string
	err = o.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(ticket_id, 0), event, attempts, give_up_at, recipient, subject, body FROM outbox
		 WHERE status = ? AND channel = ? AND next_attempt_at <= ? ORDER BY next_attempt_at, id LIMIT 1`,
		string(statusPending), string(ch), o.now().Unix()).
		Scan(&q.id, &q.ticketID, &event, &q.attempts, &q.giveUpAt, &q.to, &q.subject, &q.body)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return q, err
		}
		return q, fmt.Errorf("next due mail: %w", err)
	}
	q.event, q.channel = Event(event), ch
	return q, nil
}

// deliver sends one mail and records the outcome. A delivery failure is
// recorded, not returned: the error is for the database only. A row deleted
// meanwhile (its request or message was deleted) updates nothing. An alert
// gets one attempt (spec §6, §9.6): its failure is logged, never retried.
func (o *Outbox) deliver(ctx context.Context, s Sender, logger *slog.Logger, q queued) (bool, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "outbox.send", trace.WithAttributes(
		attribute.Int64("outbox.id", q.id), attribute.String("outbox.event", string(q.event))))
	defer span.End()

	if o.now().Unix() >= q.giveUpAt {
		telemetry.Fail(span, "delivery_expired")
		logger.WarnContext(ctx, "mail expired before sending", "outbox_id", q.id, "event", string(q.event),
			"attempts", q.attempts)
		return false, o.finish(ctx, q.id, statusFailed, q.attempts)
	}
	m, err := o.open(q)
	if err != nil {
		telemetry.Fail(span, "outbox_decrypt")
		logger.ErrorContext(ctx, "mail unreadable, marked failed", "outbox_id", q.id, "event", string(q.event))
		return false, o.finish(ctx, q.id, statusFailed, q.attempts)
	}
	sendErr := s.Send(ctx, m)
	// The outcome is recorded even if shutdown cancelled ctx during Send:
	// losing it would send the mail again at the next start.
	ctx = context.WithoutCancel(ctx)
	attempts := q.attempts + 1
	now := o.now()
	permanent := errors.Is(sendErr, ErrPermanent)
	switch {
	case sendErr == nil:
		return true, o.finish(ctx, q.id, statusSent, attempts)
	case permanent || q.channel != ChannelEmail:
		code := "delivery_failed"
		if errors.Is(sendErr, errChannelOff) {
			code = "channel_disabled"
		}
		telemetry.Fail(span, code)
		logger.WarnContext(ctx, "mail failed for good", "outbox_id", q.id, "event", string(q.event),
			"channel", string(q.channel), "attempts", attempts, "permanent", permanent, "stage", stageOf(sendErr))
		return false, o.finish(ctx, q.id, statusFailed, attempts)
	default:
		telemetry.Fail(span, "delivery_postponed")
		// The wait never outlasts the window: a mail due at give_up_at fails as
		// expired, in this pass when the send itself outlasted the window.
		next := time.Unix(min(now.Add(retryDelay(attempts)).Unix(), q.giveUpAt), 0)
		logger.WarnContext(ctx, "mail delivery postponed", "outbox_id", q.id, "event", string(q.event),
			"attempts", attempts, "next_attempt", next, "stage", stageOf(sendErr))
		if _, err := o.db.ExecContext(ctx,
			`UPDATE outbox SET attempts = ?, next_attempt_at = ? WHERE id = ? AND status = ?`,
			attempts, next.Unix(), q.id, string(statusPending)); err != nil {
			return false, fmt.Errorf("reschedule mail %d: %w", q.id, err)
		}
		return false, nil
	}
}

func (o *Outbox) open(q queued) (Message, error) {
	to, err := o.keys.OpenString(q.to)
	if err != nil {
		return Message{}, err
	}
	subject, err := o.keys.OpenString(q.subject)
	if err != nil {
		return Message{}, err
	}
	text, err := o.keys.OpenString(q.body)
	if err != nil {
		return Message{}, err
	}
	return Message{Channel: q.channel, TicketID: q.ticketID, To: to, Subject: subject, Text: text}, nil
}

// finish records the final status of a pending mail.
func (o *Outbox) finish(ctx context.Context, id int64, st outboxStatus, attempts int) error {
	query := `UPDATE outbox SET status = ?, attempts = ?, sent_at = ? WHERE id = ? AND status = ?`
	if st == statusFailed {
		query = `UPDATE outbox SET status = ?, attempts = ?, failed_at = ? WHERE id = ? AND status = ?`
	}
	if _, err := o.db.ExecContext(ctx, query, string(st), attempts, o.now().Unix(), id, string(statusPending)); err != nil {
		return fmt.Errorf("record mail %d as %s: %w", id, st, err)
	}
	return nil
}
