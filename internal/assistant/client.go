// Package assistant runs the committee assistant (design 2026-10-08): a
// streamed Messages API conversation in which the model reads the club's data
// through read-only tools its caller provides. It holds no store; its
// conversations live in memory only.
package assistant

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

const (
	tracerName     = "github.com/SkYNewZ/sos-vpdive/internal/assistant"
	apiVersion     = "2023-06-01"
	errorBodyLimit = 4 << 10
	lineLimit      = 1 << 20 // one stream line: a delta never comes near
	legacyBudget   = 2048    // thinking tokens of a Claude model before 4.6

	// Content block types.
	blockText     = "text"
	blockThinking = "thinking"
	blockRedacted = "redacted_thinking"
	blockToolUse  = "tool_use" // a block asking for a tool, and the stop reason of its reply

	stopMaxTokens = "max_tokens" // a reply cut by the length limit
	stopRefusal   = "refusal"    // Claude declined, maybe after some text
)

// legacyThinking matches the Claude models before 4.6: they refuse adaptive
// thinking and take a token budget.
var legacyThinking = regexp.MustCompile(`^claude-(3|[a-z]+-4-[0-5])`)

// idleTimeout ends a stream that sends nothing for that long; tests shorten it.
var idleTimeout = 30 * time.Second

// Failures, by stable code: spans and the usage journal carry the code.
var (
	ErrTimeout  = errors.New("timeout")
	ErrCanceled = errors.New("canceled")
	ErrHTTP     = errors.New("http_error")
	ErrInvalid  = errors.New("invalid_output")
	errIdle     = errors.New("stream idle")
)

// Code returns the stable code of err: one of the errors above, or internal
// for a failure of this server (a store, a template).
func Code(err error) string {
	for _, e := range []error{ErrTimeout, ErrCanceled, ErrHTTP, ErrInvalid} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return "internal"
}

// Message is one message of a conversation, its blocks as the API takes them.
type Message struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

// UserText is a user message of one text block.
func UserText(text string) (Message, error) {
	raw, err := json.Marshal(textBlock{Type: blockText, Text: text})
	if err != nil {
		return Message{}, fmt.Errorf("encode user text: %w", err)
	}
	return Message{Role: "user", Content: []json.RawMessage{raw}}, nil
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Tool describes a tool the model may call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Usage counts the tokens of calls. Input leaves out the tokens read from or
// written to the provider's cache.
type Usage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
}

// CostMicro is what u cost at p, in micro-dollars: prices are per million
// tokens. A cache write costs 1.25 times the input, Anthropic's rate for the
// 5-minute cache that cache_control asks for; DeepSeek reports none.
func (u *Usage) CostMicro(p config.Prices) int64 {
	return (int64(u.Input)*p.Input + int64(u.CacheWrite)*p.Input*5/4 +
		int64(u.CacheRead)*p.Cached + int64(u.Output)*p.Output) / 1_000_000
}

func (u *Usage) add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
}

// merge keeps the larger count of each field: providers send usage in
// message_start and again, final, in message_delta.
func (u *Usage) merge(o Usage) {
	u.Input, u.Output = max(u.Input, o.Input), max(u.Output, o.Output)
	u.CacheRead, u.CacheWrite = max(u.CacheRead, o.CacheRead), max(u.CacheWrite, o.CacheWrite)
}

// ToolUse is a call the model asks for.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// reply is one model response, its blocks kept raw: thinking blocks must go
// back verbatim with the tool results.
type reply struct {
	Content    []json.RawMessage
	Text       string
	ToolUses   []ToolUse
	StopReason string
	Usage      Usage
}

// Client calls the model, streaming.
type Client struct {
	Model    string
	Thinking bool

	effort    config.Effort
	maxTokens int
	endpoint  string
	key       string
	http      *http.Client // plain: no trace header leaves, no redirect followed
}

