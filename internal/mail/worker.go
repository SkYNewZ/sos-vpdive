package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/store"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

const (
	// pollInterval bounds the wait of a retry that falls due without Wake.
	pollInterval = 30 * time.Second
	// batchSize caps one pass; the next pass takes the rest.
	batchSize = 100
)

// retrySchedule is the wait after the n-th failed attempt (spec §6); the last
// entry repeats until the 7-day window ends.
var retrySchedule = [...]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour, 24 * time.Hour}

// retryDelay is the wait after the attempts-th failed attempt, attempts >= 1.
func retryDelay(attempts int) time.Duration {
	return retrySchedule[min(attempts, len(retrySchedule))-1]
}

// Run sends due mails until ctx ends: at start, on Wake and every 30 s.
// Logs carry the outbox id and event, never the recipient or the body.
func (o *Outbox) Run(ctx context.Context, s Sender, logger *slog.Logger) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if _, err := o.sendDue(ctx, s, logger); err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "outbox pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
		case <-ticker.C:
		}
	}
}

// SendDue sends every due pending mail once; tests call it directly.
func (o *Outbox) SendDue(ctx context.Context, s Sender) (int, error) {
	return o.sendDue(ctx, s, slog.New(slog.DiscardHandler))
}

// queued is a pending mail, still sealed.
type queued struct {
	id       int64
	event    Event
	attempts int
	giveUpAt int64
	to       []byte
	subject  []byte
	body     []byte
}

func (o *Outbox) sendDue(ctx context.Context, s Sender, logger *slog.Logger) (int, error) {
	due, err := o.due(ctx)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, q := range due {
		if err := ctx.Err(); err != nil {
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

func (o *Outbox) due(ctx context.Context) ([]queued, error) {
	rows, err := o.db.QueryContext(ctx,
		`SELECT id, event, attempts, give_up_at, recipient, subject, body FROM outbox
		 WHERE status = ? AND next_attempt_at <= ? ORDER BY next_attempt_at, id LIMIT ?`,
		string(statusPending), o.now().Unix(), batchSize)
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (q queued, err error) {
		var event string
		if err = rows.Scan(&q.id, &event, &q.attempts, &q.giveUpAt, &q.to, &q.subject, &q.body); err != nil {
			return q, err
		}
		q.event = Event(event)
		return q, nil
	})
	if err != nil {
		return nil, fmt.Errorf("due mails: %w", err)
	}
	return out, nil
}

// deliver sends one mail and records the outcome. A delivery failure is
// recorded, not returned: the error is for the database only. A row deleted
// meanwhile (its request or message was deleted) updates nothing.
func (o *Outbox) deliver(ctx context.Context, s Sender, logger *slog.Logger, q queued) (bool, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "outbox.send", trace.WithAttributes(
		attribute.Int64("outbox.id", q.id), attribute.String("outbox.event", string(q.event))))
	defer span.End()

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
	case permanent || now.Unix() >= q.giveUpAt:
		telemetry.Fail(span, "delivery_failed")
		logger.WarnContext(ctx, "mail failed for good", "outbox_id", q.id, "event", string(q.event),
			"attempts", attempts, "permanent", permanent, "stage", stageOf(sendErr))
		return false, o.finish(ctx, q.id, statusFailed, attempts)
	default:
		telemetry.Fail(span, "delivery_postponed")
		next := now.Add(retryDelay(attempts))
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

// stageOf names where an SMTP delivery failed, empty for senders that do not
// report a stage. Never the error text.
func stageOf(err error) string {
	stage, _ := failedStage(err)
	return string(stage)
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
	return Message{To: to, Subject: subject, Text: text}, nil
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
