package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// textAndTool is a reply with a text, then one tool call.
func textAndTool(text, id, name, input string) string {
	return sse(messageStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+q(text)+`}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":`+q(id)+`,"name":`+q(name)+`,"input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+q(input)+`}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":10}}`,
		`{"type":"message_stop"}`)
}

type runner struct{ calls []string }

func (r *runner) tools() Tools {
	return Tools{Defs: []Tool{{Name: "find_member", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Run: func(_ context.Context, name string, input json.RawMessage) (string, string, error) {
			r.calls = append(r.calls, name+" "+string(input))
			return `{"candidats":[]}`, "Recherche faite", nil
		}}
}

func TestAnswerRunsTools(t *testing.T) {
	s := &scripted{replies: []string{
		textAndTool("Je cherche.", "call_1", "find_member", `{"query":"Léa"}`),
		textStream("Léa est introuvable."),
	}}
	c := newTestClient(t, s, false)
	r := &runner{}
	var steps, deltas []string
	res, err := c.Answer(context.Background(), "S", userMessages(t, "Q"), r.tools(), Events{
		Text: func(d string) { deltas = append(deltas, d) },
		Step: func(l string) { steps = append(steps, l) },
	})
	require.NoError(t, err)
	assert.Equal(t, "Je cherche.\n\nLéa est introuvable.", res.Text, "turns are separated by a blank line")
	assert.Equal(t, res.Text, strings.Join(deltas, ""), "what streamed is what is kept")
	assert.Equal(t, []string{`find_member {"query":"Léa"}`}, r.calls)
	assert.Equal(t, []string{"Recherche faite"}, steps)
	assert.Equal(t, 2, res.Calls)
	assert.Equal(t, map[string]int{"find_member": 1}, res.Tools)
	assert.Positive(t, res.FirstText)
	require.Len(t, res.History, 4, "question, tool call, tool result, answer")

	second := s.body(t, 1)["messages"].([]any)
	require.Len(t, second, 3)
	result := second[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	assert.Equal(t, map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": `{"candidats":[]}`}, result)
}

func TestAnswerSendsThinkingBack(t *testing.T) {
	s := &scripted{replies: []string{toolStream(true, [3]string{"call_1", "find_member", `{"query":"Léa"}`}), textStream("Fait.")}}
	_, err := newTestClient(t, s, true).Answer(context.Background(), "S", userMessages(t, "Q"), (&runner{}).tools(), Events{})
	require.NoError(t, err)
	assistantTurn := s.body(t, 1)["messages"].([]any)[1].(map[string]any)["content"].([]any)
	assert.Equal(t, map[string]any{"type": "thinking", "thinking": "Chercher Léa.", "signature": "sig"}, assistantTurn[0])
}

func TestAnswerForcesAnAnswerPastTheToolLimit(t *testing.T) {
	three := toolStream(false,
		[3]string{"a", "find_member", `{"query":"1"}`}, [3]string{"b", "find_member", `{"query":"2"}`},
		[3]string{"c", "find_member", `{"query":"3"}`})
	s := &scripted{replies: []string{three, three, three, textStream("Voici ce que j'ai.")}}
	r := &runner{}
	res, err := newTestClient(t, s, false).Answer(context.Background(), "S", userMessages(t, "Q"), r.tools(), Events{})
	require.NoError(t, err)
	assert.Len(t, r.calls, MaxToolCalls, "the ninth call is refused, not run")
	assert.Equal(t, 4, res.Calls)
	assert.Equal(t, map[string]any{"type": "none"}, s.body(t, 3)["tool_choice"])
	assert.Contains(t, s.body(t, 3), "tools", "the tools stay in the forced call: the cached prefix and tool_choice need them")
	assert.Contains(t, s.raw(t, 3), "Limite de 8 lectures atteinte")
}

func TestAnswerEndsWhenTheForcedCallStillAsksForATool(t *testing.T) {
	s := &scripted{replies: []string{toolStream(false, [3]string{"a", "find_member", `{"query":"1"}`})}}
	r := &runner{}
	res, err := newTestClient(t, s, false).Answer(context.Background(), "S", userMessages(t, "Q"), r.tools(), Events{})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Equal(t, "invalid_output", Code(err))
	assert.Len(t, r.calls, MaxToolCalls, "no tool runs for the forced call")
	assert.Equal(t, MaxToolCalls+1, res.Calls, "the forced call is the last one")
	assert.Nil(t, res.History)
	assert.Equal(t, map[string]any{"type": "none"}, s.body(t, MaxToolCalls)["tool_choice"])
}

func TestAnswerCountsAnInventedToolAsUnknown(t *testing.T) {
	s := &scripted{replies: []string{
		toolStream(false, [3]string{"a", "find_member", `{}`}, [3]string{"b", "Léa_Martin_lookup", `{}`}),
		textStream("Fait."),
	}}
	res, err := newTestClient(t, s, false).Answer(context.Background(), "S", userMessages(t, "Q"), (&runner{}).tools(), Events{})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"find_member": 1, "unknown": 1}, res.Tools, "a name the model made up never reaches the journal")
}

func TestAnswerFailuresRollBack(t *testing.T) {
	s := &scripted{replies: []string{textAndTool("", "call_1", "find_member", `{}`)}}
	c := newTestClient(t, s, false)
	tools := Tools{Run: func(context.Context, string, json.RawMessage) (string, string, error) {
		return "", "", errors.New("database is locked")
	}}
	res, err := c.Answer(context.Background(), "S", userMessages(t, "Q"), tools, Events{})
	require.Error(t, err)
	assert.Equal(t, "internal", Code(err))
	assert.Nil(t, res.History, "the caller keeps its history")
	assert.Equal(t, 1, res.Calls)

	s.fail(http.StatusInternalServerError)
	_, err = c.Answer(context.Background(), "S", userMessages(t, "Q"), (&runner{}).tools(), Events{})
	require.ErrorIs(t, err, ErrHTTP)
}

func TestAnswerReportsAToolCutByTheContext(t *testing.T) {
	s := &scripted{replies: []string{textAndTool("", "call_1", "find_member", `{}`)}}
	tools := Tools{Run: func(ctx context.Context, _ string, _ json.RawMessage) (string, string, error) {
		<-ctx.Done()
		return "", "", ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	res, err := newTestClient(t, s, false).Answer(ctx, "S", userMessages(t, "Q"), tools, Events{})
	require.ErrorIs(t, err, ErrCanceled)
	assert.Equal(t, "canceled", Code(err))
	assert.Nil(t, res.History)
}
