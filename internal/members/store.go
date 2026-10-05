package members

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Import errors.
var (
	ErrPreviewNotFound       = errors.New("import preview not found, already used or expired")
	ErrStale                 = errors.New("another import happened since the preview")
	ErrSecondConfirmRequired = errors.New("second confirmation required")
)

// ImportKind is the kind column of the imports journal.
type ImportKind string

// ImportMembers marks a members list import. Lot 5 adds payments.
const ImportMembers ImportKind = "members"

// previewTTL is how long an unconfirmed preview lives in memory (spec §7.2).
const previewTTL = 15 * time.Minute

// ImportInfo is one entry of the imports journal.
type ImportInfo struct {
	ID         int64
	ExportedAt time.Time // zero when the export date was unreadable
	ImportedAt time.Time
	ImportedBy string
	Rows       int
	Skipped    int
}

// Preview compares an export with the list in place. It lives in memory only
// and can be confirmed by its uploader alone.
type Preview struct {
	ID                 string
	Username           string
	ExportedAt         time.Time
	Total              int
	Added              int
	Removed            int
	Current            int
	Skipped            int
	AmbiguousGroups    int
	AmbiguousAccounts  int
	NeedsSecondConfirm bool

	baseImportID int64
	createdAt    time.Time
	members      []Member
}

// Store imports members lists and answers whitelist lookups.
type Store struct {
	db   *sql.DB
	keys *secure.Keys
	now  func() time.Time
	ttl  time.Duration // preview lifetime; previewTTL outside tests

	mu       sync.Mutex
	previews map[string]*Preview
}

// NewStore returns a Store; now is injectable for tests.
func NewStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *Store {
	return &Store{db: db, keys: keys, now: now, ttl: previewTTL, previews: map[string]*Preview{}}
}

// NewPreview compares exp with the list in place and keeps the result for
// 15 minutes, bound to username and to the import in place.
func (s *Store) NewPreview(ctx context.Context, username string, exp *Export) (*Preview, error) {
	base, current, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	id, err := secure.NewToken()
	if err != nil {
		return nil, err
	}
	incoming := make(map[string]bool, len(exp.Members))
	added := 0
	for _, m := range exp.Members {
		h := string(s.keys.Hash(m.Email))
		incoming[h] = true
		if !current[h] {
			added++
		}
	}
	removed := 0
	for h := range current {
		if !incoming[h] {
			removed++
		}
	}
	groups, accounts := AmbiguousGroups(exp.Members)
	p := &Preview{
		ID:                 id,
		Username:           username,
		ExportedAt:         exp.ExportedAt,
		Total:              len(exp.Members),
		Added:              added,
		Removed:            removed,
		Current:            len(current),
		Skipped:            exp.Skipped,
		AmbiguousGroups:    groups,
		AmbiguousAccounts:  accounts,
		NeedsSecondConfirm: len(exp.Members)*2 < len(current),
		baseImportID:       base,
		createdAt:          s.now(),
		members:            exp.Members,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpired()
	s.previews[id] = p
	// Free the names and emails once the preview expires, even if nobody
	// touches the store again; dropExpired still enforces the injected clock.
	time.AfterFunc(s.ttl, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.previews[id] == p {
			delete(s.previews, id)
		}
	})
	return p, nil
}

// Preview returns a live preview of username.
func (s *Store) Preview(id, username string) (*Preview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookup(id, username)
}

