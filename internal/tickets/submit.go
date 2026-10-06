package tickets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Upload is a screenshot already checked by images.Sanitize.
type Upload struct {
	Data []byte
	MIME string
}

// Submission is a request as the member's form sends it.
type Submission struct {
	FormKey     string
	FirstName   string
	LastName    string
	Email       string // normalized
	Fields      Fields // Fields.Category is the chosen category
	Description string // cleaned
	Captures    []Upload
}

// Outcome is what the member form shows once a request is sent (spec §3.2).
type Outcome struct {
	Ref   string // the request is filed: its confirmation
	Token string // otherwise screen 2, behind this draft token
}

// Suggestion is the model's answer: fiches to show and a summary for the
// committee (spec §5.2, §5.6).
type Suggestion struct {
	KBIDs   []string
	Summary string
}

// Chooser asks the model about the stored draft; ok is false when it did not
// answer in time, or not at all.
type Chooser func(ctx context.Context) (s Suggestion, ok bool)

// errFormKeyTaken reports a form key that already has a request.
var errFormKeyTaken = errors.New("form key already used")

// Resubmitted tells what a form sent again with formKey finds (spec §3.2): a
// filed request shows its confirmation; a draft still on screen 2 gets a new
// token, the old page being lost; a draft left before the model's answer, or
// past its 24 hours, is confirmed.
func (s *Store) Resubmitted(ctx context.Context, formKey string) (Outcome, bool, error) {
	var (
		id         int64
		status     Status
		created    int64
		draftToken []byte
	)
	err := s.DB.QueryRowContext(ctx, `SELECT id, status, created_at, draft_token_hash FROM tickets WHERE form_key_hash = ?`,
		secure.TokenHash(formKey)).Scan(&id, &status, &created, &draftToken)
	if errors.Is(err, sql.ErrNoRows) {
		return Outcome{}, false, nil
	}
	if err != nil {
		return Outcome{}, false, fmt.Errorf("find form key: %w", err)
	}
	if status == StatusDraft && draftToken != nil && !s.draftExpired(created) {
		token, err := s.newDraftToken(ctx, s.DB, id)
		if err != nil || token != "" {
			return Outcome{Token: token}, true, err
		}
	}
	ref, err := s.Confirm(ctx, id)
	return Outcome{Ref: ref}, true, err
}

// Submit uploads the captures and stores the draft, then asks choose (nil
// without a model). Fiches lead to screen 2; otherwise the request is
// confirmed at once. ErrStorage when an upload fails (nothing is stored).
func (s *Store) Submit(ctx context.Context, sub Submission, choose Chooser) (Outcome, error) {
	id, err := s.draft(ctx, sub)
	if errors.Is(err, errFormKeyTaken) {
		out, found, err := s.Resubmitted(ctx, sub.FormKey)
		if err == nil && !found {
			err = errors.New("form key conflict without a ticket")
		}
		return out, err
	}
	if err != nil {
		return Outcome{}, err
	}
	if choose != nil {
		if sg, ok := choose(ctx); ok {
			return s.suggested(ctx, id, sg)
		}
	}
	ref, err := s.Confirm(ctx, id)
	return Outcome{Ref: ref}, err
}