// NewClient returns a client of a's provider, model and limits.
func NewClient(a *config.Assistant) *Client {
	return &Client{
		Model: a.Model, Thinking: a.Thinking, effort: a.Effort, maxTokens: a.MaxTokens,
		endpoint: a.BaseURL.JoinPath("v1", "messages").String(), key: a.APIKey,
		// The Messages API never redirects: following one would hand the key to another host.
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// call is one model call of an answer.
type call struct {
	system   string
	tools    []Tool
	messages []Message
	noTools  bool // tool_choice none: the model must answer with what it read
	onText   func(delta string)
	onThink  func()
}

type request struct {
	Model        string         `json:"model"`
	MaxTokens    int            `json:"max_tokens"`
	Stream       bool           `json:"stream"`
	Thinking     thinkingConfig `json:"thinking"`
	OutputConfig *outputConfig  `json:"output_config,omitempty"`
	CacheControl typeOnly       `json:"cache_control"` // automatic: Anthropic caches up to the last block
	System       []systemBlock  `json:"system"`
	Tools        []Tool         `json:"tools,omitempty"`
	ToolChoice   *typeOnly      `json:"tool_choice,omitempty"`
	Messages     []Message      `json:"messages"`
}

// thinkingConfig is adaptive reasoning with its summary streamed: Claude
// omits the text by default and then sends nothing while it thinks, longer
// than idleTimeout. DeepSeek takes the same shape.
type thinkingConfig struct {
	Type         string `json:"type"`
	Display      string `json:"display,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type outputConfig struct {
	Effort config.Effort `json:"effort"`
}

// systemBlock carries cache_control: Anthropic caches tools and system
// from it, a read point whatever the conversation; the request's own
// cache_control caches the conversation, which the next call of the tool
// loop resends. DeepSeek ignores both and caches every prefix by itself.
type systemBlock struct {
	Type         string   `json:"type"`
	Text         string   `json:"text"`
	CacheControl typeOnly `json:"cache_control"`
}

type typeOnly struct {
	Type string `json:"type"`
}

func (c *Client) request(k call) request {
	ephemeral := typeOnly{Type: "ephemeral"}
	r := request{
		Model: c.Model, MaxTokens: c.maxTokens, Stream: true, Thinking: thinkingConfig{Type: config.ThinkingOff(c.Model)},
		CacheControl: ephemeral, System: []systemBlock{{Type: blockText, Text: k.system, CacheControl: ephemeral}},
		Tools: k.tools, Messages: k.messages,
	}
	switch {
	case c.Thinking && legacyThinking.MatchString(c.Model):
		r.Thinking = thinkingConfig{Type: "enabled", BudgetTokens: legacyBudget}
	case c.Thinking:
		r.Thinking = thinkingConfig{Type: "adaptive", Display: "summarized"}
	}
	if c.effort != "" {
		r.OutputConfig = &outputConfig{Effort: c.effort}
	}
	if k.noTools {
		r.ToolChoice = &typeOnly{Type: "none"}
	}
	return r
}

// stream sends one call and reads its event stream, calling back on each
// text delta and when thinking starts.
func (c *Client) stream(ctx context.Context, k call) (rep reply, err error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "llm.messages", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	defer func() {
		code := "ok"
		if err != nil {
			code = Code(err)
			telemetry.Fail(span, code)
		}
		span.SetAttributes(attribute.String("llm.result", code), attribute.String("llm.stop_reason", rep.StopReason),
			attribute.Int("llm.tool_calls", len(rep.ToolUses)))
	}()
	body, err := json.Marshal(c.request(k))
	if err != nil {
		return reply{}, fmt.Errorf("encode request: %w", err)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := time.AfterFunc(idleTimeout, func() { cancel(errIdle) })
	defer idle.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return reply{}, fmt.Errorf("%w: %w", ErrHTTP, err)
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Anthropic-Version", apiVersion)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return reply{}, transportError(ctx, err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			rep, err = reply{Usage: rep.Usage}, fmt.Errorf("%w: %w", ErrHTTP, cerr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return reply{}, statusError(resp)
	}
	rep, err = readStream(resp.Body, idle, k)
	if err != nil && ctx.Err() != nil {
		return reply{Usage: rep.Usage}, transportError(ctx, err)
	}
	return rep, err
}

// statusError names a refused call by its status and the provider's error
// type, never its message: it may quote the request.
func statusError(resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	if err != nil {
		return fmt.Errorf("%w: status %d", ErrHTTP, resp.StatusCode)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil {
		body.Error.Type = "unreadable"
	}
	return fmt.Errorf("%w: status %d, %s", ErrHTTP, resp.StatusCode, cmp.Or(body.Error.Type, "no type"))
}

func transportError(ctx context.Context, err error) error {
	switch {
	case errors.Is(context.Cause(ctx), errIdle), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	case errors.Is(ctx.Err(), context.Canceled):
		return fmt.Errorf("%w: %w", ErrCanceled, err)
	}
	return fmt.Errorf("%w: %w", ErrHTTP, err)
}

// event is one stream event; Type selects the fields set.
type event struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage Usage `json:"usage"`
	} `json:"message"`
	ContentBlock *block `json:"content_block"`
	Delta        *delta `json:"delta"`
	Usage        *Usage `json:"usage"`
	Error        *struct {
		Type string `json:"type"`
	} `json:"error"`
}

type delta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// block is a content block being streamed.
type block struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Text      string `json:"text"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
	input     string // tool input fragments
}

type thinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type redactedBlock struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type toolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// raw is the block as it goes back to the API; keep is false for a block
// that must not: an empty text, or a type this client never offers.
func (b *block) raw() (raw json.RawMessage, keep bool, err error) {
	var v any
	switch b.Type {
	case blockText:
		if b.Text == "" {
			return nil, false, nil
		}
		v = textBlock{Type: blockText, Text: b.Text}
	case blockThinking:
		v = thinkingBlock{Type: blockThinking, Thinking: b.Thinking, Signature: b.Signature}
	case blockRedacted:
		v = redactedBlock{Type: blockRedacted, Data: b.Data}
	case blockToolUse:
		b.input = cmp.Or(b.input, "{}") // a call without arguments streams no input
		if !json.Valid([]byte(b.input)) {
			return nil, false, fmt.Errorf("%w: tool input is not JSON", ErrInvalid)
		}
		v = toolUseBlock{Type: blockToolUse, ID: b.ID, Name: b.Name, Input: json.RawMessage(b.input)}
	default:
		return nil, false, nil
	}
	raw, err = json.Marshal(v)
	if err != nil {
		return nil, false, fmt.Errorf("encode block: %w", err)
	}
	return raw, true, nil
}

// readStream reads the events of one response. On error the reply keeps
// the usage read so far: a cut call still cost its input.
func readStream(body io.Reader, idle *time.Timer, k call) (reply, error) {
	var (
		rep    reply
		blocks []*block
		done   bool
	)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), lineLimit)
	for sc.Scan() {
		idle.Reset(idleTimeout)
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue // event names, comments, blank lines
		}
		var ev event
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
			return reply{Usage: rep.Usage}, fmt.Errorf("%w: unreadable event: %w", ErrInvalid, err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				rep.Usage.merge(ev.Message.Usage)
			}
		case "content_block_start":
			if ev.ContentBlock == nil || ev.Index != len(blocks) {
				return reply{Usage: rep.Usage}, fmt.Errorf("%w: block %d out of order", ErrInvalid, ev.Index)
			}
			blocks = append(blocks, ev.ContentBlock)
			if ev.ContentBlock.Type == blockThinking && k.onThink != nil {
				k.onThink()
			}
			if ev.ContentBlock.Text != "" && k.onText != nil {
				k.onText(ev.ContentBlock.Text)
			}
		case "content_block_delta":
			if ev.Delta == nil || ev.Index < 0 || ev.Index >= len(blocks) {
				return reply{Usage: rep.Usage}, fmt.Errorf("%w: delta of unknown block %d", ErrInvalid, ev.Index)
			}
			applyDelta(blocks[ev.Index], *ev.Delta, k)
		case "message_delta":
			if ev.Delta != nil {
				rep.StopReason = cmp.Or(ev.Delta.StopReason, rep.StopReason) // a usage-only delta keeps it
			}
			if ev.Usage != nil {
				rep.Usage.merge(*ev.Usage)
			}
		case "message_stop":
			done = true
		case "error":
			typ := "unknown"
			if ev.Error != nil {
				typ = ev.Error.Type
			}
			return reply{Usage: rep.Usage}, fmt.Errorf("%w: stream error %s", ErrHTTP, typ)
		}
	}
	if err := sc.Err(); err != nil {
		return reply{Usage: rep.Usage}, fmt.Errorf("%w: read stream: %w", ErrHTTP, err)
	}
	if !done {
		return reply{Usage: rep.Usage}, fmt.Errorf("%w: stream ended before message_stop", ErrInvalid)
	}
	return assemble(blocks, rep)
}

// assemble turns the streamed blocks into rep's content, text and tool
// calls. On error the reply keeps its usage. A reply cut at max_tokens is
// no error here: Answer keeps its text, if any.
func assemble(blocks []*block, rep reply) (reply, error) {
	for _, b := range blocks {
		raw, keep, err := b.raw()
		if err != nil {
			return reply{Usage: rep.Usage}, err
		}
		if !keep {
			continue
		}
		rep.Content = append(rep.Content, raw)
		switch b.Type {
		case blockText:
			rep.Text += b.Text
		case blockToolUse:
			rep.ToolUses = append(rep.ToolUses, ToolUse{ID: b.ID, Name: b.Name, Input: json.RawMessage(b.input)})
		}
	}
	return rep, nil
}

func applyDelta(b *block, d delta, k call) {
	switch d.Type {
	case "text_delta":
		b.Text += d.Text
		if k.onText != nil {
			k.onText(d.Text)
		}
	case "thinking_delta":
		b.Thinking += d.Thinking
	case "signature_delta":
		b.Signature += d.Signature
	case "input_json_delta":
		b.input += d.PartialJSON
	}
}
