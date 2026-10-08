package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Answer limits (design: Limits).
const (
	MaxToolCalls  = 8
	AnswerTimeout = 90 * time.Second
)

// toolLimitResult is what a call past MaxToolCalls gets instead of data.
const toolLimitResult = `{"erreur":"Limite de 8 lectures atteinte pour cette question : réponds avec ce que tu as."}`

// unknownTool counts the calls to a name the model made up: that name came
// from the model, and the usage journal never records it.
const unknownTool = "unknown"

// Tools is what the model may call during one answer. Run executes one call
// and returns the JSON the model reads and the step the resolver sees. A
// call the model got wrong (unknown ref, bad dates) is answered in that
// JSON; an error stops the answer.
type Tools struct {
	Defs []Tool
	Run  func(ctx context.Context, name string, input json.RawMessage) (result, step string, err error)
}

// Events receive what the resolver sees while an answer runs; any may be nil.
type Events struct {
	Text     func(delta string)
	Thinking func()
	Step     func(label string)
}

// Result is what an answer said and cost.
type Result struct {
	Text      string    // Markdown: every turn's text, in order, a blank line apart
	History   []Message // the conversation grown by this answer; nil on error
	Usage     Usage
	Calls     int
	Tools     map[string]int // calls by tool name, "unknown" for a name not in Defs
	FirstText time.Duration  // from the start to the first word; 0 without text
}

// Answer runs one question to its answer: history ends with the resolver's
// message. Past MaxToolCalls the model must answer with what it read. On
// error, Result keeps what was spent and no History: the conversation
// rolls back.
func (c *Client) Answer(ctx context.Context, system string, history []Message, tools Tools, ev Events) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, AnswerTimeout)
	defer cancel()
	start := time.Now()
	res := Result{Tools: map[string]int{}}
	msgs := slices.Clone(history)
	used, separate := 0, false
	onText := func(d string) {
		if d == "" {
			return
		}
		if separate {
			d, separate = "\n\n"+d, false
		}
		if res.FirstText == 0 {
			res.FirstText = time.Since(start)
		}
		res.Text += d
		if ev.Text != nil {
			ev.Text(d)
		}
	}
	for {
		forced := used >= MaxToolCalls
		rep, err := c.stream(ctx, call{system: system, tools: tools.Defs, messages: msgs,
			noTools: forced, onText: onText, onThink: ev.Thinking})
		res.Calls++
		res.Usage.add(rep.Usage)
		if err != nil {
			return res, err
		}
		msgs = append(msgs, Message{Role: "assistant", Content: rep.Content})
		if rep.StopReason != blockToolUse || len(rep.ToolUses) == 0 {
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
					return res, err
				}
				res.Tools[journalName(tools.Defs, u.Name)]++
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
		separate = res.Text != ""
	}
}

// journalName is name when defs offers that tool, unknownTool otherwise.
func journalName(defs []Tool, name string) string {
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
