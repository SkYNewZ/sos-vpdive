package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Login limits (spec §11.3): 10 failures per hour per address; per username,
// from the 5th consecutive failure, a wait of one minute doubled at each
// further failure, capped at one hour and reset by a success. Counters live
// in the database and survive a restart; their keys are HMACs because an
// address is personal data.
const (
	ipFailureLimit   = 10
	ipWindow         = time.Hour
	userFailureLimit = 5
	userBaseDelay    = time.Minute
	userMaxDelay     = time.Hour
	counterRetention = 24 * time.Hour
)

type limiter struct {
	db   *sql.DB
	keys *secure.Keys
	now  func() time.Time
}

func (l *limiter) ipKey(ip netip.Addr) string {
	return "login-ip:" + l.keys.HashHex("login-ip:"+ip.String())
}

func (l *limiter) userKey(username string) string {
	return "login-user:" + l.keys.HashHex("login-user:"+username)
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
	now := l.now().Unix()
	windowEnd := now - int64(ipWindow/time.Second)
	return store.Tx(ctx, l.db, "login.fail", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO counters (key, window_start, count) VALUES (?, ?, 1)
			 ON CONFLICT (key) DO UPDATE SET
			   count = CASE WHEN window_start <= ? THEN 1 ELSE count + 1 END,
			   window_start = CASE WHEN window_start <= ? THEN excluded.window_start ELSE window_start END`,
			l.ipKey(ip), now, windowEnd, windowEnd); err != nil {
			return fmt.Errorf("count address failure: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO counters (key, window_start, count) VALUES (?, ?, 1)
			 ON CONFLICT (key) DO UPDATE SET count = count + 1, window_start = excluded.window_start`,
			l.userKey(username), now); err != nil {
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

// purge drops login counters untouched for a day: they block nothing.
func (l *limiter) purge(ctx context.Context) error {
	cutoff := l.now().Add(-counterRetention).Unix()
	if _, err := l.db.ExecContext(ctx, `DELETE FROM counters WHERE key LIKE 'login-%' AND window_start < ?`, cutoff); err != nil {
		return fmt.Errorf("purge login counters: %w", err)
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