// draft stores a request as a draft with its captures. errFormKeyTaken when
// a concurrent post with the same form key won.
func (s *Store) draft(ctx context.Context, sub Submission) (int64, error) {
	if sub.FormKey == "" || len(sub.Captures) > MaxCapturesPerPost {
		return 0, ErrInvalid
	}
	fields, err := json.Marshal(sub.Fields)
	if err != nil {
		return 0, fmt.Errorf("encode fields: %w", err)
	}
	token, err := secure.NewToken()
	if err != nil {
		return 0, err
	}
	keys, err := s.upload(ctx, sub.Captures)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.tx(ctx, "draft", func(ctx context.Context, tx *sql.Tx) error {
		now := s.Now().Unix()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO tickets (token_hash, token, form_key_hash, email_hash, category, status, created_at, updated_at,
			  first_name, last_name, email, fields, description)
			 VALUES (?, ?, ?, ?, ?, 'draft', ?, ?, ?, ?, ?, ?, ?)`,
			secure.TokenHash(token), s.Keys.SealString(token), secure.TokenHash(sub.FormKey), s.Keys.Hash(sub.Email),
			sub.Fields.Category, now, now, s.Keys.SealString(sub.FirstName), s.Keys.SealString(sub.LastName),
			s.Keys.SealString(sub.Email), s.Keys.Seal(fields), s.Keys.SealString(sub.Description))
		if err != nil {
			return fmt.Errorf("insert draft: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("insert draft: %w", err)
		}
		return s.insertAttachments(ctx, tx, id, 0, sub.Captures, keys)
	})
	if err != nil {
		s.deleteObjects(ctx, keys)
		if isUniqueViolation(err) {
			return 0, errFormKeyTaken
		}
		return 0, err
	}
	return id, nil
}

// Confirm moves a draft to todo, draws its reference and queues the member
// acknowledgement and the club mail. Confirming twice returns the same ref.
func (s *Store) Confirm(ctx context.Context, id int64) (string, error) {
	var (
		ref     string
		created bool
	)
	err := s.tx(ctx, "confirm", func(ctx context.Context, tx *sql.Tx) error {
		t, err := s.load(ctx, tx, id)
		if err != nil {
			return err
		}
		if t.status != StatusDraft {
			ref = t.ref
			return nil
		}
		if ref, err = nextRef(ctx, tx); err != nil {
			return err
		}
		now := s.Now().Unix()
		if _, err := tx.ExecContext(ctx,
			`UPDATE tickets SET status = 'todo', ref = ?, submitted_at = ?, updated_at = ?, version = version + 1
			 WHERE id = ? AND status = 'draft'`, ref, now, now, id); err != nil {
			return fmt.Errorf("confirm ticket: %w", err)
		}
		t.ref, t.status = ref, StatusTodo
		if err := s.addEvent(ctx, tx, id, eventSubmitted, ActorMember, nil); err != nil {
			return err
		}
		if err := s.memberMail(ctx, tx, t, 0, mail.EventSubmitted, mailData{}); err != nil {
			return err
		}
		created = true
		return s.clubMail(ctx, tx, t, 0, mail.EventNewTicket, mailData{})
	})
	if err != nil {
		return "", err
	}
	if created {
		s.changed(ChangeCreated, id)
	}
	return ref, nil
}

// nextRef draws the next reference from meta.last_ref. A deleted request
// never gives its number back (spec §3.3).
func nextRef(ctx context.Context, tx *sql.Tx) (string, error) {
	var raw []byte
	n := 0
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'last_ref'`).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", fmt.Errorf("read last ref: %w", err)
	default:
		if n, err = strconv.Atoi(string(raw)); err != nil {
			return "", fmt.Errorf("last ref %q: %w", raw, err)
		}
	}
	n++
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('last_ref', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		[]byte(strconv.Itoa(n))); err != nil {
		return "", fmt.Errorf("write last ref: %w", err)
	}
	return fmt.Sprintf("CPP-%04d", n), nil
}

// upload seals and stores captures before any row is written (spec §9.8).
// On failure the objects already sent are removed and ErrStorage returned.
func (s *Store) upload(ctx context.Context, captures []Upload) ([]string, error) {
	keys := make([]string, 0, len(captures))
	for _, c := range captures {
		key, err := secure.NewToken()
		if err == nil {
			err = s.Blobs.Put(ctx, key, s.Keys.Seal(c.Data))
		}
		if err != nil {
			s.Logger.WarnContext(ctx, "capture upload failed", "error", err)
			s.deleteObjects(ctx, keys)
			return nil, fmt.Errorf("%w: %w", ErrStorage, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// insertAttachments writes the rows of uploaded captures; messageID 0 means
// sent with the form.
func (s *Store) insertAttachments(ctx context.Context, tx *sql.Tx, ticketID, messageID int64, captures []Upload, keys []string) error {
	for i, c := range captures {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attachments (ticket_id, message_id, mime, size, object_key, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			ticketID, store.NullIfZero(messageID), c.MIME, len(c.Data), keys[i], s.Now().Unix()); err != nil {
			return fmt.Errorf("insert attachment: %w", err)
		}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
