package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Querier is what *sql.DB and *sql.Tx share for reads.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Execer is what *sql.DB and *sql.Tx share for writes.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// NullIfZero maps the zero value to NULL, for optional columns kept as plain
// Go values.
func NullIfZero[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

// UnixOrNull stores a time as Unix seconds, NULL when zero.
func UnixOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

// UnixTime reads Unix seconds back as a UTC time, zero when NULL.
func UnixTime(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

// Collect scans every row of a query result into a slice. It takes the
// (rows, err) pair QueryContext returns, closes the rows and reports the
// iteration error, so callers only write the per-row scan.
func Collect[T any](rows *sql.Rows, err error, scan func(*sql.Rows) (T, error)) (out []T, rerr error) {
	if err != nil {
		return nil, err
	}
	defer func() { rerr = errors.Join(rerr, rows.Close()) }()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
