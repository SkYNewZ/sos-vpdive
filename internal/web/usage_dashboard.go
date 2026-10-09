package web

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// The usage dashboard of /assistant/journal (2026-10-09): what the assistant
// and the form's suggestions cost, per Paris day, per month and per account.
const (
	dashboardDays   = 30
	dashboardMonths = usageRetention

	// Chart geometry, in viewBox units: a column per slot, 100 high.
	slotWidth = 10
	barWidth  = 6
	minHeight = 1.5 // a call worth little still shows
	gap       = 1   // between the two segments of a column
)

// usageCall is one model call: an assistant answer, or a suggestion when
// Account is empty.
type usageCall struct {
	Account string
	At      time.Time
	Cost    sql.NullInt64
}

// spend sums model calls. Cost is valid only while every call had a price:
// a partial sum would understate it.
type spend struct {
	Calls int
	Cost  sql.NullInt64
}

func (s spend) add(cost sql.NullInt64) spend {
	s.Cost = sql.NullInt64{Int64: s.Cost.Int64 + cost.Int64, Valid: cost.Valid && (s.Calls == 0 || s.Cost.Valid)}
	s.Calls++
	return s
}

// priced is true when s has a cost for every call, none included.
func (s spend) priced() bool { return s.Calls == 0 || s.Cost.Valid }

// value is what the charts measure: micro-dollars, or calls without prices.
func (s spend) value(cost bool) int64 {
	if cost {
		return s.Cost.Int64
	}
	return int64(s.Calls)
}

// split is the spend of both modes over a period.
type split struct{ Assistant, Suggest spend }

// Cost sums both modes, valid when every call had a price.
func (p split) Cost() sql.NullInt64 {
	return sql.NullInt64{Int64: p.Assistant.Cost.Int64 + p.Suggest.Cost.Int64,
		Valid: p.Assistant.Calls+p.Suggest.Calls > 0 && p.Assistant.priced() && p.Suggest.priced()}
}

func (p split) add(c usageCall) split {
	if c.Account == "" {
		p.Suggest = p.Suggest.add(c.Cost)
	} else {
		p.Assistant = p.Assistant.add(c.Cost)
	}
	return p
}

// column is a day or a month: its sums, its stacked bar, its axis label.
type column struct {
	split

	Label string // « mar. 27 oct. », « octobre 2026 »
	Tick  string // under the axis, on some columns
	X     int    // the left edge of its slot
	A, S  string // path data of the assistant and suggestion segments
}

type chart struct {
	Cost    bool   // dollars, when every call shown had a price; else calls
	Top     string // the value of the top gridline
	Width   int    // of the viewBox
	Columns []column
}

// accountSpend is an account's month, or the form's when Account is empty.
type accountSpend struct {
	spend

	Account string
	Share   int // percent of the largest
}

type dashboard struct {
	Today, Month split
	Days, Months chart
	Accounts     []accountSpend // this month, the largest first
}

// firstMonth is the start of the oldest month the dashboard shows.
func firstMonth(now time.Time, paris *time.Location) time.Time {
	today := midnight(now, paris)
	return today.AddDate(0, 1-dashboardMonths, 1-today.Day())
}

// buildDashboard sums calls into the days and months that end at now in paris.
func buildDashboard(calls []usageCall, now time.Time, paris *time.Location) dashboard {
	today := midnight(now, paris)
	first := firstMonth(now, paris)
	month := first.AddDate(0, dashboardMonths-1, 0)
	days, dayAt := make([]column, dashboardDays), make(map[string]int, dashboardDays)
	for i := range days {
		d := today.AddDate(0, 0, i+1-dashboardDays)
		dayAt[parisDay(d, paris)] = i
		days[i].Label = frShortDay(d)
		if (dashboardDays-1-i)%7 == 0 {
			days[i].Tick = strconv.Itoa(d.Day()) + " " + frMonthsShort[d.Month()-1]
		}
	}
	months, monthAt := make([]column, dashboardMonths), make(map[string]int, dashboardMonths)
	for i := range months {
		m := first.AddDate(0, i, 0)
		monthAt[m.Format("2006-01")] = i
		months[i].Label = frMonth(m)
		if (dashboardMonths-1-i)%2 == 0 {
			months[i].Tick = frMonthsShort[m.Month()-1]
		}
	}
	accounts := map[string]spend{}
	for _, c := range calls {
		t := c.At.In(paris)
		if i, ok := dayAt[parisDay(t, paris)]; ok {
			days[i].split = days[i].add(c)
		}
		if i, ok := monthAt[t.Format("2006-01")]; ok {
			months[i].split = months[i].add(c)
		}
		if !t.Before(month) {
			accounts[c.Account] = accounts[c.Account].add(c.Cost)
		}
	}
	return dashboard{
		Today: days[dashboardDays-1].split, Month: months[dashboardMonths-1].split,
		Days: draw(days), Months: draw(months), Accounts: ranked(accounts),
	}
}

