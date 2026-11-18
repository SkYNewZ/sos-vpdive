package tickets

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// MonthStat counts the requests of one Paris month and category.
type MonthStat struct {
	Month     string // 2026-10
	Category  string
	Submitted int // requests still in place that were submitted this month
	Closed    int // requests closed this month, deleted and purged ones included
	// HoursToClose adds up, for Closed, the hours from submission to closing.
	HoursToClose int64
	Avoided      int // requests a fiche answered before they were sent
}

// Stats counts the requests in place by status, and by month and category.
type Stats struct {
	ByStatus map[Status]int
	Months   []MonthStat // by month, then category
}

// Stats counts what the committee has to report on. A closed request that
// was deleted or purged lives on in stats_monthly only (spec §8.3): it still
// counts as closed in its month, no longer as submitted.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	months := map[[2]string]*MonthStat{}
	at := func(t time.Time, category string) *MonthStat {
		key := [2]string{t.In(s.paris).Format("2006-01"), category}
		if months[key] == nil {
			months[key] = &MonthStat{Month: key[0], Category: category}
		}
		return months[key]
	}
	out := Stats{ByStatus: map[Status]int{}}

	rows, err := s.DB.QueryContext(ctx, `SELECT category, status, submitted_at, closed_at FROM submitted_tickets`)
	if err == nil {
		err = scanEach(rows, func() error {
			var (
				category          string
				status            Status
				submitted, closed sql.NullInt64
			)
			if err := rows.Scan(&category, &status, &submitted, &closed); err != nil {
				return err
			}
			out.ByStatus[status]++
			at(store.UnixTime(submitted), category).Submitted++
			if status == StatusDone && closed.Valid {
				m := at(store.UnixTime(closed), category)
				m.Closed++
				m.HoursToClose += (closed.Int64 - submitted.Int64) / 3600
			}
			return nil
		})
	}
	if err != nil {
		return Stats{}, fmt.Errorf("count requests: %w", err)
	}

	rows, err = s.DB.QueryContext(ctx, `SELECT month, category, closed_count, hours_to_close_total FROM stats_monthly`)
	if err == nil {
		err = scanEach(rows, func() error {
			var (
				month, category string
				count           int
				hours           int64
			)
			if err := rows.Scan(&month, &category, &count, &hours); err != nil {
				return err
			}
			key := [2]string{month, category}
			if months[key] == nil {
				months[key] = &MonthStat{Month: month, Category: category}
			}
			months[key].Closed += count
			months[key].HoursToClose += hours
			return nil
		})
	}
	if err != nil {
		return Stats{}, fmt.Errorf("read monthly stats: %w", err)
	}

	rows, err = s.DB.QueryContext(ctx, `SELECT category, created_at FROM deflections`)
	if err == nil {
		err = scanEach(rows, func() error {
			var (
				category string
				created  int64
			)
			if err := rows.Scan(&category, &created); err != nil {
				return err
			}
			at(time.Unix(created, 0), category).Avoided++
			return nil
		})
	}
	if err != nil {
		return Stats{}, fmt.Errorf("count avoided requests: %w", err)
	}

	for _, m := range months {
		out.Months = append(out.Months, *m)
	}
	slices.SortFunc(out.Months, func(a, b MonthStat) int {
		return cmp.Or(cmp.Compare(a.Month, b.Month), cmp.Compare(a.Category, b.Category))
	})
	return out, nil
}

// scanEach calls read for each row of rows and closes them.
func scanEach(rows *sql.Rows, read func() error) (err error) {
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		if err := read(); err != nil {
			return err
		}
	}
	return rows.Err()
}
