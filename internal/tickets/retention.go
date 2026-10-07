package tickets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Retention of spec §8.3 and §9.8.
const (
	draftTTL    = 24 * time.Hour
	orphanGrace = 24 * time.Hour
	idleAfter   = 365 * 24 * time.Hour
)

// Purge deletes drafts older than 24 h and done requests past RetentionDays
// (monthly stats first). Open requests are never deleted nor closed.
func (s *Store) Purge(ctx context.Context) error {
	now := s.Now()
	retention := time.Duration(s.RetentionDays) * 24 * time.Hour
	var (
		list []ticketRow
		keys []string
	)
	err := s.tx(ctx, "purge", func(ctx context.Context, tx *sql.Tx) error {
		var err error
		list, err = listDoomed(ctx, tx,
			`SELECT id, status, category, submitted_at, closed_at FROM tickets
			 WHERE (status = 'draft' AND created_at < ?) OR (status = 'done' AND closed_at < ?)`,
			now.Add(-draftTTL).Unix(), now.Add(-retention).Unix())
		if err != nil {
			return err
		}
		keys, err = s.drop(ctx, tx, list)
		return err
	})
	if err != nil {
		return err
	}
	s.deleteObjects(ctx, keys)
	for _, d := range list {
		if d.status == StatusDone {
			s.changed(ChangeDeleted, d.id, "")
		}
	}
	return nil
}

// SweepOrphans deletes stored objects without an attachment row for 24 h:
// uploads whose row was never written, and deletions that failed (spec §9.8).
func (s *Store) SweepOrphans(ctx context.Context) error {
	objects, err := s.Blobs.List(ctx)
	if err != nil {
		return fmt.Errorf("list capture objects: %w", err)
	}
	keys, err := objectKeys(ctx, s.DB, `SELECT object_key FROM attachments`)
	if err != nil {
		return err
	}
	used := make(map[string]bool, len(keys))
	for _, k := range keys {
		used[k] = true
	}
	cutoff := s.Now().Add(-orphanGrace)
	var errs []error
	for _, o := range objects {
		if o.Modified.After(cutoff) || used[o.Key] {
			continue
		}
		if err := s.Blobs.Delete(ctx, o.Key); err != nil {
			errs = append(errs, fmt.Errorf("delete orphan capture: %w", err))
		}
	}
	return errors.Join(errs...)
}

// IdleRefs lists the references of open requests without activity for 12
// months: the banner shows them for review, nothing closes them (spec §8.3).
func (s *Store) IdleRefs(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT ref FROM tickets
		WHERE status IN ('todo', 'in_progress', 'waiting') AND updated_at < ?
		ORDER BY updated_at, id`, s.Now().Add(-idleAfter).Unix())
	refs, err := store.Collect(rows, err, func(rows *sql.Rows) (ref string, err error) {
		err = rows.Scan(&ref)
		return ref, err
	})
	if err != nil {
		return nil, fmt.Errorf("idle tickets: %w", err)
	}
	return refs, nil
}