// draw scales the columns to a round top and lays out their bars.
func draw(cols []column) chart {
	cost := slices.ContainsFunc(cols, func(c column) bool { return c.Cost().Valid }) &&
		!slices.ContainsFunc(cols, func(c column) bool { return !c.Assistant.priced() || !c.Suggest.priced() })
	var largest int64
	for _, c := range cols {
		largest = max(largest, c.Assistant.value(cost)+c.Suggest.value(cost))
	}
	top := roundUp(largest)
	ch := chart{Cost: cost, Width: len(cols) * slotWidth, Columns: cols, Top: strconv.FormatInt(top, 10)}
	if cost {
		ch.Top = dollars(sql.NullInt64{Int64: top, Valid: true})
	}
	for i := range cols {
		c := &cols[i]
		c.X = i * slotWidth
		x := float64(c.X + (slotWidth-barWidth)/2)
		ha, hs := height(c.Assistant.value(cost), top), height(c.Suggest.value(cost), top)
		if ha > 0 {
			c.A = bar(x, 100-ha, 100, hs == 0)
		}
		if hs > 0 {
			bottom := 100 - ha
			if ha > 0 {
				bottom -= gap
			}
			c.S = bar(x, min(100-ha-hs, bottom-minHeight), bottom, true)
		}
	}
	return ch
}

// roundUp rounds v up to 1, 2, 2.5 or 5 times a power of ten, so the axis
// reads clean; 1 at least.
func roundUp(v int64) int64 {
	for p := int64(1); ; p *= 10 {
		for _, top := range []int64{p, 2 * p, 5 * p / 2, 5 * p} {
			if top >= v {
				return top
			}
		}
	}
}

func height(v, top int64) float64 {
	if v == 0 {
		return 0
	}
	return max(100*float64(v)/float64(top), minHeight)
}

// bar is the path of a segment from y down to bottom; its top corners are
// rounded when it ends the column. The radii differ on each axis: the
// viewBox stretches to the chart's box.
func bar(x, y, bottom float64, round bool) string {
	if !round {
		return fmt.Sprintf("M%g %gV%.2fh%dV%gZ", x, bottom, y, barWidth, bottom)
	}
	const rx = 1.2
	ry := min(2, bottom-y)
	return fmt.Sprintf("M%g %gV%.2fA%g %.2f 0 0 1 %g %.2fH%gA%g %.2f 0 0 1 %g %.2fV%gZ",
		x, bottom, y+ry, rx, ry, x+rx, y, x+barWidth-rx, rx, ry, x+barWidth, y+ry, bottom)
}

// ranked sorts the month's accounts, the largest first: by cost when every
// call had a price, else by calls.
func ranked(accounts map[string]spend) []accountSpend {
	out := make([]accountSpend, 0, len(accounts))
	cost := true
	for account, s := range accounts {
		out = append(out, accountSpend{Account: account, spend: s})
		cost = cost && s.Cost.Valid
	}
	slices.SortFunc(out, func(a, b accountSpend) int {
		return cmp.Or(cmp.Compare(b.value(cost), a.value(cost)), cmp.Compare(a.Account, b.Account))
	})
	if len(out) > 0 && out[0].value(cost) > 0 {
		for i := range out {
			out[i].Share = int(math.Round(100 * float64(out[i].value(cost)) / float64(out[0].value(cost))))
		}
	}
	return out
}

// dollars formats micro-dollars the French way: cents, or four decimals
// under one cent, where single suggestions live.
func dollars(micro sql.NullInt64) string {
	if !micro.Valid {
		return "—"
	}
	format := "%.2f\u00a0$"
	if micro.Int64 > 0 && micro.Int64 < 10_000 {
		format = "%.4f\u00a0$"
	}
	return strings.Replace(fmt.Sprintf(format, float64(micro.Int64)/1e6), ".", ",", 1)
}

// usageCalls reads the model calls since since: the assistant's, quota
// refusals left out, and the form's.
func (s *Server) usageCalls(ctx context.Context, since time.Time) ([]usageCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account, at, cost_micro_usd FROM assistant_usage WHERE at >= ? AND outcome != ?
		UNION ALL SELECT '', at, cost_micro_usd FROM suggest_usage WHERE at >= ?`, since.Unix(), outcomeLimit, since.Unix())
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (c usageCall, err error) {
		var at int64
		err = rows.Scan(&c.Account, &at, &c.Cost)
		c.At = time.Unix(at, 0)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("usage calls: %w", err)
	}
	return out, nil
}
