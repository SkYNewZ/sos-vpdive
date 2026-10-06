package members

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Preview compares an export with the list in place. It lives in memory only
// and can be confirmed by its uploader alone.
type Preview struct {
	imports.Meta

	ExportedAt        time.Time
	Total             int
	Added             int
	Removed           int
	Current           int
	Skipped           int
	AmbiguousGroups   int
	AmbiguousAccounts int

	members []Member
}

// Store imports members lists and answers whitelist lookups.
type Store struct {
	db       *sql.DB
	keys     *secure.Keys
	now      func() time.Time
	previews *imports.Previews[*Preview]
}

// NewStore returns a Store; now is injectable for tests.
func NewStore(db *sql.DB, keys *secure.Keys, now func() time.Time) *Store {
	return &Store{db: db, keys: keys, now: now, previews: imports.NewPreviews[*Preview](now)}
}

// NewPreview compares exp with the list in place and keeps the result for
// 15 minutes, bound to username and to the import in place.
func (s *Store) NewPreview(ctx context.Context, username string, exp *Export) (*Preview, error) {
	base, current, err := s.current(ctx)
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
		Username:           username,
		NeedsSecondConfirm: len(exp.Members)*2 < len(current),
		Base:               base,
		ExportedAt:         exp.ExportedAt,
		Total:              len(exp.Members),
		Added:              added,
		Removed:            removed,
		Current:            len(current),
		Skipped:            exp.Skipped,
		AmbiguousGroups:    groups,
		AmbiguousAccounts:  accounts,
		members:            exp.Members,
	}
	if _, err := s.previews.Put(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Preview returns a live preview of username.
func (s *Store) Preview(id, username string) (*Preview, error) {
	return s.previews.Get(id, username)
}

// Confirm replaces the whole list with the previewed export and journals the
// import, in one transaction, then marks the payment lines of homonyms. The
// preview is consumed first, so a second confirmation of the same preview
// gets imports.ErrPreviewNotFound.
func (s *Store) Confirm(ctx context.Context, id, username string, secondConfirm bool) error {
	p, err := s.previews.Take(id, username, secondConfirm)
	if err != nil {
		return err
	}
	e := imports.Entry{Kind: imports.Members, ExportedAt: p.ExportedAt, ImportedAt: s.now(),
		ImportedBy: username, Rows: p.Total, Skipped: p.Skipped}
	return imports.Replace(ctx, s.db, "members.replace", &p.Meta, e, func(ctx context.Context, tx *sql.Tx, _ int64) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM members`); err != nil {
			return fmt.Errorf("clear members: %w", err)
		}
		return s.insertMembers(ctx, tx, p.members)
	})
}

// LastImport returns the latest members import, if any.
func (s *Store) LastImport(ctx context.Context) (imports.Info, bool, error) {
	return imports.Last(ctx, s.db, imports.Members)
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
	NameHash       []byte  // finds the payment lines (spec §7.3)
}

// Find returns the member of email. An address with a space returns
// secure.ErrEmailSpace, as Lookup does.
func (s *Store) Find(ctx context.Context, email string) (Profile, bool, error) {
	normalized, err := secure.NormalizeEmail(email)
	if err != nil {
		return Profile{}, false, err
	}
	var (
		p                             Profile
		first, last, seasons, licence []byte
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT name_hash, first_name, last_name, seasons, licence_expires FROM members WHERE email_hash = ?`,
		s.keys.Hash(normalized)).Scan(&p.NameHash, &first, &last, &seasons, &licence)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, fmt.Errorf("find member: %w", err)
	}
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
			string(imports.Members)).Scan(&last); err != nil {
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

// current returns the latest members import id (0 when none) and the email
// hashes of the list in place.
func (s *Store) current(ctx context.Context) (base int64, current map[string]bool, err error) {
	if base, err = imports.LatestID(ctx, s.db, imports.Members); err != nil {
		return 0, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT email_hash FROM members`)
	hashes, err := store.Collect(rows, err, func(rows *sql.Rows) (h []byte, err error) {
		err = rows.Scan(&h)
		return h, err
	})
	if err != nil {
		return 0, nil, fmt.Errorf("current members: %w", err)
	}
	current = make(map[string]bool, len(hashes))
	for _, h := range hashes {
		current[string(h)] = true
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