// Confirm replaces the whole list with the previewed export and journals the
// import, in one transaction. The preview is consumed first, so a second
// confirmation of the same preview gets ErrPreviewNotFound.
func (s *Store) Confirm(ctx context.Context, id, username string, secondConfirm bool) error {
	p, err := s.take(id, username, secondConfirm)
	if err != nil {
		return err
	}
	return store.Tx(ctx, s.db, "members.replace", func(ctx context.Context, tx *sql.Tx) error {
		latest, err := latestImportID(ctx, tx)
		if err != nil {
			return err
		}
		if latest != p.baseImportID {
			return ErrStale
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM members`); err != nil {
			return fmt.Errorf("clear members: %w", err)
		}
		if err := s.insertMembers(ctx, tx, p.members); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO imports (kind, exported_at, imported_at, imported_by, row_count, skipped_count)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			string(ImportMembers), unixOrNull(p.ExportedAt), s.now().Unix(), username, p.Total, p.Skipped); err != nil {
			return fmt.Errorf("journal import: %w", err)
		}
		return nil
	})
}

// LastImport returns the latest members import, if any.
func (s *Store) LastImport(ctx context.Context) (ImportInfo, bool, error) {
	var (
		info     ImportInfo
		exported sql.NullInt64
		imported int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, exported_at, imported_at, imported_by, row_count, skipped_count
		 FROM imports WHERE kind = ? ORDER BY id DESC LIMIT 1`, string(ImportMembers)).
		Scan(&info.ID, &exported, &imported, &info.ImportedBy, &info.Rows, &info.Skipped)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportInfo{}, false, nil
	}
	if err != nil {
		return ImportInfo{}, false, fmt.Errorf("last import: %w", err)
	}
	if exported.Valid {
		info.ExportedAt = time.Unix(exported.Int64, 0).UTC()
	}
	info.ImportedAt = time.Unix(imported, 0).UTC()
	return info, true, nil
}

// Lookup reports whether email belongs to the list in place (spec §3.6).
// An address containing a space returns secure.ErrEmailSpace.
func (s *Store) Lookup(ctx context.Context, email string) (bool, error) {
	normalized, err := secure.NormalizeEmail(email)
	if err != nil {
		return false, err
	}
	var found bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM members WHERE email_hash = ?)`,
		s.keys.Hash(normalized)).Scan(&found); err != nil {
		return false, fmt.Errorf("lookup member: %w", err)
	}
	return found, nil
}

// Profile is what a ticket page shows of the requester (spec §4.3).
type Profile struct {
	FirstName      string
	LastName       string
	Seasons        *string // nil when the export had no "Année(s)" column
	LicenceExpires string  // YYYY-MM-DD or ""
}

// Find returns the member of email. An address with a space returns
// secure.ErrEmailSpace, as Lookup does.
func (s *Store) Find(ctx context.Context, email string) (Profile, bool, error) {
	normalized, err := secure.NormalizeEmail(email)
	if err != nil {
		return Profile{}, false, err
	}
	var first, last, seasons, licence []byte
	err = s.db.QueryRowContext(ctx,
		`SELECT first_name, last_name, seasons, licence_expires FROM members WHERE email_hash = ?`,
		s.keys.Hash(normalized)).Scan(&first, &last, &seasons, &licence)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, fmt.Errorf("find member: %w", err)
	}
	var p Profile
	if p.FirstName, err = s.keys.OpenString(first); err != nil {
		return Profile{}, false, fmt.Errorf("decrypt member: %w", err)
	}
	if p.LastName, err = s.keys.OpenString(last); err != nil {
		return Profile{}, false, fmt.Errorf("decrypt member: %w", err)
	}
	if seasons != nil {
		v, err := s.keys.OpenString(seasons)
		if err != nil {
			return Profile{}, false, fmt.Errorf("decrypt member: %w", err)
		}
		p.Seasons = &v
	}
	if licence != nil {
		if p.LicenceExpires, err = s.keys.OpenString(licence); err != nil {
			return Profile{}, false, fmt.Errorf("decrypt member: %w", err)
		}
	}
	return p, true, nil
}

// EraseTx deletes the member of email inside tx and reports whether one
// existed (erasure, spec §4.5). The next import lists it again if VPDive does.
func (s *Store) EraseTx(ctx context.Context, tx *sql.Tx, email string) (bool, error) {
	normalized, err := secure.NormalizeEmail(email)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM members WHERE email_hash = ?`, s.keys.Hash(normalized))
	if err != nil {
		return false, fmt.Errorf("erase member: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("erase member: %w", err)
	}
	return n > 0, nil
}

