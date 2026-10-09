package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provider answers every Messages call with reply, streamed when asked.
func provider(t *testing.T, reply string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := io.WriteString(w, reply); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// benchDirs writes one message file and returns its directory and an
// output directory.
func benchDirs(t *testing.T, message string) (dir, out string) {
	t.Helper()
	dir, out = t.TempDir(), filepath.Join(t.TempDir(), "out")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "message1"), []byte(message), 0o600))
	return dir, out
}

func TestAssistantBenchWritesMetrics(t *testing.T) {
	stream := "event: x\ndata: " + `{"type":"message_start","message":{"usage":{"input_tokens":1000,"cache_creation_input_tokens":4000,"cache_read_input_tokens":3000}}}` +
		"\n\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Solde juste."}}` +
		"\n\ndata: " + `{"type":"content_block_stop","index":0}` +
		"\n\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1000}}` +
		"\n\ndata: " + `{"type":"message_stop"}` + "\n\n"
	env := devEnv(t, freePort(t))
	env["ASSISTANT_ENABLED"], env["ASSISTANT_API_KEY"], env["ASSISTANT_BASE_URL"] = "true", "sk-test", provider(t, stream)
	env["ASSISTANT_EFFORT"], env["ASSISTANT_PRICE_INPUT"], env["ASSISTANT_PRICE_OUTPUT"], env["ASSISTANT_PRICE_CACHED"] = "low", "2", "10", "0.2"
	dir, out := benchDirs(t, "Mon carnet est faux.")
	require.NoError(t, run(context.Background(), []string{"assistant-bench", "-messages", dir, "-out", out}, getenv(env), io.Discard))

	data, err := os.ReadFile(filepath.Join(out, "metrics.jsonl"))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m), "one line for one message")
	assert.Equal(t, "ok", m["outcome"])
	assert.Equal(t, "claude-sonnet-5-5", m["model"])
	assert.Equal(t, "low", m["effort"])
	assert.InDelta(t, 4000, m["cache_write_tokens"], 0)
	assert.InDelta(t, 22_600, m["cost_micro_usd"], 0, "2 000 + 10 000 + 600 + 10 000: cache writes at 1.25 times the input")
}

func TestSuggestBenchWritesResults(t *testing.T) {
	reply := `{"content":[{"type":"text","text":"{\"fiches\":[\"carnet-solde-negatif\"],\"resume\":\"Solde contesté.\"}"}],` +
		`"usage":{"input_tokens":3100,"output_tokens":40}}`
	env := devEnv(t, freePort(t))
	env["LLM_API_KEY"], env["LLM_BASE_URL"], env["LLM_MODEL"] = "sk-test", provider(t, reply), "claude-haiku-5-5"
	dir, out := benchDirs(t, `{"category":"carnet","values":{"carnet":"10-n1n2"},"description":"Mon carnet est négatif."}`)
	require.NoError(t, run(context.Background(), []string{"suggest-bench", "-messages", dir, "-out", out, "-runs", "2"}, getenv(env), io.Discard))

	data, err := os.ReadFile(filepath.Join(out, "suggestions.jsonl"))
	require.NoError(t, err)
	dec := json.NewDecoder(bytes.NewReader(data))
	for run := 1; run <= 2; run++ {
		var line map[string]any
		require.NoError(t, dec.Decode(&line), "one line per run")
		assert.Equal(t, "message1", line["message"])
		assert.InDelta(t, run, line["run"], 0)
		assert.Equal(t, "claude-haiku-5-5", line["model"])
		assert.Equal(t, "ok", line["outcome"])
		assert.Equal(t, []any{"carnet-solde-negatif"}, line["fiches"])
		assert.Equal(t, "Solde contesté.", line["summary"])
		assert.InDelta(t, 3100, line["input_tokens"], 0)
		assert.InDelta(t, 40, line["output_tokens"], 0)
	}
}

func TestSuggestBenchUsage(t *testing.T) {
	err := run(context.Background(), []string{"suggest-bench"}, func(string) string { return "" }, io.Discard)
	assert.ErrorContains(t, err, "-messages")
}
