package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

// benchMetrics is one line of metrics.jsonl.
type benchMetrics struct {
	Message    string         `json:"message"`
	Model      string         `json:"model"`
	Thinking   bool           `json:"thinking"`
	Outcome    string         `json:"outcome"`
	Calls      int            `json:"calls"`
	Tools      map[string]int `json:"tools"`
	Input      int            `json:"input_tokens"`
	Output     int            `json:"output_tokens"`
	CacheRead  int            `json:"cache_read_tokens"`
	FirstMS    int64          `json:"first_token_ms"`
	DurationMS int64          `json:"duration_ms"`
}

// assistantBench answers every message file of -messages with the assistant,
// headless, against the configured database, and writes to -out one
// transcript per message and metrics.jsonl. It never records usage. Only
// regular files named message* are read: the directory may hold a grading
// grid that must never reach the model.
func assistantBench(ctx context.Context, getenv func(string) string, args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("assistant-bench", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("messages", "", "directory of messages, one per file")
	out := fs.String("out", "", "directory for transcripts and metrics")
	account := fs.String("account", "bench", "committee account the answers run as")
	if err := fs.Parse(args); err != nil || *dir == "" || *out == "" {
		return usageError{"assistant-bench needs -messages DIR and -out DIR"}
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	if cfg.Assistant == nil {
		return errors.New("assistant-bench needs ASSISTANT_ENABLED=true and LLM_API_KEY")
	}
	a, err := setup(ctx, cfg, telemetry.NewLogger(io.Discard, cfg.LogLevel))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, a.db.Close()) }()
	root, err := os.OpenRoot(*dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	entries, err := os.ReadDir(*dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	dest, err := os.OpenRoot(*out)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dest.Close()) }()
	metrics, err := dest.OpenFile("metrics.jsonl", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, metrics.Close()) }()
	enc := json.NewEncoder(metrics)
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), "message") {
			continue
		}
		text, err := root.ReadFile(e.Name())
		if err != nil {
			return err
		}
		r, err := a.web.BenchAnswer(ctx, *account, string(text))
		if err != nil {
			return err
		}
		steps := "(aucune)"
		if len(r.Steps) > 0 {
			steps = "- " + strings.Join(r.Steps, "\n- ")
		}
		transcript := "# " + e.Name() + "\n\n## Question\n\n" + string(text) + "\n\n## Étapes\n\n" + steps +
			"\n\n## Réponse (" + r.Outcome + ")\n\n" + r.Answer + "\n"
		if err := dest.WriteFile(e.Name()+".md", []byte(transcript), 0o600); err != nil {
			return err
		}
		u := r.Result.Usage
		if err := enc.Encode(benchMetrics{Message: e.Name(), Model: cfg.Assistant.Model, Thinking: cfg.Assistant.Thinking,
			Outcome: r.Outcome, Calls: r.Result.Calls, Tools: r.Result.Tools, Input: u.Input, Output: u.Output, CacheRead: u.CacheRead,
			FirstMS: r.Result.FirstText.Milliseconds(), DurationMS: r.Duration.Milliseconds()}); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "%s: %s, %d calls, %.1f s\n", e.Name(), r.Outcome, r.Result.Calls, r.Duration.Seconds()); err != nil {
			return err
		}
	}
	return nil
}
