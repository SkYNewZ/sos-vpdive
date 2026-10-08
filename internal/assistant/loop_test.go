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
	turns := 0
	res, err := c.Answer(context.Background(), "S", userMessages(t, "Q"), r.tools(), Events{
		Text: func(d string) { deltas = append(deltas, d) },
		Step: func(l string) { steps = append(steps, l) },
		Turn: func() { turns++ },
	})
	require.NoError(t, err)
	assert.Equal(t, "Léa est introuvable.", res.Text, "the answer is the last turn: what came before the tools is narration")
	assert.Equal(t, []string{"Je cherche.", "Léa est introuvable."}, deltas, "every turn streams")
	assert.Equal(t, 1, turns, "the second call starts a new turn")
	assert.Equal(t, []string{`find_member {"query":"Léa"}`}, r.calls)
	assert.Equal(t, []string{"Recherche faite"}, steps)
	assert.Equal(t, 2, res.Calls)
	assert.Equal(t, map[string]int{"find_member": 1}, res.Tools)
	assert.Positive(t, res.FirstText)
	require.Len(t, res.History, 4, "question, tool call, tool result, answer")
	assert.Contains(t, string(res.History[1].Content[0]), "Je cherche.", "the model keeps its own narration")

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

// Codex review: a last turn without text, or with a call left without its
// result, would be stored and refused by the provider on every later
// question: the answer fails and the conversation rolls back.
func TestAnswerRefusesATurnTheConversationCannotKeep(t *testing.T) {
	bar := string(rune(0xff5c)) // DeepSeek writes its tags with fullwidth bars
	cases := map[string]string{
		"no text":            textStream(),
		"thinking only, cut": strings.Replace(toolStream(true), `"stop_reason":"tool_use"`, `"stop_reason":"max_tokens"`, 1),
		"call without tool_use stop": strings.Replace(toolStream(false, [3]string{"a", "find_member", `{"query":"Léa"}`}),
			`"stop_reason":"tool_use"`, `"stop_reason":"end_turn"`, 1),
		"DSML call written as text": textStream("Je consulte ses sorties.\n<"+bar+bar+"DSML"+bar+bar+" calls>\n<"+bar+bar+"DSML"+bar+bar,
			` invoke name="member_outings">`),
		"function call written as text": textStream(`<tool_call>{"name":"find_member"}</tool_call>`),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			s := &scripted{replies: []string{reply}}
			r := &runner{}
			res, err := newTestClient(t, s, false).Answer(context.Background(), "S", userMessages(t, "Q"), r.tools(), Events{})
			require.ErrorIs(t, err, ErrInvalid)
			assert.Nil(t, res.History, "nothing is stored")
			assert.Empty(t, res.Text)
			assert.Empty(t, r.calls, "a call written as text never runs")
		})
	}
}

// An answer cut at max_tokens keeps what it wrote, with a note that says so.
func TestAnswerKeepsACutAnswer(t *testing.T) {
	s := &scripted{replies: []string{strings.Replace(textStream("Le solde est ", "de -48,00 €"), "end_turn", "max_tokens", 1)}}
	res, err := newTestClient(t, s, false).Answer(context.Background(), "S", userMessages(t, "Q"), (&runner{}).tools(), Events{})
	require.NoError(t, err)
	assert.Equal(t, "Le solde est de -48,00 €\n\n_Réponse coupée : limite de longueur atteinte._", res.Text)
	require.Len(t, res.History, 2)
	assert.NotContains(t, string(res.History[1].Content[0]), "coupée", "the model's turn goes back as it wrote it")
}

// Re-review: an answer cut inside its draft closes the draft before the
// note, so that « Copier » does not copy the note with the draft.
func TestAnswerClosesADraftCutShort(t *testing.T) {
	cut := "### Brouillon\n\n```brouillon\nBonjour Léa,\nton carnet"
	s := &scripted{replies: []string{strings.Replace(textStream(cut), "end_turn", "max_tokens", 1)}}
	res, err := newTestClient(t, s, false).Answer(context.Background(), "S", userMessages(t, "Q"), (&runner{}).tools(), Events{})
	require.NoError(t, err)
	assert.Equal(t, cut+"\n```"+cutNote, res.Text)
	html := string(Render(res.Text))
	draft := html[strings.Index(html, "<pre>"):strings.Index(html, "</pre>")]
	assert.Contains(t, draft, "ton carnet")
	assert.NotContains(t, draft, "coupée")
	assert.Contains(t, html, "<em>Réponse coupée")
}

func TestCloseFence(t *testing.T) {
	for in, want := range map[string]string{
		"Le solde est de":                    "Le solde est de",
		"```brouillon\nBonjour\n```\n\nPuis": "```brouillon\nBonjour\n```\n\nPuis",
		"~~~\ncode\n":                        "~~~\ncode\n~~~",
		"````brouillon\n```\nBonjour":        "````brouillon\n```\nBonjour\n````",
	} {
		assert.Equal(t, want, closeFence(in), in)
	}
}

func TestAnswerKeepsTextThatOnlyLooksLikeMarkup(t *testing.T) {
	for _, text := range []string{"Le tableau | a | b | et <b>gras</b>.", "Utilise l'outil find_member.", "Invoque-le : name=Léa"} {
		s := &scripted{replies: []string{textStream(text)}}
		res, err := newTestClient(t, s, false).Answer(context.Background(), "S", userMessages(t, "Q"), (&runner{}).tools(), Events{})
		require.NoError(t, err, text)
		assert.Equal(t, text, res.Text)
	}
}
