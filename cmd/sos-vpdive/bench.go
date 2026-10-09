package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// benchMetrics is one line of metrics.jsonl.
type benchMetrics struct {
	Message    string         `json:"message"`
	Model      string         `json:"model"`
	Thinking   bool           `json:"thinking"`
	Effort     config.Effort  `json:"effort,omitempty"`
	Outcome    string         `json:"outcome"`
	Calls      int            `json:"calls"`
	Tools      map[string]int `json:"tools"`
	Input      int            `json:"input_tokens"`
	Output     int            `json:"output_tokens"`
	CacheRead  int            `json:"cache_read_tokens"`
	CacheWrite int            `json:"cache_write_tokens"`
	CostMicro  *int64         `json:"cost_micro_usd,omitempty"` // with ASSISTANT_PRICE_*
	FirstMS    int64          `json:"first_token_ms"`
	DurationMS int64          `json:"duration_ms"`
}

// suggestMetrics is one line of suggestions.jsonl.
type suggestMetrics struct {
	Message    string   `json:"message"`
	Run        int      `json:"run"`
	Model      string   `json:"model"`
	Outcome    string   `json:"outcome"`
	Fiches     []string `json:"fiches"`
	Summary    string   `json:"summary"`
	Input      int      `json:"input_tokens"`
	Output     int      `json:"output_tokens"`
	DurationMS int64    `json:"duration_ms"`
}

// bench is what both benches share: the app against the configured
// database, the message files and the output directory. Only regular files
// named message* are read: the directory may hold a grading grid that must
// never reach the model.
type bench struct {
	app      *app
	messages *os.Root
	names    []string
	out      *os.Root
}

func openBench(ctx context.Context, cfg *config.Config, dir, out string) (b *bench, err error) {
	b = &bench{}
	defer func() {
		if err != nil {
			err = errors.Join(err, b.close())
		}
	}()
	if b.app, err = setup(ctx, cfg, telemetry.NewLogger(io.Discard, cfg.LogLevel)); err != nil {
		return b, err
	}
	if b.messages, err = os.OpenRoot(dir); err != nil {
		return b, err
	}
	entries, err := fs.ReadDir(b.messages.FS(), ".")
	if err != nil {
		return b, err
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), "message") {
			b.names = append(b.names, e.Name())
		}
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return b, err
	}
	b.out, err = os.OpenRoot(out)
	return b, err
}

func (b *bench) close() error {
	var errs []error
	for _, r := range []*os.Root{b.messages, b.out} {
		if r != nil {
			errs = append(errs, r.Close())
		}
	}
	if b.app != nil {
		errs = append(errs, b.app.db.Close())
	}
	return errors.Join(errs...)
}

// assistantBench answers every message file of -messages with the assistant,
// headless, against the configured database, and writes to -out one
// transcript per message and metrics.jsonl. It never records usage.
func assistantBench(ctx context.Context, getenv func(string) string, args []string, stdout io.Writer) (err error) {
	flags := flag.NewFlagSet("assistant-bench", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("messages", "", "directory of messages, one per file")
	out := flags.String("out", "", "directory for transcripts and metrics")
	account := flags.String("account", "bench", "committee account the answers run as")
	if err := flags.Parse(args); err != nil || *dir == "" || *out == "" {
		return usageError{"assistant-bench needs -messages DIR and -out DIR"}
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	if cfg.Assistant == nil {
		return errors.New("assistant-bench needs ASSISTANT_ENABLED=true and ASSISTANT_API_KEY")
	}
	b, err := openBench(ctx, cfg, *dir, *out)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, b.close()) }()
	metrics, err := b.out.OpenFile("metrics.jsonl", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, metrics.Close()) }()
	enc := json.NewEncoder(metrics)
	for _, name := range b.names {
		if err := ctx.Err(); err != nil {
			return err // interrupted: the remaining messages would all read « canceled »
		}
		text, err := b.messages.ReadFile(name)
		if err != nil {
			return err
		}
		r, err := b.app.web.BenchAnswer(ctx, *account, string(text))
		if err != nil {
			return err
		}
		steps := "(aucune)"
		if len(r.Steps) > 0 {
			steps = "- " + strings.Join(r.Steps, "\n- ")
		}
		transcript := "# " + name + "\n\n## Question\n\n" + string(text) + "\n\n## Étapes\n\n" + steps +
			"\n\n## Réponse (" + r.Outcome + ")\n\n" + r.Answer + "\n"
		if err := b.out.WriteFile(name+".md", []byte(transcript), 0o600); err != nil {
			return err
		}
		a, u := cfg.Assistant, r.Result.Usage
		m := benchMetrics{Message: name, Model: a.Model, Thinking: a.Thinking, Effort: a.Effort,
			Outcome: r.Outcome, Calls: r.Result.Calls, Tools: r.Result.Tools, Input: u.Input, Output: u.Output,
			CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
			FirstMS: r.Result.FirstText.Milliseconds(), DurationMS: r.Duration.Milliseconds()}
		if a.Priced {
			cost := u.CostMicro(a)
			m.CostMicro = &cost
		}
		if err := enc.Encode(m); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "%s: %s, %d calls, %.1f s\n", name, r.Outcome, r.Result.Calls, r.Duration.Seconds()); err != nil {
			return err
		}
	}
	return nil
}

// suggestBench asks for the suggestions of every message file of -messages,
// -runs times each, as the member form does but without its daily cap, and
// writes suggestions.jsonl to -out. A message file is a JSON request:
// {"category": "carnet", "values": {"carnet": "10-n1n2"}, "description": "…"}.
func suggestBench(ctx context.Context, getenv func(string) string, args []string, stdout io.Writer) (err error) {
	flags := flag.NewFlagSet("suggest-bench", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("messages", "", "directory of requests, one JSON file each")
	out := flags.String("out", "", "directory for the results")
	runs := flags.Int("runs", 1, "calls per request")
	if err := flags.Parse(args); err != nil || *dir == "" || *out == "" || *runs < 1 {
		return usageError{"suggest-bench needs -messages DIR and -out DIR"}
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	if cfg.LLM == nil {
		return errors.New("suggest-bench needs LLM_API_KEY")
	}
	b, err := openBench(ctx, cfg, *dir, *out)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, b.close()) }()
	results, err := b.out.OpenFile("suggestions.jsonl", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, results.Close()) }()
	enc := json.NewEncoder(results)
	for _, name := range b.names {
		data, err := b.messages.ReadFile(name)
		if err != nil {
			return err
		}
		var req struct {
			tickets.Fields

			Description string `json:"description"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		for run := 1; run <= *runs; run++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			r, err := b.app.web.BenchSuggest(ctx, tickets.Submission{Fields: req.Fields, Description: req.Description})
			if err != nil {
				return err
			}
			if err := enc.Encode(suggestMetrics{Message: name, Run: run, Model: cfg.LLM.Model, Outcome: r.Outcome,
				Fiches: r.IDs, Summary: r.Summary, Input: r.InputTokens, Output: r.OutputTokens,
				DurationMS: r.Duration.Milliseconds()}); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(stdout, "%s #%d: %s, %.1f s\n", name, run, r.Outcome, r.Duration.Seconds()); err != nil {
				return err
			}
		}
	}
	return nil
}
