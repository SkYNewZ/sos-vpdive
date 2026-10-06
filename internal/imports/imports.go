// Package imports holds what the members, payments and Mollie imports share
// (spec §7.2, §7.6): the imports journal, unconfirmed previews kept in
// memory, the replacement transaction with the guards of a pushed import, and
// the « ambiguë » mark of payment lines (§7.3).
package imports

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// Import errors.
var (
	ErrPreviewNotFound       = errors.New("import preview not found, already used or expired")
	ErrStale                 = errors.New("another import happened since the preview")
	ErrSecondConfirmRequired = errors.New("second confirmation required")
	ErrUnchanged             = errors.New("same file as the latest import of its kind")
	ErrTooFew                = errors.New("file holds less than half of the data in place")
)

// Kind is the kind column of the imports journal.
type Kind string

// Import kinds.
const (
	Members  Kind = "members"
	Payments Kind = "payments"
	Mollie   Kind = "vpaydive" // the VPayDive export, Mollie collections (spec §7.5)
)

// ScriptAuthor is the journal author of a pushed import (spec §7.6).
const ScriptAuthor = "script"

// inPlace counts the data a pushed import replaces: accounts for members,
// lines otherwise.
var inPlace = map[Kind]string{
	Members:  `SELECT COUNT(*) FROM members`,
	Payments: `SELECT COUNT(*) FROM payment_lines`,
	Mollie:   `SELECT COUNT(*) FROM online_payment_lines`,
}

// Limits are the workbook limits applied to every upload (spec §7.2): 50 MiB
// decompressed, 20 000 rows.
func Limits() xlsx.Limits {
	return xlsx.Limits{MaxUncompressed: 50 << 20, MaxRows: 20_000, MaxCells: 1_000_000}
}

// previewTTL is how long an unconfirmed preview lives in memory (spec §7.2).
const previewTTL = 15 * time.Minute

// Entry is one row of the imports journal. Zero times are stored as NULL.
type Entry struct {
	Kind       Kind
	ExportedAt time.Time // date of the export, when the file gives one
	PeriodFrom time.Time // payments: earliest and latest line of the file
	PeriodTo   time.Time
	ImportedAt time.Time
	ImportedBy string
	Rows       int
	Skipped    int
	FileHash   []byte // HMAC of the file; nil for imports made before lot 7
}

// Info is a journal row read back.
type Info struct {
	Entry

	ID int64
}

// Last returns the latest import of kind, if any.
func Last(ctx context.Context, q store.Querier, kind Kind) (Info, bool, error) {
	var (
		info               = Info{Kind: kind}
		exported, from, to sql.NullInt64
		imported           int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, exported_at, period_from, period_to, imported_at, imported_by, row_count, skipped_count, file_hash
		 FROM imports WHERE kind = ? ORDER BY id DESC LIMIT 1`, string(kind)).
		Scan(&info.ID, &exported, &from, &to, &imported, &info.ImportedBy, &info.Rows, &info.Skipped, &info.FileHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Info{}, false, nil
	}
	if err != nil {
		return Info{}, false, fmt.Errorf("last %s import: %w", kind, err)
	}
	info.ExportedAt, info.PeriodFrom, info.PeriodTo = store.UnixTime(exported), store.UnixTime(from), store.UnixTime(to)
	info.ImportedAt = time.Unix(imported, 0).UTC()
	return info, true, nil
}

// Replace confirms a preview in one transaction named span (spec §7.2): it
// refuses with ErrStale when an import of the same kind happened since the
// preview, journals e, lets fn replace the data, then marks the payment lines
// of homonyms.
func Replace(ctx context.Context, db *sql.DB, span string, m *Meta, e Entry,
	fn func(ctx context.Context, tx *sql.Tx, importID int64) error,
) error {
	return store.Tx(ctx, db, span, func(ctx context.Context, tx *sql.Tx) error {
		latest, _, err := Last(ctx, tx, e.Kind)
		if err != nil {
			return err
		}
		if latest.ID != m.Base {
			return ErrStale
		}
		return replace(ctx, tx, e, fn)
	})
}

// Push is Replace for a file pushed without preview (spec §7.6). Nobody saw
// the counts, so it refuses a file under half of the data in place with
// ErrTooFew; a file with the bytes of the latest import of its kind changes
// nothing and returns ErrUnchanged.
func Push(ctx context.Context, db *sql.DB, span string, e Entry,
	fn func(ctx context.Context, tx *sql.Tx, importID int64) error,
) error {
	return store.Tx(ctx, db, span, func(ctx context.Context, tx *sql.Tx) error {
		latest, ok, err := Last(ctx, tx, e.Kind)
		if err != nil {
			return err
		}
		if ok && bytes.Equal(latest.FileHash, e.FileHash) {
			return ErrUnchanged
		}
		var current int
		if err := tx.QueryRowContext(ctx, inPlace[e.Kind]).Scan(&current); err != nil {
			return fmt.Errorf("count %s in place: %w", e.Kind, err)
		}
		if e.Rows*2 < current {
			return ErrTooFew
		}
		return replace(ctx, tx, e, fn)
	})
}

// replace journals e, lets fn replace the data and marks the lines of
// homonyms, inside tx.
func replace(ctx context.Context, tx *sql.Tx, e Entry, fn func(ctx context.Context, tx *sql.Tx, importID int64) error) error {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO imports (kind, exported_at, period_from, period_to, imported_at, imported_by, row_count, skipped_count, file_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(e.Kind), store.UnixOrNull(e.ExportedAt), store.UnixOrNull(e.PeriodFrom), store.UnixOrNull(e.PeriodTo),
		e.ImportedAt.Unix(), e.ImportedBy, e.Rows, e.Skipped, e.FileHash)
	if err != nil {
		return fmt.Errorf("journal import: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("journal import: %w", err)
	}
	if err := fn(ctx, tx, id); err != nil {
		return err
	}
	return MarkAmbiguous(ctx, tx)
}

// MarkAmbiguous marks the payment and Mollie lines whose name belongs to
// several members. The mark is never cleared: only a new import of the lines
// starts afresh (spec §7.3, §7.5).
func MarkAmbiguous(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`UPDATE payment_lines SET ambiguous = 1
		 WHERE ambiguous = 0 AND name_hash IN (SELECT name_hash FROM members GROUP BY name_hash HAVING COUNT(*) > 1)`,
		`UPDATE online_payment_lines SET ambiguous = 1
		 WHERE ambiguous = 0 AND name_hash IN (SELECT name_hash FROM members GROUP BY name_hash HAVING COUNT(*) > 1)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("mark ambiguous lines: %w", err)
		}
	}
	return nil
}

