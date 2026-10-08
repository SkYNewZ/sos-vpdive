package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Answer limits (design: Limits).
const (
	MaxToolCalls  = 8
	AnswerTimeout = 90 * time.Second
)

// toolLimitResult is what a call past MaxToolCalls gets instead of data.
const toolLimitResult = `{"erreur":"Limite de 8 lectures atteinte pour cette question : réponds avec ce que tu as."}`

// unknownTool stands for a tool name the model made up.
const unknownTool = "unknown"

// cutNote ends an answer cut at max_tokens: its text is kept, and the
// resolver reads that it is incomplete.
const cutNote = "\n\n_Réponse coupée : limite de longueur atteinte._"

// fenceLine opens or closes a fenced block: three backquotes or tildes at
// least, indented by three spaces at most.
var fenceLine = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

// toolMarkup is a tool call the model wrote as text instead of making it:
// one of DeepSeek's tags, whose name (DSML, tool…) follows a '<' and bars,
// ASCII or fullwidth (U+FF5C), or a function-call tag of another family.
var toolMarkup = regexp.MustCompile(`(?i)</?\s*(?:[|\x{ff5c}]+\s*(?:DSML|tool)|tool_calls?\b|function_calls?\b|invoke\s+name=)`)

// Tools is what the model may call during one answer. Run executes one call,
// of a name in Defs or one the model made up, and returns the JSON the model
// reads and the step the resolver sees. A call the model got wrong (unknown
// tool or ref, bad dates) is answered in that JSON; an error stops the answer.
type Tools struct {
	Defs []Tool
	Run  func(ctx context.Context, name string, input json.RawMessage) (result, step string, err error)
}

// Events receive what the resolver sees while an answer runs; any may be nil.
// Every turn streams its text; Turn says a new model call starts after tool
// results, so the text streamed so far was not the answer. Retry says the
// last reply wrote a tool call as text: what it streamed must go at once,
// and the same call runs again (Turn follows).
type Events struct {
	Text     func(delta string)
	Thinking func()
	Step     func(label string)
	Turn     func()
	Retry    func()
}

// Result is what an answer said and cost.
type Result struct {
	Text      string    // Markdown: the last turn's text; what came before the tools is narration
	History   []Message // the conversation grown by this answer; nil on error
	Usage     Usage
	Calls     int
	Tools     map[string]int // calls by tool name, "unknown" for a name not in Defs
	FirstText time.Duration  // from the start to the first word; 0 without text
}

// Answer runs one question to its answer: history ends with the resolver's
// message. Past MaxToolCalls the model must answer with what it read: a
// tool call then ends the answer with ErrInvalid, and so does a last turn
// the conversation could not keep (see lastTurn). A last turn that writes a
// tool call as text is dropped and its call made again, once. On error,
// Result keeps what was spent and no History: the conversation rolls back.
func (c *Client) Answer(ctx context.Context, system string, history []Message, tools Tools, ev Events) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, AnswerTimeout)
	defer cancel()
	start := time.Now()
	res := Result{Tools: map[string]int{}}
	msgs := slices.Clone(history)
	used := 0
	retried := false // the call to make is a retry
	onText := func(d string) {
		if d == "" {
			return
		}
		if res.FirstText == 0 {
			res.FirstText = time.Since(start)
		}
		if ev.Text != nil {
			ev.Text(d)
		}
	}
	for {
		if res.Calls > 0 && ev.Turn != nil {
			ev.Turn()
		}
		forced := used >= MaxToolCalls
		rep, err := c.stream(ctx, call{system: system, tools: tools.Defs, messages: msgs,
			noTools: forced, onText: onText, onThink: ev.Thinking})
		res.Calls++
		res.Usage.add(rep.Usage)
		if err != nil {
			return res, err
		}
		last := rep.StopReason != blockToolUse || len(rep.ToolUses) == 0
		if last && !retried && toolMarkup.MatchString(rep.Text) {
			retried = true
			if ev.Retry != nil {
				ev.Retry()
			}
			continue // the reply is dropped: msgs is the same request
		}
		retried = false
		msgs = append(msgs, Message{Role: "assistant", Content: rep.Content})
		if last {
			if res.Text, err = lastTurn(rep); err != nil {
				return res, err
			}
			res.History = msgs
			return res, nil
		}
		if forced {
			return res, fmt.Errorf("%w: tool call after tool_choice none", ErrInvalid)
		}
		results := make([]json.RawMessage, 0, len(rep.ToolUses))
		for _, u := range rep.ToolUses {
			out := toolLimitResult
			if used < MaxToolCalls {
				var step string
				if out, step, err = tools.Run(ctx, u.Name, u.Input); err != nil {
					if ctx.Err() != nil {
						return res, transportError(ctx, err) // the deadline or the resolver's leaving, not a failure of ours
					}
					return res, err
				}
				res.Tools[KnownTool(tools.Defs, u.Name)]++
				if ev.Step != nil {
					ev.Step(step)
				}
			}
			used++
			raw, err := json.Marshal(toolResultBlock{Type: "tool_result", ToolUseID: u.ID, Content: out})
			if err != nil {
				return res, fmt.Errorf("encode tool result: %w", err)
			}
			results = append(results, raw)
		}
		msgs = append(msgs, Message{Role: "user", Content: results})
	}
}

// lastTurn is the answer a last turn gives. It fails with ErrInvalid on a
// turn without text, or with a call left without its result: either, kept,
// would have the provider refuse every later question. It fails too on a
// tool call written as text, which the resolver must not read. A turn cut
// at max_tokens keeps its text, with cutNote.
func lastTurn(rep reply) (string, error) {
	switch {
	case len(rep.ToolUses) > 0:
		return "", fmt.Errorf("%w: tool call without a tool_use stop", ErrInvalid)
	case strings.TrimSpace(rep.Text) == "":
		return "", fmt.Errorf("%w: no text", ErrInvalid)
	case toolMarkup.MatchString(rep.Text):
		return "", fmt.Errorf("%w: tool call written as text", ErrInvalid)
	case rep.StopReason == stopMaxTokens:
		return closeFence(rep.Text) + cutNote, nil
	}
	return rep.Text, nil
}

// closeFence closes the fence a cut text leaves open: the cut note must not
// land in a draft the resolver copies.
func closeFence(text string) string {
	open := ""
	for line := range strings.Lines(text) {
		m := fenceLine.FindStringSubmatch(line)
		switch {
		case m == nil:
		case open == "":
			open = m[1]
		case m[1][0] == open[0] && len(m[1]) >= len(open) && strings.TrimSpace(line[len(m[0]):]) == "":
			open = ""
		}
	}
	if open == "" {
		return text
	}
	return strings.TrimSuffix(text, "\n") + "\n" + open
}

// KnownTool is name when defs offers that tool, "unknown" otherwise: a
// name the model made up never reaches telemetry or the usage journal.
func KnownTool(defs []Tool, name string) string {
	if slices.ContainsFunc(defs, func(t Tool) bool { return t.Name == name }) {
		return name
	}
	return unknownTool
}

type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
}
