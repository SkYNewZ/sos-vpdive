package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// retention is how long lines outlive their import when no new one comes
// (spec §8.3).
const retention = 90 * 24 * time.Hour

// lineTable is a table of imported lines. payment_lines and
// online_payment_lines share their columns and their rules: sealed JSON
// keyed by the name hash, attribution at display time, the « ambiguë » mark,
// purge after 90 days, erasure by name (spec §7.3, §7.5, §8.3, §4.5). Only
// the table and the import kind differ.
type lineTable struct {
	db   *sql.DB
	keys *secure.Keys
	now  func() time.Time
	kind imports.Kind
	name string // names the lines in spans and errors
	q    lineQueries
}

// lineQueries are the statements of one table, built once from a constant
// table name.
type lineQueries struct {
	exists, all, ofName, count, report, erase, purge, clear, insert string
}

func newLineTable(db *sql.DB, keys *secure.Keys, now func() time.Time, kind imports.Kind, name, table string) lineTable {
	return lineTable{db: db, keys: keys, now: now, kind: kind, name: name, q: lineQueries{
		exists: `SELECT EXISTS (SELECT 1 FROM ` + table + `)`,
		all:    `SELECT name_hash, ambiguous, data FROM ` + table + ` ORDER BY id`,
		ofName: `SELECT name_hash, ambiguous, data FROM ` + table + ` WHERE name_hash = ? ORDER BY id`,
		count:  `SELECT COUNT(*) FROM ` + table + ` WHERE name_hash = ?`,
		report: `SELECT MAX(l.ambiguous), COUNT(*), (SELECT COUNT(*) FROM members m WHERE m.name_hash = l.name_hash)
			 FROM ` + table + ` l GROUP BY l.name_hash`,
		erase:  `DELETE FROM ` + table + ` WHERE name_hash = ?`,
		purge:  `DELETE FROM ` + table + ` WHERE (SELECT MAX(imported_at) FROM imports WHERE kind = ?) < ?`,
		clear:  `DELETE FROM ` + table,
		insert: `INSERT INTO ` + table + ` (import_id, name_hash, data) VALUES (?, ?, ?)`,
	}}
}

// LastImport returns the latest import of the lines, if any.
func (t lineTable) LastImport(ctx context.Context) (imports.Info, bool, error) {
	return imports.Last(ctx, t.db, t.kind)
}

// Report counts the lines in place by attribution (spec §7.3): to exactly
// one member, to a name several members share or that is marked, or to no
// member.
func (t lineTable) Report(ctx context.Context) (Report, error) {
	rows, err := t.db.QueryContext(ctx, t.q.report)
	payers, err := store.Collect(rows, err, func(rows *sql.Rows) (c struct{ marked, lines, members int }, err error) {
		err = rows.Scan(&c.marked, &c.lines, &c.members)
		return c, err
	})
	if err != nil {
		return Report{}, fmt.Errorf("%s report: %w", t.name, err)
	}
	var r Report
	for _, p := range payers {
		dst := &r.Unmatched
		switch {
		case p.marked == 1 || p.members > 1:
			dst = &r.Ambiguous
		case p.members == 1:
			dst = &r.Attached
		}
		dst.Lines += p.lines
		dst.Payers++
	}
	return r, nil
}

// Purge deletes every line when no import of the kind happened for 90 days
// (spec §8.3). The imports journal holds no personal data and stays.
func (t lineTable) Purge(ctx context.Context) error {
	return store.Tx(ctx, t.db, t.name+".purge", func(ctx context.Context, tx *sql.Tx) error {
		// Never imported: MAX is NULL, the comparison is false, nothing goes.
		if _, err := tx.ExecContext(ctx, t.q.purge, string(t.kind), t.now().Add(-retention).Unix()); err != nil {
			return fmt.Errorf("purge %s lines: %w", t.name, err)
		}
		return nil
	})
}

// Expired reports an import older than the 90-day retention: once the daily
// purge has run, its lines are gone (spec §8.3). A recent import may hold no
// line at all, when the VPDive filters matched nothing.
func (t lineTable) Expired(info imports.Info) bool {
	return t.now().Sub(info.ImportedAt) > retention
}

// Count returns the number of lines of nameHash, ambiguous ones included.
func (t lineTable) Count(ctx context.Context, nameHash []byte) (int, error) {
	var n int
	if err := t.db.QueryRowContext(ctx, t.q.count, nameHash).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s lines of a name: %w", t.name, err)
	}
	return n, nil
}

