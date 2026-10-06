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

// Resubmitted returns the reference of the request already created with
// formKey, confirming it first if it was left as a draft (spec §3.2).
func (s *Store) Resubmitted(ctx context.Context, formKey string) (ref string, found bool, err error) {
	var (
		id     int64
		status Status
		stored sql.NullString
	)
	err = s.DB.QueryRowContext(ctx, `SELECT id, status, ref FROM tickets WHERE form_key_hash = ?`,
		secure.TokenHash(formKey)).Scan(&id, &status, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find form key: %w", err)
	}
	if status != StatusDraft {
		return stored.String, true, nil
	}
	ref, err = s.Confirm(ctx, id)
	return ref, true, err
}

// Submit uploads the captures, stores the draft, then confirms it. A UNIQUE
// conflict on the form key falls back to Resubmitted. ErrStorage when an
// upload fails (nothing is stored).
func (s *Store) Submit(ctx context.Context, sub Submission) (string, error) {
	if sub.FormKey == "" || len(sub.Captures) > MaxCapturesPerPost {
		return "", ErrInvalid
	}
	fields, err := json.Marshal(sub.Fields)
	if err != nil {
		return "", fmt.Errorf("encode fields: %w", err)
	}
	token, err := secure.NewToken()
	if err != nil {
		return "", err
	}
	keys, err := s.upload(ctx, sub.Captures)
	if err != nil {
		return "", err
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
			ref, found, rerr := s.Resubmitted(ctx, sub.FormKey)
			if rerr == nil && !found {
				rerr = fmt.Errorf("form key conflict without a ticket: %w", err)
			}
			return ref, rerr
		}
		return "", err
	}
	return s.Confirm(ctx, id)
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