// Meta is what every preview carries; previews embed it.
type Meta struct {
	ID                 string // set by Put
	Username           string // the uploader, the only one who may confirm
	NeedsSecondConfirm bool
	Base               int64 // id of the latest import of the kind when the preview was made, 0 when none

	created time.Time
}

// PreviewMeta gives Previews access to the embedded Meta.
func (m *Meta) PreviewMeta() *Meta { return m }

// Previews keeps unconfirmed previews in memory for 15 minutes.
type Previews[T interface{ PreviewMeta() *Meta }] struct {
	now func() time.Time
	ttl time.Duration // previewTTL outside tests

	mu    sync.Mutex
	items map[string]T
}

// NewPreviews returns an empty Previews; now is injectable for tests.
func NewPreviews[T interface{ PreviewMeta() *Meta }](now func() time.Time) *Previews[T] {
	return &Previews[T]{now: now, ttl: previewTTL, items: map[string]T{}}
}

// Put keeps v under a new random id, which it writes into v's Meta.
func (ps *Previews[T]) Put(v T) error {
	id, err := secure.NewToken()
	if err != nil {
		return err
	}
	m := v.PreviewMeta()
	m.ID, m.created = id, ps.now()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.dropExpired()
	ps.items[id] = v
	// Free the export once the preview expires, even if nobody touches the
	// store again; dropExpired still enforces the injected clock. The timer
	// holds the id only, never the preview: ids are random and never reused.
	time.AfterFunc(ps.ttl, func() {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		delete(ps.items, id)
	})
	return nil
}

// Get returns the live preview id of username.
func (ps *Previews[T]) Get(id, username string) (T, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.lookup(id, username)
}

// Take removes and returns the live preview id of username. Without
// secondConfirm, a preview that needs it is refused and kept for a retry.
func (ps *Previews[T]) Take(id, username string, secondConfirm bool) (T, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	v, err := ps.lookup(id, username)
	if err != nil {
		return v, err
	}
	if v.PreviewMeta().NeedsSecondConfirm && !secondConfirm {
		var zero T
		return zero, ErrSecondConfirmRequired
	}
	delete(ps.items, id)
	return v, nil
}

// lookup returns the live preview id of username. Callers hold ps.mu.
func (ps *Previews[T]) lookup(id, username string) (T, error) {
	ps.dropExpired()
	v, ok := ps.items[id]
	if !ok || v.PreviewMeta().Username != username {
		var zero T
		return zero, ErrPreviewNotFound
	}
	return v, nil
}

// dropExpired removes previews older than the TTL. Callers hold ps.mu.
func (ps *Previews[T]) dropExpired() {
	now := ps.now()
	for id, v := range ps.items {
		if now.Sub(v.PreviewMeta().created) > ps.ttl {
			delete(ps.items, id)
		}
	}
}
