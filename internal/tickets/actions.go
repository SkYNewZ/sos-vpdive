package tickets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

// Action is a committee action on a request (route POST /demandes/{id}/actions).
type Action string

// Committee actions.
const (
	ActionTake       Action = "take"
	ActionReassign   Action = "reassign"
	ActionUnassign   Action = "unassign"
	ActionWait       Action = "wait"
	ActionResume     Action = "resume"
	ActionReply      Action = "reply"
	ActionReplyWait  Action = "reply_wait"
	ActionReplyClose Action = "reply_close"
	ActionNote       Action = "note"
	ActionCategory   Action = "category"
	ActionClose      Action = "close"
)

// allowedFrom is the matrix of spec §8.1: the statuses each committee action
// starts from. An action absent from this table is refused.
var allowedFrom = map[Action][]Status{
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

// Command is one committee action as the ticket page posts it.
type Command struct {
	Action       Action
	TicketID     int64
	Version      int64  // as displayed; ErrStale when it moved
	Actor        string // signed-in username
	Assignee     string // reassign
	Body         string // reply, note: cleaned by the caller
	Category     string // category
	AttachmentID int64  // delete_capture
	MessageID    int64  // delete_message
}

// outcome is what an action leaves to do once committed.
type outcome struct {
	change  ChangeType
	objects []string // stored captures to delete
}

// Apply runs one committee action in one transaction (ticket, journal,
// messages, mails), then publishes the change. ErrStale, ErrNotAllowed,
// ErrInvalid, ErrNotFound.
func (s *Store) Apply(ctx context.Context, cmd Command) error {
	from, ok := allowedFrom[cmd.Action]
	if !ok {
		return ErrInvalid
	}
	var res outcome
	err := s.tx(ctx, string(cmd.Action), func(ctx context.Context, tx *sql.Tx) error {
		t, err := s.load(ctx, tx, cmd.TicketID)
		switch {
		case err != nil:
			return err
		case t.status == StatusDraft:
			return ErrNotFound
		case t.version != cmd.Version:
			return ErrStale
		case !slices.Contains(from, t.status):
			return ErrNotAllowed
		}
		res, err = s.apply(ctx, tx, t, cmd)
		return err
	})
	if err != nil {
		return err
	}
	s.deleteObjects(ctx, res.objects)
	s.changed(res.change, cmd.TicketID)
	return nil
}

// apply performs an allowed action on t, inside tx.
func (s *Store) apply(ctx context.Context, tx *sql.Tx, t ticketRow, cmd Command) (outcome, error) {
	updated := outcome{change: ChangeUpdated}
	after := t
	switch cmd.Action {
	case ActionTake:
		after.status, after.assignee = StatusInProgress, cmd.Actor
		if err := s.transition(ctx, tx, t, after, cmd.Actor, eventTaken, nil); err != nil {
			return outcome{}, err
		}
		return updated, s.memberMail(ctx, tx, after, 0, mail.EventTaken, mailData{Resolver: s.resolverText(cmd.Actor)})
	case ActionReassign:
		if _, known := s.Account(cmd.Assignee); !known || cmd.Assignee == t.assignee {
			return outcome{}, ErrInvalid
		}
		after.assignee = cmd.Assignee
		return updated, s.transition(ctx, tx, t, after, cmd.Actor, eventReassigned,
			map[string]string{dataFrom: t.assignee, dataTo: cmd.Assignee})
	case ActionUnassign:
		after.status, after.assignee = StatusTodo, ""
		return updated, s.transition(ctx, tx, t, after, cmd.Actor, eventUnassigned, map[string]string{dataFrom: t.assignee})
	case ActionWait:
		after.status = StatusWaiting
		if err := s.transition(ctx, tx, t, after, cmd.Actor, eventWaiting, nil); err != nil {
			return outcome{}, err
		}
		return updated, s.memberMail(ctx, tx, after, 0, mail.EventWaiting, mailData{})
	case ActionResume:
		after.status = StatusInProgress
		return updated, s.transition(ctx, tx, t, after, cmd.Actor, eventResumed, nil)
	case ActionReply, ActionReplyWait, ActionReplyClose:
		return updated, s.reply(ctx, tx, t, cmd)
	case ActionNote:
		if cmd.Body == "" {
			return outcome{}, ErrInvalid
		}
		if _, err := s.insertMessage(ctx, tx, t.id, cmd.Actor, true, cmd.Body); err != nil {
			return outcome{}, err
		}
		return updated, s.save(ctx, tx, t, t)
	case ActionCategory:
		if _, known := s.Catalog.Category(cmd.Category); !known || cmd.Category == t.category {
			return outcome{}, ErrInvalid
		}
		after.category = cmd.Category
		return updated, s.transition(ctx, tx, t, after, cmd.Actor, eventCategoryChanged,
			map[string]string{dataFrom: t.category, dataTo: cmd.Category})
	case ActionClose:
		after = s.closed(t, cmd.Actor)
		if err := s.transition(ctx, tx, t, after, cmd.Actor, eventClosed, nil); err != nil {
			return outcome{}, err
		}
		return updated, s.memberMail(ctx, tx, after, 0, mail.EventClosed, mailData{})
	default:
		return outcome{}, ErrInvalid
	}
}

// transition saves after and journals the change.
func (s *Store) transition(ctx context.Context, tx *sql.Tx, before, after ticketRow, actor string, typ eventType, data map[string]string) error {
	if err := s.save(ctx, tx, before, after); err != nil {
		return err
	}
	return s.addEvent(ctx, tx, before.id, typ, actor, data)
}

// closed is t once closed by actor, who becomes its resolver if it had none.
func (s *Store) closed(t ticketRow, actor string) ticketRow {
	t.status, t.closedAt = StatusDone, s.Now().Unix()
	if t.assignee == "" {
		t.assignee = actor
	}
	return t
}

// reply adds a resolver's answer and, for reply_wait and reply_close, the
// status change: one action, one mail (spec §6).
func (s *Store) reply(ctx context.Context, tx *sql.Tx, t ticketRow, cmd Command) error {
	if cmd.Body == "" {
		return ErrInvalid
	}
	msgID, err := s.insertMessage(ctx, tx, t.id, cmd.Actor, false, cmd.Body)
	if err != nil {
		return err
	}
	after, typ := t, eventType("")
	d := mailData{Resolver: s.resolverText(cmd.Actor), Excerpt: truncate(cmd.Body, replyExcerpt)}
	if cmd.Action == ActionReplyWait {
		d.Waiting = true
		if t.status == StatusInProgress {
			after.status, typ = StatusWaiting, eventWaiting
		}
	}
	if cmd.Action == ActionReplyClose {
		d.Closed = true
		after, typ = s.closed(t, cmd.Actor), eventClosed
	}
	if err := s.save(ctx, tx, t, after); err != nil {
		return err
	}
	if typ != "" {
		if err := s.addEvent(ctx, tx, t.id, typ, cmd.Actor, nil); err != nil {
			return err
		}
	}
	return s.memberMail(ctx, tx, after, msgID, mail.EventReplied, d)
}

// canReply applies spec §8.1: open, or closed for at most ReplyWindow.
func (s *Store) canReply(status Status, closedAt time.Time) bool {
	return status.Open() || (status == StatusDone && !s.Now().After(closedAt.Add(ReplyWindow)))
}

// MemberReply adds a member message with captures (spec §8.1 rows "Réponse de
// l'adhérent"). ErrNotAllowed after ReplyWindow, ErrTooManyCaptures, ErrStorage.
func (s *Store) MemberReply(ctx context.Context, id int64, body string, captures []Upload) error {
	if body == "" {
		return ErrInvalid
	}
	if len(captures) > MaxCapturesPerPost {
		return ErrTooManyCaptures
	}
	// Check before uploading, then again inside the transaction.
	if err := s.memberMayReply(ctx, s.DB, id, len(captures)); err != nil {
		return err
	}
	keys, err := s.upload(ctx, captures)
	if err != nil {
		return err
	}
	err = s.tx(ctx, "member_reply", func(ctx context.Context, tx *sql.Tx) error {
		if err := s.memberMayReply(ctx, tx, id, len(captures)); err != nil {
			return err
		}
		t, err := s.load(ctx, tx, id)
		if err != nil {
			return err
		}
		return s.memberReply(ctx, tx, t, body, captures, keys)
	})
	if err != nil {
		s.deleteObjects(ctx, keys)
		return err
	}
	s.changed(ChangeReplied, id)
	return nil
}

func (s *Store) memberReply(ctx context.Context, tx *sql.Tx, t ticketRow, body string, captures []Upload, keys []string) error {
	msgID, err := s.insertMessage(ctx, tx, t.id, "", false, body)
	if err != nil {
		return err
	}
	if err := s.insertAttachments(ctx, tx, t.id, msgID, captures, keys); err != nil {
		return err
	}
	after, typ, data := t, eventType(""), map[string]string(nil)
	if t.status == StatusWaiting {
		after.status, typ = StatusInProgress, eventMemberReplied
	}
	if t.status == StatusDone {
		// Reopened: back to its resolver if they still have an account (spec §8.1).
		after.status, after.closedAt = StatusInProgress, 0
		if _, known := s.Account(t.assignee); !known {
			after.status, after.assignee = StatusTodo, ""
		}
		typ, data = eventReopened, map[string]string{dataStatus: string(after.status)}
	}
	if err := s.save(ctx, tx, t, after); err != nil {
		return err
	}
	if typ != "" {
		if err := s.addEvent(ctx, tx, t.id, typ, actorMember, data); err != nil {
			return err
		}
	}
	return s.clubMail(ctx, tx, after, msgID, mail.EventMemberReplied, mailData{Reopened: t.status == StatusDone})
}

// memberMayReply checks the reply window and the per-request capture limit.
func (s *Store) memberMayReply(ctx context.Context, q querier, id int64, captures int) error {
	var (
		status   Status
		closedAt sql.NullInt64
		stored   int
	)
	err := q.QueryRowContext(ctx,
		`SELECT status, closed_at, (SELECT COUNT(*) FROM attachments WHERE ticket_id = tickets.id) FROM tickets WHERE id = ?`, id).
		Scan(&status, &closedAt, &stored)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read reply state: %w", err)
	}
	switch {
	case status == StatusDraft:
		return ErrNotFound
	case !s.canReply(status, unixTime(closedAt)):
		return ErrNotAllowed
	case stored+captures > MaxCapturesPerTicket:
		return ErrTooManyCaptures
	}
	return nil
}

// MemberClose lets the member mark the request as settled. No mail.
func (s *Store) MemberClose(ctx context.Context, id int64) error {
	err := s.tx(ctx, "member_close", func(ctx context.Context, tx *sql.Tx) error {
		t, err := s.load(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case t.status == StatusDraft:
			return ErrNotFound
		case !t.status.Open():
			return ErrNotAllowed
		}
		after := t
		after.status, after.closedAt = StatusDone, s.Now().Unix()
		return s.transition(ctx, tx, t, after, actorMember, eventClosedByMember, nil)
	})
	if err != nil {
		return err
	}
	s.changed(ChangeUpdated, id)
	return nil
}
