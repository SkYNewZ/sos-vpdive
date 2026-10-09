package imports

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Result is the outcome of a run of an import (design 2026-10-09).
type Result string

// Run results.
const (
	Imported  Result = "imported"  // the data in place was replaced
	Unchanged Result = "unchanged" // a push with the bytes of the latest import
	Refused   Result = "refused"   // the service refused the file
	Failed    Result = "failed"    // the script could not complete the run
)

// Run limits: the detail of a failure reported by the script is cut, and
// runs are purged after RunsRetention.
const (
	MaxDetail     = 200
	RunsRetention = 90 * 24 * time.Hour
)

// Run is one row of the runs journal: a manual import, a push, a refusal or
// a failure reported by the script. Code is the refusal code or the failure
// class, Detail the refusal message or the script's failure text.
type Run struct {
	Kind    Kind
	At      time.Time
	By      string // the account, or ScriptAuthor
	Result  Result
	Code    string
	Detail  string
	Rows    int
	Skipped int
}

// Record journals r through q.
func Record(ctx context.Context, q store.Execer, r Run) error {
	if _, err := q.ExecContext(ctx,
		`INSERT INTO import_runs (kind, at, by, result, code, detail, row_count, skipped_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(r.Kind), r.At.Unix(), r.By, string(r.Result), r.Code, r.Detail, r.Rows, r.Skipped); err != nil {
		return fmt.Errorf("journal %s run: %w", r.Kind, err)
	}
	return nil
}

// Runs returns the runs of kind at or after since, the latest first.
func Runs(ctx context.Context, q store.Querier, kind Kind, since time.Time) ([]Run, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT at, by, result, code, detail, row_count, skipped_count FROM import_runs
		 WHERE kind = ? AND at >= ? ORDER BY id DESC`, string(kind), since.Unix())
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (Run, error) {
		var (
			r  = Run{Kind: kind}
			at int64
		)
		err := rows.Scan(&at, &r.By, &r.Result, &r.Code, &r.Detail, &r.Rows, &r.Skipped)
		r.At = time.Unix(at, 0).UTC()
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("list %s runs: %w", kind, err)
	}
	return out, nil
}

// PurgeRuns deletes the runs older than RunsRetention at now.
func PurgeRuns(ctx context.Context, db *sql.DB, now time.Time) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM import_runs WHERE at < ?`, now.Add(-RunsRetention).Unix()); err != nil {
		return fmt.Errorf("purge import runs: %w", err)
	}
	return nil
}
