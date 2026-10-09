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
	"github.com/SkYNewZ/sos-vpdive/internal/store"
	"github.com/SkYNewZ/sos-vpdive/internal/suggest"
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
	if a := s.cfg.Assistant; a != nil && a.Prices.Set {
		cost = sql.NullInt64{Int64: u.Result.Usage.CostMicro(a.Prices), Valid: true}
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

// recordSuggestion writes a suggestion call, even after the member left; a
// failure is logged.
func (s *Server) recordSuggestion(ctx context.Context, res suggest.Result, d time.Duration, callErr error) {
	ctx = context.WithoutCancel(ctx)
	outcome := outcomeOK
	if callErr != nil {
		outcome = suggest.Code(callErr)
	}
	var cost sql.NullInt64
	if p := s.cfg.LLM.Prices; p.Set {
		u := assistant.Usage{Input: res.InputTokens, Output: res.OutputTokens}
		cost = sql.NullInt64{Int64: u.CostMicro(p), Valid: true}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO suggest_usage (at, model, input_tokens, output_tokens, cost_micro_usd, duration_ms, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, s.now().Unix(), s.cfg.LLM.Model, res.InputTokens, res.OutputTokens, cost, d.Milliseconds(),
		outcome); err != nil {
		s.logger.ErrorContext(ctx, "record suggestion usage", "error", err)
	}
}

// toolsText writes the tools JSON of a row as « find_member × 1, outing × 2 ».
var toolsText = strings.NewReplacer(`{`, "", `}`, "", `"`, "", `:`, " × ", `,`, ", ")

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
	dashboard

	Rows []journalRow
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

// assistantJournal shows the owner what the assistant and the suggestions
// cost, who asked and when (GET /assistant/journal).
func (s *Server) assistantJournal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.now()
	calls, err := s.usageCalls(ctx, firstMonth(now, s.paris))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows, err := s.journalRows(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p, err := s.adminPage(r, "Journal de l'assistant")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = journalData{dashboard: buildDashboard(calls, now, s.paris), Rows: rows}
	s.render(w, r, http.StatusOK, "assistant_journal", p)
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