// HasList reports whether a list is in place. Without one the form is closed.
func (s *Store) HasList(ctx context.Context) (bool, error) {
	var found bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM members)`).Scan(&found); err != nil {
		return false, fmt.Errorf("members list presence: %w", err)
	}
	return found, nil
}

// Purge deletes the list when no members import happened for 12 months
// (spec §8.3). The imports journal holds no personal data and stays.
func (s *Store) Purge(ctx context.Context) error {
	cutoff := s.now().AddDate(-1, 0, 0).Unix()
	return store.Tx(ctx, s.db, "members.purge", func(ctx context.Context, tx *sql.Tx) error {
		var last sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(imported_at) FROM imports WHERE kind = ?`,
			string(ImportMembers)).Scan(&last); err != nil {
			return fmt.Errorf("last import date: %w", err)
		}
		if !last.Valid || last.Int64 >= cutoff {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM members`); err != nil {
			return fmt.Errorf("purge members: %w", err)
		}
		return nil
	})
}

func (s *Store) take(id, username string, secondConfirm bool) (*Preview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.lookup(id, username)
	if err != nil {
		return nil, err
	}
	if p.NeedsSecondConfirm && !secondConfirm {
		return nil, ErrSecondConfirmRequired
	}
	delete(s.previews, id)
	return p, nil
}

// lookup returns the live preview id of username. Callers hold s.mu.
func (s *Store) lookup(id, username string) (*Preview, error) {
	s.dropExpired()
	p, ok := s.previews[id]
	if !ok || p.Username != username {
		return nil, ErrPreviewNotFound
	}
	return p, nil
}

// dropExpired removes previews older than previewTTL. Callers hold s.mu.
func (s *Store) dropExpired() {
	now := s.now()
	for id, p := range s.previews {
		if now.Sub(p.createdAt) > previewTTL {
			delete(s.previews, id)
		}
	}
}

// current returns the latest members import id (0 when none) and the email
// hashes of the list in place.
func (s *Store) current(ctx context.Context) (base int64, current map[string]bool, err error) {
	if base, err = latestImportID(ctx, s.db); err != nil {
		return 0, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT email_hash FROM members`)
	if err != nil {
		return 0, nil, fmt.Errorf("current members: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	current = map[string]bool{}
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return 0, nil, fmt.Errorf("current members: %w", err)
		}
		current[string(h)] = true
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("current members: %w", err)
	}
	return base, current, nil
}

func (s *Store) insertMembers(ctx context.Context, tx *sql.Tx, ms []Member) (err error) {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO members (email_hash, name_hash, first_name, last_name, email, seasons, licence_expires)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare member insert: %w", err)
	}
	defer func() { err = errors.Join(err, stmt.Close()) }()
	for _, m := range ms {
		if _, err := stmt.ExecContext(ctx,
			s.keys.Hash(m.Email),
			s.keys.Hash(secure.NameKey(m.LastName, m.FirstName)),
			s.keys.SealString(m.FirstName),
			s.keys.SealString(m.LastName),
			s.keys.SealString(m.Email),
			s.sealOptional(m.Seasons),
			s.sealNonEmpty(m.LicenceExpires),
		); err != nil {
			return fmt.Errorf("insert member of row %d: %w", m.Row, err)
		}
	}
	return nil
}

// sealOptional stores NULL for an absent column and an encrypted value otherwise.
func (s *Store) sealOptional(v *string) any {
	if v == nil {
		return nil
	}
	return s.keys.SealString(*v)
}

func (s *Store) sealNonEmpty(v string) any {
	if v == "" {
		return nil
	}
	return s.keys.SealString(v)
}

// rowQuerier is satisfied by *sql.DB and *sql.Tx.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// latestImportID returns the id of the latest members import, 0 when none.
func latestImportID(ctx context.Context, q rowQuerier) (int64, error) {
	var id int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM imports WHERE kind = ?`,
		string(ImportMembers)).Scan(&id); err != nil {
		return 0, fmt.Errorf("latest import: %w", err)
	}
	return id, nil
}

func unixOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}
