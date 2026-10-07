package push

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// unusedFor is how long a subscription stays without a successful push (spec §8.3).
const unusedFor = 90 * 24 * time.Hour

// Store keeps the committee's push subscriptions (table push_subscriptions),
// endpoint and keys sealed. A subscription belongs to a session: deleting
// the session (logout, revocation, expiry) deletes it through the cascade.
type Store struct {
	db   *sql.DB
	keys *secure.Keys
	now  func() time.Time
}

// NewStore returns the subscriptions store; now is injectable for tests.
func NewStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *Store {
	return &Store{db: db, keys: keys, now: now}
}

var (
	// ErrNoSubscription reports a session without a push subscription.
	ErrNoSubscription = errors.New("no push subscription on this device")
	// errUnreadable reports a subscription that could not be decrypted.
	errUnreadable = errors.New("push subscription unreadable")
)

type sealedKeys struct {
	P256DH string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// Save records sub for a session. A session is one device: its previous
// subscription goes. The same browser subscribing again, after a new login,
// moves its row to the new session instead of adding one.
func (s *Store) Save(ctx context.Context, sessionHash []byte, username string, sub Subscription) error {
	keys, err := json.Marshal(sealedKeys{
		P256DH: base64.RawURLEncoding.EncodeToString(sub.P256DH),
		Auth:   base64.RawURLEncoding.EncodeToString(sub.Auth),
	})
	if err != nil {
		return fmt.Errorf("encode push keys: %w", err)
	}
	endpointHash, now := secure.TokenHash(sub.Endpoint), s.now().Unix()
	return store.Tx(ctx, s.db, "push.save", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM push_subscriptions WHERE session_token_hash = ? AND endpoint_hash != ?`,
			sessionHash, endpointHash); err != nil {
			return fmt.Errorf("replace push subscription: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO push_subscriptions (session_token_hash, username, endpoint_hash, created_at, last_used_at, endpoint, keys)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (endpoint_hash) DO UPDATE SET session_token_hash = excluded.session_token_hash,
			     username = excluded.username, last_used_at = excluded.last_used_at, keys = excluded.keys`,
			sessionHash, username, endpointHash, now, now, s.keys.SealString(sub.Endpoint), s.keys.Seal(keys)); err != nil {
			return fmt.Errorf("save push subscription: %w", err)
		}
		return nil
	})
}

// DeleteSession removes the subscription of a session (« Désactiver »).
func (s *Store) DeleteSession(ctx context.Context, sessionHash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE session_token_hash = ?`, sessionHash); err != nil {
		return fmt.Errorf("delete push subscription: %w", err)
	}
	return nil
}

// HasSession reports whether a session has a subscription.
func (s *Store) HasSession(ctx context.Context, sessionHash []byte) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM push_subscriptions WHERE session_token_hash = ?`, sessionHash).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find push subscription: %w", err)
	}
	return true, nil
}

// Session returns the subscription of a session, decrypted, or
// ErrNoSubscription.
func (s *Store) Session(ctx context.Context, sessionHash []byte) (Subscription, error) {
	var (
		id             int64
		endpoint, keys []byte
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, endpoint, keys FROM push_subscriptions WHERE session_token_hash = ?`,
		sessionHash).Scan(&id, &endpoint, &keys)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNoSubscription
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("find push subscription: %w", err)
	}
	sub, err := s.open(id, endpoint, keys)
	if err != nil {
		return Subscription{}, fmt.Errorf("%w: %w", errUnreadable, err)
	}
	return sub, nil
}

// List returns the subscriptions of live sessions, decrypted, for one alert.
// An expired session stays a day before the purge: it gets nothing. Sessions
// of accounts that changed are revoked, at reload and at startup, and their
// subscriptions go with them. The ids of rows that do not decrypt come apart:
// they must not cost the other devices their alert.
func (s *Store) List(ctx context.Context) (subs []Subscription, unreadable []int64, err error) {
	type sealedRow struct {
		id             int64
		endpoint, keys []byte
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.endpoint, p.keys FROM push_subscriptions p
		 JOIN sessions s ON s.token_hash = p.session_token_hash
		 WHERE s.expires_at > ? ORDER BY p.id`, s.now().Unix())
	sealed, err := store.Collect(rows, err, func(rows *sql.Rows) (r sealedRow, err error) {
		err = rows.Scan(&r.id, &r.endpoint, &r.keys)
		return r, err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	for _, r := range sealed {
		sub, err := s.open(r.id, r.endpoint, r.keys)
		if err != nil {
			unreadable = append(unreadable, r.id)
			continue
		}
		subs = append(subs, sub)
	}
	return subs, unreadable, nil
}

// Delete removes a subscription the push service no longer knows.
func (s *Store) Delete(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete push subscription %d: %w", id, err)
	}
	return nil
}

// Touch records a successful push.
func (s *Store) Touch(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions SET last_used_at = ? WHERE id = ?`, s.now().Unix(), id); err != nil {
		return fmt.Errorf("touch push subscription %d: %w", id, err)
	}
	return nil
}

// Purge deletes the subscriptions without a successful push for 90 days.
func (s *Store) Purge(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE last_used_at < ?`,
		s.now().Add(-unusedFor).Unix()); err != nil {
		return fmt.Errorf("purge push subscriptions: %w", err)
	}
	return nil
}

func (s *Store) open(id int64, endpoint, sealed []byte) (sub Subscription, err error) {
	sub.ID = id
	if sub.Endpoint, err = s.keys.OpenString(endpoint); err != nil {
		return sub, fmt.Errorf("endpoint: %w", err)
	}
	raw, err := s.keys.Open(sealed)
	if err != nil {
		return sub, fmt.Errorf("keys: %w", err)
	}
	var k sealedKeys
	if err = json.Unmarshal(raw, &k); err != nil {
		return sub, fmt.Errorf("keys: %w", err)
	}
	if sub.P256DH, err = base64.RawURLEncoding.DecodeString(k.P256DH); err != nil {
		return sub, fmt.Errorf("keys: %w", err)
	}
	if sub.Auth, err = base64.RawURLEncoding.DecodeString(k.Auth); err != nil {
		return sub, fmt.Errorf("keys: %w", err)
	}
	return sub, nil
}
