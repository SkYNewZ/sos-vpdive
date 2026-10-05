package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Login limits (spec §11.3): 10 failures per hour per address; per username,
// from the 5th consecutive failure, a wait of one minute doubled at each
// further failure, capped at one hour and reset by a success. The other
// limits of §11.3 use allow. Counters live in the database and survive a
// restart; their keys are HMACs because addresses and emails are personal
// data.
const (
	ipFailureLimit   = 10
	ipWindow         = time.Hour
	userFailureLimit = 5
	userBaseDelay    = time.Minute
	userMaxDelay     = time.Hour
	// counterRetention outlives the longest window (one day for lost links).
	counterRetention = 48 * time.Hour
)

type limiter struct {
	db   *sql.DB
	keys *secure.Keys
	now  func() time.Time
}

// hashedKey keeps the purpose of "purpose:value" readable and hides the value.
func (l *limiter) hashedKey(key string) string {
	purpose, _, _ := strings.Cut(key, ":")
	return purpose + ":" + l.keys.HashHex(key)
}

func (l *limiter) ipKey(ip netip.Addr) string {
	return l.hashedKey("login-ip:" + ip.String())
}

func (l *limiter) userKey(username string) string {
	return l.hashedKey("login-user:" + username)
}

// rowQuerier is what *sql.DB and *sql.Tx share.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// countInWindow counts one more event under the hashed key in a fixed window
// that starts with its first event, and returns the count. It is the single
// window rule of every limit.
func (l *limiter) countInWindow(ctx context.Context, q rowQuerier, key string, window time.Duration) (int, error) {
	now := l.now().Unix()
	windowEnd := now - int64(window/time.Second)
	var count int
	err := q.QueryRowContext(ctx,
		`INSERT INTO counters (key, window_start, count) VALUES (?, ?, 1)
		 ON CONFLICT (key) DO UPDATE SET
		   count = CASE WHEN window_start <= ? THEN 1 ELSE count + 1 END,
		   window_start = CASE WHEN window_start <= ? THEN excluded.window_start ELSE window_start END
		 RETURNING count`,
		key, now, windowEnd, windowEnd).Scan(&count)
	return count, err
}

// allow counts one more event under key ("purpose:value") and reports whether
// the count stays within limit.
func (l *limiter) allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	count, err := l.countInWindow(ctx, l.db, l.hashedKey(key), window)
	if err != nil {
		return false, fmt.Errorf("rate limit: %w", err)
	}
	return count <= limit, nil
}

// userDelay is the wait after count consecutive failures.
func userDelay(count int) time.Duration {
	d := userBaseDelay
	for i := userFailureLimit; i < count && d < userMaxDelay; i++ {
		d *= 2
	}
	return min(d, userMaxDelay)
}

// retryAfter returns how long a login from ip for username stays blocked;
// zero means allowed.
func (l *limiter) retryAfter(ctx context.Context, ip netip.Addr, username string) (time.Duration, error) {
	now := l.now()
	var wait time.Duration
	start, count, err := l.counter(ctx, l.ipKey(ip))
	if err != nil {
		return 0, err
	}
	if count >= ipFailureLimit {
		if end := time.Unix(start, 0).Add(ipWindow); end.After(now) {
			wait = end.Sub(now)
		}
	}
	last, count, err := l.counter(ctx, l.userKey(username))
	if err != nil {
		return 0, err
	}
	if count >= userFailureLimit {
		if end := time.Unix(last, 0).Add(userDelay(count)); end.After(now) {
			wait = max(wait, end.Sub(now))
		}
	}
	return wait, nil
}

// fail records a failed attempt: the address counts within a fixed one-hour
// window; the username counts consecutive failures, window_start holding the
// last one.
func (l *limiter) fail(ctx context.Context, ip netip.Addr, username string) error {
	return store.Tx(ctx, l.db, "login.fail", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := l.countInWindow(ctx, tx, l.ipKey(ip), ipWindow); err != nil {
			return fmt.Errorf("count address failure: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO counters (key, window_start, count) VALUES (?, ?, 1)
			 ON CONFLICT (key) DO UPDATE SET count = count + 1, window_start = excluded.window_start`,
			l.userKey(username), l.now().Unix()); err != nil {
			return fmt.Errorf("count username failure: %w", err)
		}
		return nil
	})
}

// succeed resets the consecutive failures of username.
func (l *limiter) succeed(ctx context.Context, username string) error {
	if _, err := l.db.ExecContext(ctx, `DELETE FROM counters WHERE key = ?`, l.userKey(username)); err != nil {
		return fmt.Errorf("reset login failures: %w", err)
	}
	return nil
}

// purge drops counters untouched for two days: they block nothing.
func (l *limiter) purge(ctx context.Context) error {
	cutoff := l.now().Add(-counterRetention).Unix()
	if _, err := l.db.ExecContext(ctx, `DELETE FROM counters WHERE window_start < ?`, cutoff); err != nil {
		return fmt.Errorf("purge counters: %w", err)
	}
	return nil
}

func (l *limiter) counter(ctx context.Context, key string) (start int64, count int, err error) {
	err = l.db.QueryRowContext(ctx, `SELECT window_start, count FROM counters WHERE key = ?`, key).Scan(&start, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read login counter: %w", err)
	}
	return start, count, nil
}
