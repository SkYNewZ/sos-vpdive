package members

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
		NeedsSecondConfirm: imports.UnderHalf(len(exp.Members), len(current)),
		Base:               base,
		FileHash:           exp.FileHash,
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
	if err := s.previews.Put(p); err != nil {
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
		ImportedBy: username, Rows: p.Total, Skipped: p.Skipped, FileHash: p.FileHash}
	return imports.Replace(ctx, s.db, "members.replace", &p.Meta, e, s.replaceWith(p.members))
}

// Import replaces the whole list with a pushed export, without preview
// (spec §7.6): imports.Push refuses an unchanged file and a file with less
// than half of the accounts in place.
func (s *Store) Import(ctx context.Context, exp *Export) error {
	e := imports.Entry{Kind: imports.Members, ExportedAt: exp.ExportedAt, ImportedAt: s.now(),
		ImportedBy: imports.ScriptAuthor, Rows: len(exp.Members), Skipped: exp.Skipped, FileHash: exp.FileHash}
	return imports.Push(ctx, s.db, "members.replace", e, s.replaceWith(exp.Members))
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
	if p.FirstName, p.LastName, err = s.openNames(first, last); err != nil {
		return Profile{}, false, err
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

// Named returns the first and last name of the first member of nameHash,
// and how many members bear that name: "" and 0 when none does.
func (s *Store) Named(ctx context.Context, nameHash []byte) (name string, members int, err error) {
	var first, last []byte
	err = s.db.QueryRowContext(ctx,
		`SELECT first_name, last_name, (SELECT COUNT(*) FROM members WHERE name_hash = ?1)
		 FROM members WHERE name_hash = ?1 ORDER BY id LIMIT 1`, nameHash).Scan(&first, &last, &members)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("member of a name: %w", err)
	}
	firstName, lastName, err := s.openNames(first, last)
	if err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(firstName + " " + lastName), members, nil
}

// NameCount returns how many members bear nameHash, decrypting nothing: a
// count above one is a homonym (spec §7.3).
func (s *Store) NameCount(ctx context.Context, nameHash []byte) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM members WHERE name_hash = ?`, nameHash).Scan(&n); err != nil {
		return 0, fmt.Errorf("count members of a name: %w", err)
	}
	return n, nil
}

// EraseTx deletes the member of email inside tx and returns its name hash
// and names, ok false when there was none (erasure, spec §4.5): the caller
// erases the payment lines and calendar participations of that name. The
// next import lists it again if VPDive does.
func (s *Store) EraseTx(ctx context.Context, tx *sql.Tx, email string) (Profile, bool, error) {
	normalized, err := secure.NormalizeEmail(email)
	if err != nil {
		return Profile{}, false, err
	}
	var (
		p           Profile
		first, last []byte
	)
	err = tx.QueryRowContext(ctx, `DELETE FROM members WHERE email_hash = ? RETURNING name_hash, first_name, last_name`,
		s.keys.Hash(normalized)).Scan(&p.NameHash, &first, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, fmt.Errorf("erase member: %w", err)
	}
	if p.FirstName, p.LastName, err = s.openNames(first, last); err != nil {
		return Profile{}, false, err
	}
	return p, true, nil
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
	return store.Tx(ctx, s.db, "members.purge", func(ctx context.Context, tx *sql.Tx) error {
		// Never imported: MAX is NULL, the comparison is false, nothing goes.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM members WHERE (SELECT MAX(imported_at) FROM imports WHERE kind = ?) < ?`,
			string(imports.Members), s.now().AddDate(-1, 0, 0).Unix()); err != nil {
			return fmt.Errorf("purge members: %w", err)
		}
		return nil
	})
}

// openNames decrypts the stored first and last name of a member.
func (s *Store) openNames(first, last []byte) (firstName, lastName string, err error) {
	if firstName, err = s.keys.OpenString(first); err != nil {
		return "", "", fmt.Errorf("decrypt member: %w", err)
	}
	if lastName, err = s.keys.OpenString(last); err != nil {
		return "", "", fmt.Errorf("decrypt member: %w", err)
	}
	return firstName, lastName, nil
}

// replaceWith replaces the list in place with ms.
func (s *Store) replaceWith(ms []Member) func(context.Context, *sql.Tx, int64) error {
	return func(ctx context.Context, tx *sql.Tx, _ int64) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM members`); err != nil {
			return fmt.Errorf("clear members: %w", err)
		}
		return s.insertMembers(ctx, tx, ms)
	}
}

// current returns the latest members import id (0 when none) and the email
// hashes of the list in place.
func (s *Store) current(ctx context.Context) (base int64, current map[string]bool, err error) {
	last, _, err := imports.Last(ctx, s.db, imports.Members)
	if err != nil {
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
	base, current = last.ID, make(map[string]bool, len(hashes))
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