// EraseTx deletes every line of nameHash inside tx, a homonym's included
// (erasure, spec §4.5; owner decision). The next import brings them back
// while VPDive has them.
func (t lineTable) EraseTx(ctx context.Context, tx *sql.Tx, nameHash []byte) (int, error) {
	res, err := tx.ExecContext(ctx, t.q.erase, nameHash)
	if err != nil {
		return 0, fmt.Errorf("erase %s lines: %w", t.name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("erase %s lines: %w", t.name, err)
	}
	return int(n), nil
}

// storedLine is a line of either export, as it is inserted.
type storedLine interface {
	Line | MollieLine
	origin() (row int, nameKey string)
}

func (l Line) origin() (int, string)       { return l.Row, l.NameKey }
func (l MollieLine) origin() (int, string) { return l.Row, l.NameKey }

// replaceLines replaces the lines of t with lines, inside the import
// transaction: the name is hashed, the rest sealed as JSON.
func replaceLines[T storedLine](t lineTable, lines []T) func(context.Context, *sql.Tx, int64) error {
	return func(ctx context.Context, tx *sql.Tx, importID int64) (err error) {
		if _, err := tx.ExecContext(ctx, t.q.clear); err != nil {
			return fmt.Errorf("clear %s lines: %w", t.name, err)
		}
		stmt, err := tx.PrepareContext(ctx, t.q.insert)
		if err != nil {
			return fmt.Errorf("prepare %s line insert: %w", t.name, err)
		}
		defer func() { err = errors.Join(err, stmt.Close()) }()
		for _, l := range lines {
			row, nameKey := l.origin()
			data, err := json.Marshal(l)
			if err != nil {
				return fmt.Errorf("encode %s line of row %d: %w", t.name, row, err)
			}
			if _, err := stmt.ExecContext(ctx, importID, t.keys.Hash(nameKey), t.keys.Seal(data)); err != nil {
				return fmt.Errorf("insert %s line of row %d: %w", t.name, row, err)
			}
		}
		return nil
	}
}

// sealedLine is a stored line before decryption.
type sealedLine struct {
	nameHash  []byte
	ambiguous bool
	data      []byte
}

// sealed reads stored lines in id order; query selects name_hash, ambiguous
// and data.
func (t lineTable) sealed(ctx context.Context, query string, args ...any) ([]sealedLine, error) {
	rows, err := t.db.QueryContext(ctx, query, args...)
	found, err := store.Collect(rows, err, func(rows *sql.Rows) (r sealedLine, err error) {
		err = rows.Scan(&r.nameHash, &r.ambiguous, &r.data)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("read %s lines: %w", t.name, err)
	}
	return found, nil
}

// openLine decrypts and decodes one stored line.
func openLine[T Line | MollieLine](keys *secure.Keys, sealed []byte) (T, error) {
	var l T
	plain, err := keys.Open(sealed)
	if err != nil {
		return l, fmt.Errorf("decrypt line: %w", err)
	}
	if err := json.Unmarshal(plain, &l); err != nil {
		return l, fmt.Errorf("decode line: %w", err)
	}
	return l, nil
}

// nameLines decides what a block shows for nameHash, nil when the requester
// is not in the members list, in the order of the BlockState values. With
// BlockLines it returns the sealed lines of the name, in file order.
func (t lineTable) nameLines(ctx context.Context, nameHash []byte) (BlockState, imports.Info, []sealedLine, error) {
	var inPlace bool
	if err := t.db.QueryRowContext(ctx, t.q.exists).Scan(&inPlace); err != nil {
		return "", imports.Info{}, nil, fmt.Errorf("presence of %s lines: %w", t.name, err)
	}
	info, imported, err := t.LastImport(ctx)
	switch {
	case err != nil:
		return "", imports.Info{}, nil, err
	case !imported:
		return BlockNoLines, info, nil, nil
	case !inPlace && t.Expired(info):
		return BlockPurged, info, nil, nil
	case nameHash == nil:
		return BlockNoMember, info, nil, nil
	}
	var members int
	if err := t.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM members WHERE name_hash = ?`, nameHash).Scan(&members); err != nil {
		return "", imports.Info{}, nil, fmt.Errorf("members of a name: %w", err)
	}
	found, err := t.sealed(ctx, t.q.ofName, nameHash)
	switch {
	case err != nil:
		return "", imports.Info{}, nil, err
	case members > 1 || slices.ContainsFunc(found, func(r sealedLine) bool { return r.ambiguous }):
		return BlockAmbiguous, info, nil, nil
	case len(found) == 0:
		return BlockEmpty, info, nil, nil
	}
	return BlockLines, info, found, nil
}
