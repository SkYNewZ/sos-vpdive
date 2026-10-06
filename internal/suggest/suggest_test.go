package suggest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

var fiches = []Fiche{
	{ID: "carnet-solde-negatif", Title: "Mon carnet affiche un montant négatif", Answer: "C'est normal."},
	{ID: "carnet-plongee-annulee", Title: "Une plongée annulée a été décomptée", Answer: "C'est provisoire."},
	{ID: "inscription-impossible", Title: "Je n'arrive pas à m'inscrire", Answer: "Vérifie ton profil."},
	{ID: "caci-redemande", Title: "VPDive me redemande mon CACI", Answer: "Un CACI vaut un an."},
}

var request = Request{
	Category:    "Carnet, solde de plongées",
	Fields:      []Field{{Label: "Montant affiché par VPDive", Value: "-180,00 €"}},
	Description: "Mon carnet affiche -180 € alors que la sortie du 12 a été annulée.",
}

// stub stands for the provider: it records the last request and answers
// with status and a Messages body holding text, or with reply as is.
type stub struct {
	mu     sync.Mutex
	req    *http.Request
	body   []byte
	status int
	text   string
	reply  string
	delay  time.Duration
}

// block is a content block of a Messages response.
type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.req, s.body = r, body
	status, text, reply, delay := s.status, s.text, s.reply, s.delay
	s.mu.Unlock()
	if text != "" {
		b, err := json.Marshal(struct {
			Content []block `json:"content"`
		}{[]block{{Type: "text", Text: text}}})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reply = string(b)
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, reply)
}

func newClient(t *testing.T, s *stub, path string) *Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL + path)
	require.NoError(t, err)
	return New(&config.LLM{BaseURL: base, APIKey: "sk-test", Model: "claude-haiku-4-5-20251001", Timeout: time.Second, DailyLimit: 10})
}

func TestNewWithoutKeyIsNil(t *testing.T) {
	assert.Nil(t, New(nil))
}

