package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const historyLife = 7 * 24 * time.Hour

type transcript struct {
	Exchanges []Exchange `json:"exchanges"`
	People    []Person   `json:"people"`
}
type providerContext struct {
	History []Message `json:"history"`
	Emails  []string  `json:"emails"`
}

func (s *Store) encode(c Conversation) ([]byte, []byte, error) {
	visible, err := json.Marshal(transcript{c.Exchanges, c.People})
	if err != nil {
		return nil, nil, fmt.Errorf("encode transcript: %w", err)
	}
	model, err := json.Marshal(providerContext{c.History, c.Emails})
	if err != nil {
		return nil, nil, fmt.Errorf("encode context: %w", err)
	}
	return s.keys.Seal(visible), s.keys.Seal(model), nil
}
func (s *Store) create(ctx context.Context, c Conversation) error {
	visible, model, err := s.encode(c)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO assistant_conversations(id,account,ticket_id,created_at,seen_at,title,transcript,context) VALUES(?,?,?,?,?,?,?,?)`, c.ID, c.Account, c.TicketID, c.Created.UnixNano(), c.Seen.UnixNano(), s.keys.SealString(c.Title), visible, model)
	if err != nil {
		return fmt.Errorf("create conversation: %w", err)
	}
	return nil
}
func (s *Store) read(ctx context.Context, account, id string, resume bool) (Conversation, error) {
	var c Conversation
	var created, seen int64
	var title, visible, model []byte
	fields := `id,account,ticket_id,created_at,seen_at,revision,history_bytes,title,transcript`
	if resume {
		fields += `,context`
	}
	dest := []any{&c.ID, &c.Account, &c.TicketID, &created, &seen, &c.revision, &c.historyBytes, &title, &visible}
	if resume {
		dest = append(dest, &model)
	}
	err := s.db.QueryRowContext(ctx, `SELECT `+fields+` FROM assistant_conversations WHERE id=? AND account=? AND created_at>? AND question_count>0`, id, account, s.now().Add(-historyLife).UnixNano()).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("read conversation: %w", err)
	}
	c.Created, c.Seen = time.Unix(0, created), time.Unix(0, seen)
	if c.Title, err = s.keys.OpenString(title); err != nil {
		return Conversation{}, err
	}
	plain, err := s.keys.Open(visible)
	if err != nil {
		return Conversation{}, err
	}
	var t transcript
	if err = json.Unmarshal(plain, &t); err != nil {
		return Conversation{}, errors.New("invalid conversation transcript")
	}
	c.Exchanges, c.People = t.Exchanges, t.People
	if resume {
		if err = c.resumeError(s.now()); err != nil {
			return Conversation{}, err
		}
		plain, err = s.keys.Open(model)
		if err != nil {
			return Conversation{}, err
		}
		var p providerContext
		if err = json.Unmarshal(plain, &p); err != nil {
			return Conversation{}, errors.New("invalid conversation context")
		}
		c.History, c.Emails = p.History, p.Emails
	}
	return c, nil
}
func (s *Store) save(ctx context.Context, c Conversation) error {
	visible, model, err := s.encode(c)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE assistant_conversations SET seen_at=?,question_count=?,history_bytes=?,transcript=?,context=?,revision=revision+1 WHERE id=? AND account=? AND revision=? AND created_at>?`, c.Seen.UnixNano(), len(c.Exchanges), historySize(c.History), visible, model, c.ID, c.Account, c.revision, s.now().Add(-historyLife).UnixNano())
	if err != nil {
		return fmt.Errorf("save conversation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// EraseAllTx removes every archive in the same transaction as content erasure.
func EraseAllTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM assistant_conversations`)
	if err != nil {
		return fmt.Errorf("erase conversation history: %w", err)
	}
	return nil
}

// PurgeHistory deletes expired archives even when the assistant is disabled.
func PurgeHistory(ctx context.Context, db *sql.DB, now time.Time) error {
	_, err := db.ExecContext(ctx, `DELETE FROM assistant_conversations WHERE created_at<=? OR (question_count=0 AND created_at<=?)`, now.Add(-historyLife).UnixNano(), now.Add(-maxLife).UnixNano())
	if err != nil {
		return fmt.Errorf("purge conversations: %w", err)
	}
	return nil
}

// HistoryItem is the small, visible part of one archive; it holds no model context.
type HistoryItem struct {
	ID, Title     string
	Created, Seen time.Time
	CanResume     bool
}

// List returns one page of private titles, newest activity first. Reads do not refresh activity.
func (s *Store) List(ctx context.Context, account string, page int) ([]HistoryItem, bool, error) {
	if page < 0 || page > 1_000_000 {
		page = 0
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,title,created_at,seen_at,question_count,history_bytes FROM assistant_conversations WHERE account=? AND question_count>0 AND created_at>? ORDER BY seen_at DESC,rowid DESC LIMIT 31 OFFSET ?`, account, s.now().Add(-historyLife).UnixNano(), page*30)
	if err != nil {
		return nil, false, fmt.Errorf("list conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var items []HistoryItem
	for rows.Next() {
		var item HistoryItem
		var title []byte
		var created, seen int64
		var questions, size int
		if err = rows.Scan(&item.ID, &title, &created, &seen, &questions, &size); err != nil {
			return nil, false, err
		}
		item.Title, err = s.keys.OpenString(title)
		if err != nil {
			return nil, false, err
		}
		item.Created, item.Seen = time.Unix(0, created), time.Unix(0, seen)
		c := Conversation{Created: item.Created, Seen: item.Seen, historyBytes: size}
		item.CanResume = s.now().Before(c.Expires()) && questions < MaxQuestions && size <= maxHistory
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(items) > 30
	if more {
		items = items[:30]
	}
	return items, more, nil
}

// ClaimTitle makes at most one title attempt for a completed conversation.
func (s *Store) ClaimTitle(ctx context.Context, account, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE assistant_conversations SET title_attempted=1 WHERE id=? AND account=? AND question_count>0 AND title_attempted=0 AND created_at>?`, id, account, s.now().Add(-historyLife).UnixNano())
	if err != nil {
		return false, fmt.Errorf("claim conversation title: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetTitle never inserts: a result arriving after erasure must not resurrect its row.
func (s *Store) SetTitle(ctx context.Context, account, id, title string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE assistant_conversations SET title=? WHERE id=? AND account=? AND title_attempted=1 AND created_at>?`, s.keys.SealString(title), id, account, s.now().Add(-historyLife).UnixNano())
	if err != nil {
		return fmt.Errorf("save conversation title: %w", err)
	}
	return nil
}
