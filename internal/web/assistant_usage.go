package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// usageRetention is how long the assistant journal keeps a row.
const usageRetention = 12 // months

// usageEntry is one question for the journal: never its text.
type usageEntry struct {
	Account  string
	Origin   string // page or demande
	Model    string
	Thinking bool
	Result   assistant.Result
	Duration time.Duration
	Outcome  string
}

// costMicro is what usage cost, in micro-dollars: prices are per million tokens.
func costMicro(u assistant.Usage, a *config.Assistant) int64 {
	return (int64(u.Input+u.CacheWrite)*a.PriceInput + int64(u.CacheRead)*a.PriceCached + int64(u.Output)*a.PriceOutput) / 1_000_000
}

// recordUsage writes u, even after the resolver left; a failure is logged.
func (s *Server) recordUsage(ctx context.Context, u usageEntry) {
	ctx = context.WithoutCancel(ctx)
	tools := u.Result.Tools
	if tools == nil {
		tools = map[string]int{}
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		s.logger.ErrorContext(ctx, "record assistant usage", "error", err)
		return
	}
	var cost, first sql.NullInt64
	if a := s.cfg.Assistant; a != nil && a.Priced {
		cost = sql.NullInt64{Int64: costMicro(u.Result.Usage, a), Valid: true}
	}
	if u.Result.FirstText > 0 {
		first = sql.NullInt64{Int64: u.Result.FirstText.Milliseconds(), Valid: true}
	}
	us := u.Result.Usage
	if _, err := s.db.ExecContext(ctx, `INSERT INTO assistant_usage (account, at, origin, model, thinking, calls, tools,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micro_usd, first_token_ms, duration_ms, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Account, s.now().Unix(), u.Origin, u.Model, u.Thinking, u.Result.Calls, string(toolsJSON),
		us.Input, us.Output, us.CacheRead, us.CacheWrite, cost, first, u.Duration.Milliseconds(), u.Outcome); err != nil {
		s.logger.ErrorContext(ctx, "record assistant usage", "error", err)
	}
}

// toolsText writes the tools JSON of a row as « find_member × 1, outing × 2 ».
var toolsText = strings.NewReplacer(`{`, "", `}`, "", `"`, "", `:`, " × ", `,`, ", ")

// journalTotal sums an account's questions over a period.
type journalTotal struct {
	Account                  string
	Questions                int
	Input, Output, CacheRead int
	Cost                     sql.NullInt64
}

// journalRow is one question of the journal.
type journalRow struct {
	Account, Origin, Model, Tools, Outcome string
	At                                     time.Time
	Thinking                               bool
	Calls, Input, Output, CacheRead        int
	Cost, FirstMS                          sql.NullInt64
	DurationMS                             int64
}

type journalData struct {
	Today, Month []journalTotal
	Rows         []journalRow
}

// Outcomes the journal stores: answered, refused by the quota, or a code
// of assistant.Code.
const (
	outcomeOK       = "ok"
	outcomeLimit    = "limit"
	outcomeInternal = "internal"
	outcomeTimeout  = "timeout"
	outcomeCanceled = "canceled"
	outcomeHTTP     = "http_error"
	outcomeInvalid  = "invalid_output"
)

// outcomeLabel says how a question ended, in the journal.
var outcomeLabel = map[string]string{
	outcomeOK: "répondu", outcomeTimeout: "délai dépassé", outcomeHTTP: "erreur du fournisseur", outcomeInvalid: "réponse illisible",
	outcomeLimit: "quota atteint", outcomeCanceled: "arrêté", outcomeInternal: "erreur interne",
}

// assistantJournal shows the owner who used the assistant, when, and what
// it cost (GET /assistant/journal).
func (s *Server) assistantJournal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	day := midnight(s.now(), s.paris)
	var (
		d   journalData
		err error
	)
	if d.Today, err = s.journalTotals(ctx, day); err == nil {
		if d.Month, err = s.journalTotals(ctx, day.AddDate(0, 0, 1-day.Day())); err == nil {
			d.Rows, err = s.journalRows(ctx)
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p, err := s.adminPage(r, "Journal de l'assistant")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = d
	s.render(w, r, http.StatusOK, "assistant_journal", p)
}

func (s *Server) journalTotals(ctx context.Context, since time.Time) ([]journalTotal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account, COUNT(*), SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens),
		SUM(cost_micro_usd) FROM assistant_usage WHERE at >= ? GROUP BY account ORDER BY account`, since.Unix())
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (t journalTotal, err error) {
		err = rows.Scan(&t.Account, &t.Questions, &t.Input, &t.Output, &t.CacheRead, &t.Cost)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("assistant journal totals: %w", err)
	}
	return out, nil
}

func (s *Server) journalRows(ctx context.Context) ([]journalRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account, at, origin, model, thinking, calls, tools, input_tokens, output_tokens,
		cache_read_tokens, cost_micro_usd, first_token_ms, duration_ms, outcome FROM assistant_usage ORDER BY id DESC LIMIT 100`)
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (j journalRow, err error) {
		var at int64
		err = rows.Scan(&j.Account, &at, &j.Origin, &j.Model, &j.Thinking, &j.Calls, &j.Tools, &j.Input, &j.Output,
			&j.CacheRead, &j.Cost, &j.FirstMS, &j.DurationMS, &j.Outcome)
		j.At = time.Unix(at, 0)
		j.Tools = toolsText.Replace(j.Tools)
		j.Outcome = outcomeLabel[j.Outcome]
		return j, err
	})
	if err != nil {
		return nil, fmt.Errorf("assistant journal rows: %w", err)
	}
	return out, nil
}

// dollars formats micro-dollars the French way, four decimals: « 0,0123 $ ».
func dollars(micro sql.NullInt64) string {
	if !micro.Valid {
		return "—"
	}
	return strings.Replace(fmt.Sprintf("%.4f $", float64(micro.Int64)/1e6), ".", ",", 1)
}