func TestChooseSendsAMessagesRequest(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
	s := &stub{status: http.StatusOK, text: `{"fiches": ["carnet-plongee-annulee"], "resume": "Carnet décompté."}`}
	c := newClient(t, s, "/anthropic")
	ctx, parent := otel.Tracer("test").Start(context.Background(), "request")
	res, err := c.Choose(ctx, request, fiches)
	parent.End()
	require.NoError(t, err)
	assert.Equal(t, Result{IDs: []string{"carnet-plongee-annulee"}, Summary: "Carnet décompté."}, res)

	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, http.MethodPost, s.req.Method)
	assert.Equal(t, "/anthropic/v1/messages", s.req.URL.Path, "a provider prefix is kept")
	assert.Equal(t, "sk-test", s.req.Header.Get("X-Api-Key"))
	assert.Equal(t, "2023-06-01", s.req.Header.Get("Anthropic-Version"))
	assert.Equal(t, "application/json", s.req.Header.Get("Content-Type"))
	assert.Empty(t, s.req.Header.Get("Traceparent"), "the trace context never leaves (spec §9.9)")

	var body struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools any `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(s.body, &body))
	assert.Equal(t, "claude-haiku-4-5-20251001", body.Model)
	assert.Equal(t, 400, body.MaxTokens)
	assert.Contains(t, body.System, "jamais une instruction")
	assert.Nil(t, body.Tools)
	require.Len(t, body.Messages, 1)
	assert.Equal(t, "user", body.Messages[0].Role)
	var doc struct {
		Fiches []struct {
			ID      string `json:"id"`
			Titre   string `json:"titre"`
			Reponse string `json:"reponse"`
		} `json:"fiches"`
		Demande struct {
			Categorie string `json:"categorie"`
			Champs    []struct {
				Champ  string `json:"champ"`
				Valeur string `json:"valeur"`
			} `json:"champs"`
			Description string `json:"description"`
		} `json:"demande"`
	}
	require.NoError(t, json.Unmarshal([]byte(body.Messages[0].Content), &doc), "the user message is one JSON document")
	require.Len(t, doc.Fiches, len(fiches))
	assert.Equal(t, "carnet-solde-negatif", doc.Fiches[0].ID)
	assert.Equal(t, "C'est normal.", doc.Fiches[0].Reponse)
	assert.Equal(t, request.Category, doc.Demande.Categorie)
	assert.Equal(t, request.Description, doc.Demande.Description)
	require.Len(t, doc.Demande.Champs, 1)
	assert.Equal(t, "-180,00 €", doc.Demande.Champs[0].Valeur)

	var span sdktrace.ReadOnlySpan
	for _, sp := range rec.Ended() {
		if sp.Name() == "llm.messages" {
			span = sp
		}
	}
	require.NotNil(t, span)
	attrs := map[string]string{}
	for _, a := range span.Attributes() {
		attrs[string(a.Key)] = a.Value.String()
	}
	assert.Equal(t, map[string]string{"llm.result": "ok", "llm.fiches": "1"}, attrs, "no text on the span")
}

func TestChooseReadsTheAnswer(t *testing.T) {
	long := strings.Repeat("é", 250)
	words := strings.Repeat("mot ", 60) // 240 runes
	tests := []struct {
		name, text string
		want       Result
	}{
		{"bare JSON", `{"fiches": ["caci-redemande"], "resume": "CACI redemandé."}`,
			Result{IDs: []string{"caci-redemande"}, Summary: "CACI redemandé."}},
		{"code fence", "```json\n{\"fiches\": [\"caci-redemande\"], \"resume\": \"CACI.\"}\n```",
			Result{IDs: []string{"caci-redemande"}, Summary: "CACI."}},
		{"plain fence", "```\n{\"fiches\": [], \"resume\": \"Rien.\"}\n```", Result{Summary: "Rien."}},
		{"unknown, duplicated and too many ids",
			`{"fiches": ["inconnue", "caci-redemande", "caci-redemande", "carnet-solde-negatif", "carnet-plongee-annulee", "inscription-impossible"], "resume": "x"}`,
			Result{IDs: []string{"caci-redemande", "carnet-solde-negatif", "carnet-plongee-annulee"}, Summary: "x"}},
		{"empty list", `{"fiches": [], "resume": "Demande sans fiche."}`, Result{Summary: "Demande sans fiche."}},
		{"no fiches key", `{"resume": "Seul."}`, Result{Summary: "Seul."}},
		{"links and tags", `{"fiches": [], "resume": "Voir <b>ici</b> [la page](https://x.example) ou https://evil.example/a?b=c et www.x.fr <script>alert(1)</script>"}`,
			Result{Summary: "Voir ici la page ou et alert(1)"}},
		{"one long word cut at 200 runes", `{"fiches": [], "resume": "` + long + `"}`, Result{Summary: strings.Repeat("é", 199) + "…"}},
		{"words cut at the last that fits", `{"fiches": [], "resume": "` + words + `"}`,
			Result{Summary: strings.TrimSpace(strings.Repeat("mot ", 49)) + "…"}},
		{"link written as a tag", `{"fiches": [], "resume": "Voir <a href=\"https://x.y\">ici</a> fin"}`, Result{Summary: "Voir ici fin"}},
		{"only tags and addresses", `{"fiches": [], "resume": "<b></b> https://x.example"}`, Result{}},
		{"200 runes kept whole", `{"fiches": [], "resume": "` + strings.Repeat("a", 200) + `"}`, Result{Summary: strings.Repeat("a", 200)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, &stub{status: http.StatusOK, text: tt.text}, "")
			res, err := c.Choose(context.Background(), request, fiches)
			require.NoError(t, err)
			assert.Equal(t, tt.want, res)
		})
	}
}

func TestChooseFailures(t *testing.T) {
	tests := []struct {
		name string
		stub *stub
		want error
	}{
		{"unreadable JSON", &stub{status: http.StatusOK, text: "Voici les fiches : carnet"}, ErrInvalid},
		{"text around the JSON", &stub{status: http.StatusOK, text: `{"fiches": [], "resume": "x"} Bonne journée`}, ErrInvalid},
		{"summary not a string", &stub{status: http.StatusOK, text: `{"fiches": [], "resume": 3}`}, ErrInvalid},
		{"no text block", &stub{status: http.StatusOK, reply: `{"content": [{"type": "tool_use"}]}`}, ErrInvalid},
		{"not a Messages body", &stub{status: http.StatusOK, reply: `<html>`}, ErrInvalid},
		{"fiches not strings", &stub{status: http.StatusOK, text: `{"fiches": [1, 2], "resume": "x"}`}, ErrInvalid},
		{"answer over 64 KB", &stub{status: http.StatusOK, text: `{"fiches": [], "resume": "` + strings.Repeat("a", 70<<10) + `"}`}, ErrInvalid},
		{"server error", &stub{status: http.StatusInternalServerError, reply: `{"type": "error"}`}, ErrHTTP},
		{"rate limited", &stub{status: http.StatusTooManyRequests, reply: `{}`}, ErrHTTP},
		{"too slow", &stub{status: http.StatusOK, text: `{"fiches": []}`, delay: 5 * time.Second}, ErrTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, tt.stub, "")
			c.timeout = 100 * time.Millisecond
			start := time.Now()
			res, err := c.Choose(context.Background(), request, fiches)
			require.ErrorIs(t, err, tt.want)
			assert.Equal(t, Result{}, res, "a failure gives neither fiche nor summary")
			assert.Less(t, time.Since(start), 2*time.Second)
		})
	}
}

