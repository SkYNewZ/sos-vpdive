package tickets

import (
	"cmp"
	"context"
	"database/sql"
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

// MonthLayout is how a month is written in stats_monthly and in Stats.
const MonthLayout = "2006-01"

// Stats counts what the committee has to report on. A closed request that
// was deleted or purged lives on in stats_monthly only (spec §8.3): it still
// counts as closed in its month, no longer as submitted.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	months := map[[2]string]*MonthStat{}
	at := func(month, category string) *MonthStat {
		key := [2]string{month, category}
		if months[key] == nil {
			months[key] = &MonthStat{Month: month, Category: category}
		}
		return months[key]
	}
	monthOf := func(unix int64) string { return time.Unix(unix, 0).In(s.paris).Format(MonthLayout) }
	out := Stats{ByStatus: map[Status]int{}}

	type live struct {
		category          string
		status            Status
		submitted, closed sql.NullInt64
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT category, status, submitted_at, closed_at FROM submitted_tickets`)
	requests, err := store.Collect(rows, err, func(rows *sql.Rows) (r live, err error) {
		return r, rows.Scan(&r.category, &r.status, &r.submitted, &r.closed)
	})
	if err != nil {
		return Stats{}, fmt.Errorf("count requests: %w", err)
	}
	for _, r := range requests {
		out.ByStatus[r.status]++
		at(monthOf(r.submitted.Int64), r.category).Submitted++
		if r.status == StatusDone && r.closed.Valid {
			m := at(monthOf(r.closed.Int64), r.category)
			m.Closed++
			m.HoursToClose += (r.closed.Int64 - r.submitted.Int64) / 3600 // as addStats counts them
		}
	}

	rows, err = s.DB.QueryContext(ctx, `SELECT month, category, closed_count, hours_to_close_total, 0 FROM stats_monthly
		UNION ALL SELECT '', category, 0, 0, created_at FROM deflections`)
	archived, err := store.Collect(rows, err, func(rows *sql.Rows) (m MonthStat, err error) {
		var avoidedAt int64
		if err = rows.Scan(&m.Month, &m.Category, &m.Closed, &m.HoursToClose, &avoidedAt); err == nil && m.Month == "" {
			m.Month, m.Avoided = monthOf(avoidedAt), 1
		}
		return m, err
	})
	if err != nil {
		return Stats{}, fmt.Errorf("read monthly stats: %w", err)
	}
	for _, a := range archived {
		m := at(a.Month, a.Category)
		m.Closed += a.Closed
		m.HoursToClose += a.HoursToClose
		m.Avoided += a.Avoided
	}

	for _, m := range months {
		out.Months = append(out.Months, *m)
	}
	slices.SortFunc(out.Months, func(a, b MonthStat) int {
		return cmp.Or(cmp.Compare(a.Month, b.Month), cmp.Compare(a.Category, b.Category))
	})
	return out, nil
}