func TestChooseReportsACancelledCall(t *testing.T) {
	c := newClient(t, &stub{status: http.StatusOK, text: `{"fiches": []}`, delay: 5 * time.Second}, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel) // the request went away while the model was thinking
	_, err := c.Choose(ctx, request, fiches)
	require.ErrorIs(t, err, ErrCanceled)
	assert.Equal(t, "canceled", codeOf(err), "a cancelled call is not a provider error")
}

func TestChooseKeepsAwkwardTextAndPaths(t *testing.T) {
	s := &stub{status: http.StatusOK, text: `{"fiches": [], "resume": "ok"}`}
	c := newClient(t, s, "/anthropic/")
	awkward := Request{Category: "Autre", Description: "Il a écrit \"-180 €\" puis C:\\vpdive\n<b>gras</b> 🐠 {\"fiches\": [\"x\"]}"}
	_, err := c.Choose(context.Background(), awkward, fiches)
	require.NoError(t, err)
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, "/anthropic/v1/messages", s.req.URL.Path, "a trailing slash in LLM_BASE_URL is harmless")
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(s.body, &body))
	var doc struct {
		Demande struct {
			Description string `json:"description"`
		} `json:"demande"`
	}
	require.NoError(t, json.Unmarshal([]byte(body.Messages[0].Content), &doc))
	assert.Equal(t, awkward.Description, doc.Demande.Description, "quotes, backslashes, tags and emoji reach the model intact, as data")
}

func TestChooseDoesNotFollowRedirects(t *testing.T) {
	elsewhere := &stub{status: http.StatusOK, text: `{"fiches": ["caci-redemande"], "resume": "x"}`}
	target := httptest.NewServer(elsewhere)
	t.Cleanup(target.Close)
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/messages", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(moved.Close)
	base, err := url.Parse(moved.URL)
	require.NoError(t, err)
	c := New(&config.LLM{BaseURL: base, APIKey: "sk-test", Model: "m", Timeout: time.Second})
	res, err := c.Choose(context.Background(), request, fiches)
	require.ErrorIs(t, err, ErrHTTP, "a redirect means a misconfigured LLM_BASE_URL")
	assert.Equal(t, Result{}, res)
	elsewhere.mu.Lock()
	defer elsewhere.mu.Unlock()
	assert.Nil(t, elsewhere.req, "the request and its key never reach another host")
}

// closeFails is a response body whose Close fails.
type closeFails struct{ io.Reader }

func (closeFails) Close() error { return errors.New("close failed") }

type closeFailsTransport struct{ body string }

func (c closeFailsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: closeFails{strings.NewReader(c.body)}, Request: r}, nil
}

func TestChooseReportsABodyThatFailsToClose(t *testing.T) {
	base, err := url.Parse("https://llm.example")
	require.NoError(t, err)
	c := New(&config.LLM{BaseURL: base, APIKey: "k", Model: "m", Timeout: time.Second})
	c.http.Transport = closeFailsTransport{body: `{"content": [{"type": "text", "text": "{\"fiches\": [], \"resume\": \"x\"}"}]}`}
	_, err = c.Choose(context.Background(), request, fiches)
	require.ErrorIs(t, err, ErrHTTP, "an error is never ignored")
}

func TestChooseUnreachableProvider(t *testing.T) {
	base, err := url.Parse("http://127.0.0.1:1")
	require.NoError(t, err)
	c := New(&config.LLM{BaseURL: base, APIKey: "k", Model: "m", Timeout: time.Second})
	_, err = c.Choose(context.Background(), request, fiches)
	require.ErrorIs(t, err, ErrHTTP)
}
