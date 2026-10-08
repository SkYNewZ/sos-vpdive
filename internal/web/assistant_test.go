package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

func assistantEnv(t *testing.T, stub *streamStub, limit int) (*testEnv, *http.Cookie) {
	t.Helper()
	e := newTestEnv(t, withAssistant(t, stub, limit))
	e.importMembers(t, "members_valid.xlsx")
	return e, e.login(t)
}

func (m *streamStub) script(replies ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status, m.replies = 0, replies
}

// sessionOf is the store's key of cookie's session.
func sessionOf(cookie *http.Cookie) string { return string(secure.TokenHash(cookie.Value)) }

func TestAssistantOffByDefault(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	cookie := e.login(t)
	for _, path := range []string{"/assistant", "/assistant/journal", "/assistant/abc"} {
		status, _ := e.page(t, cookie, path)
		assert.Equal(t, http.StatusNotFound, status, path)
	}
	rec := e.postAs(t, cookie, "/assistant/messages", url.Values{"text": {"Q"}})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	_, board := e.page(t, cookie, "/")
	assert.NotContains(t, board, `href="/assistant"`)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	assert.NotContains(t, e.openTicket(t, cookie, hugo.ID).body, "data-assistant")
}

func TestAssistantRoutesAreGuarded(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 50)
	rec := e.do(t, http.MethodGet, adminHost, "/assistant", nil)
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/connexion", rec.Header().Get("Location"))
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/assistant", nil).Code, "committee site only")

	q := url.Values{"text": {"Q"}}
	post := func(mutators ...func(*http.Request)) int {
		return e.do(t, http.MethodPost, adminHost, "/assistant/messages", formBody(q), append([]func(*http.Request){formType}, mutators...)...).Code
	}
	assert.Equal(t, http.StatusForbidden, post(), "signed out")
	assert.Equal(t, http.StatusForbidden, post(withCookie(cookie)), "no csrf")
	q.Set("csrf", e.csrf(t, cookie, "/assistant"))
	evil := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }
	assert.Equal(t, http.StatusForbidden, post(withCookie(cookie), evil), "another origin")
	assert.Empty(t, stub.calls(), "a refused question reaches no model")
}

func TestAssistantAnswersWithTools(t *testing.T) {
	stub := &streamStub{replies: []string{
		sseTool(t, "find_member", `{"query":"Hugo Bernard"}`),
		sseText(t, "### Adhérent\n\n", "Hugo Bernard est **membre**."),
	}}
	e, cookie := assistantEnv(t, stub, 50)
	status, ev, _ := e.ask(t, cookie, url.Values{"text": {"Hugo Bernard <hugo.bernard@example.org>, 06 12 34 56 78, me dit que son carnet est faux."}})
	require.Equal(t, http.StatusOK, status)

	start := ev.of("start")
	require.Len(t, start, 1)
	id := start[0]["conversation"].(string)
	assert.Equal(t, "/assistant/"+id, start[0]["url"])
	assert.Equal(t, "Recherche « Hugo Bernard » : 1 candidat", ev.of("step")[0]["label"])
	done := ev.of("done")
	require.Len(t, done, 1)
	assert.Contains(t, done[0]["html"], "<h3>Adhérent</h3>")
	assert.Contains(t, done[0]["html"], "<strong>membre</strong>")
	assert.Contains(t, done[0]["sources"], "Liste des membres")
	assert.Equal(t, "Il te reste 49 questions aujourd'hui.", done[0]["remaining"])
	dossiers := ev.of("dossier")
	require.NotEmpty(t, dossiers)
	assert.Contains(t, dossiers[len(dossiers)-1]["html"], "hugo.bernard@example.org", "the resolver sees the address")
	assert.Equal(t, "Dossier · 1 adhérent", dossiers[len(dossiers)-1]["summary"], "the phone bar follows the column")
	assert.Equal(t, "Il te reste 49 questions aujourd'hui.", start[0]["remaining"], "the question counts from the start")
	assert.Contains(t, html.UnescapeString(dossiers[0]["html"].(string)), "Il te reste 49 questions aujourd'hui.", "the column never reads blank while streaming")
	assert.Nil(t, start[0]["question"], "a typed question is not sent back")

	calls := stub.calls()
	require.Len(t, calls, 2)
	for _, body := range calls {
		assert.NotContains(t, body, "hugo.bernard@", "the model never does")
		assert.NotContains(t, body, "06 12 34 56 78")
	}
	assert.Contains(t, calls[0], "[email 1]")

	status, page := e.page(t, cookie, "/assistant/"+id)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, page, "Hugo Bernard <hugo.bernard@example.org>, 06 12 34 56 78", "the question as typed")
	assert.Contains(t, page, "data-assistant-dossier-summary>Dossier · 1 adhérent</summary>", "the phone bar, which the stream updates")
	assert.Contains(t, page, "<strong>membre</strong>")
	assert.Contains(t, page, "1 donnée consultée</summary>", "the singular for one step")
	assert.Equal(t, 1, e.count(t, "assistant_usage"))
}

func TestAssistantFollowUpKeepsTheConversation(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "Première.")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q1"}})
	id := ev.of("start")[0]["conversation"].(string)
	stub.script(sseText(t, "Seconde."))
	e.clock.advance(10 * time.Minute)
	_, ev, _ = e.ask(t, cookie, url.Values{"text": {"Q2"}, "conversation": {id}})
	assert.Equal(t, id, ev.of("start")[0]["conversation"])
	dossiers := ev.of("dossier")
	require.NotEmpty(t, dossiers)
	erased := e.clock.now().Add(30 * time.Minute).In(e.srv.paris).Format("15:04")
	assert.Contains(t, html.UnescapeString(dossiers[len(dossiers)-1]["html"].(string)), "S'efface à "+erased+" sans nouvelle question", "the expiry runs from this answer, as a reload shows")
	second := stub.calls()[1]
	assert.Contains(t, second, "Première.", "the history goes back")
	assert.Equal(t, 1, strings.Count(second, "Nous sommes le"), "the context comes once")

	_, page := e.page(t, cookie, "/assistant/"+id)
	assert.Equal(t, 2, strings.Count(page, "Aucune donnée consultée</summary>"), "answers that used no tool say so once reloaded")
	assert.Equal(t, 1, strings.Count(page, "Recherche en cours…</summary>"), "only the template for the next exchange says it is searching")
}

func TestAssistantConversationIsPrivate(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	id := ev.of("start")[0]["conversation"].(string)
	bob := e.loginBob(t)
	status, _, body := e.ask(t, bob, url.Values{"text": {"Q"}, "conversation": {id}})
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, body, "Conversation effacée")
	status, _ = e.page(t, bob, "/assistant/"+id)
	assert.Equal(t, http.StatusNotFound, status)
}

func TestAssistantConversationExpires(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	id := ev.of("start")[0]["conversation"].(string)
	e.clock.advance(31 * time.Minute)
	status, _, body := e.ask(t, cookie, url.Values{"text": {"Q"}, "conversation": {id}})
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, body, "Conversation effacée")
	assert.Contains(t, body, "Ta prochaine question en ouvrira une nouvelle.", "app.js forgets the conversation on a 404")
}

// A request's analysis that was erased or filled up does not trap the
// panel: app.js forgets it on a 404 or a 410, and the next question, or
// « Nouvelle analyse », starts the analysis again with the request.
func TestAssistantRequestAnalysisStartsAgain(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "Analyse.")}}
	e, cookie := assistantEnv(t, stub, 50)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	assert.Contains(t, e.openTicket(t, cookie, hugo.ID).body, "data-assistant-restart", "the panel offers a new analysis")
	demande := itoa(hugo.ID)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {""}, "demande": {demande}})
	first := ev.of("start")[0]["conversation"].(string)

	c, err := e.srv.convs.Begin(sessionOf(cookie), "alice", first, 0, nil)
	require.NoError(t, err)
	c.Exchanges = make([]assistant.Exchange, assistant.MaxQuestions)
	e.srv.convs.Finish(c)
	status, _, body := e.ask(t, cookie, url.Values{"text": {"Et ensuite ?"}, "demande": {demande}, "conversation": {first}})
	assert.Equal(t, http.StatusGone, status)
	assert.Contains(t, body, "Conversation trop longue.")
	assert.Contains(t, body, "Ta prochaine question relancera l'analyse de la demande.")

	_, ev, _ = e.ask(t, cookie, url.Values{"text": {"Et ensuite ?"}, "demande": {demande}})
	require.Len(t, ev.of("done"), 1)
	second := ev.of("start")[0]["conversation"].(string)
	assert.NotEqual(t, first, second, "a full analysis is never resumed")
	calls := stub.calls()
	assert.Contains(t, calls[len(calls)-1], `\u003cdemande\u003e`, "the new analysis carries the request")
	assert.Contains(t, calls[len(calls)-1], "Et ensuite ?")
	assert.Contains(t, e.openTicket(t, cookie, hugo.ID).body, `data-conversation="`+second+`"`, "the page shows the newest analysis")

	e.clock.advance(31 * time.Minute)
	status, _, body = e.ask(t, cookie, url.Values{"text": {"Et ensuite ?"}, "demande": {demande}, "conversation": {second}})
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, body, "Ta prochaine question relancera l'analyse de la demande.")

	_, ev, _ = e.ask(t, cookie, url.Values{"text": {""}, "demande": {demande}})
	require.Len(t, ev.of("done"), 1, "« Nouvelle analyse »")
	assert.Equal(t, "Analyse de la demande "+hugo.Ref, ev.of("start")[0]["question"])
}

// What the model wrote before its tools is narration: it streams while the
// tools run, then the next turn replaces it, and only the last turn stays.
func TestAssistantShowsOnlyTheLastTurn(t *testing.T) {
	narration := sseEvents(sseStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Je cherche Hugo."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"find_member","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"Hugo Bernard\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":10}}`, `{"type":"message_stop"}`)
	stub := &streamStub{replies: []string{narration, sseText(t, "### Adhérent\n\nHugo Bernard.")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Où en est Hugo Bernard ?"}})
	answers := ev.of("answer")
	require.Len(t, answers, 2)
	assert.Contains(t, answers[0]["html"], "Je cherche Hugo.", "the narration streams while the tools run")
	assert.NotContains(t, answers[1]["html"], "Je cherche", "the next turn replaces it at once")
	done := ev.of("done")
	require.Len(t, done, 1)
	assert.NotContains(t, done[0]["html"], "Je cherche")
	assert.Contains(t, done[0]["html"], "Hugo Bernard.")
	_, page := e.page(t, cookie, "/assistant/"+ev.of("start")[0]["conversation"].(string))
	assert.NotContains(t, page, "Je cherche Hugo.", "nor is it stored")
	assert.Contains(t, stub.calls()[1], "Je cherche Hugo.", "the model keeps its own turn")
}

// An answer cut at max_tokens is kept, with a note, and counts as an answer.
func TestAssistantKeepsACutAnswer(t *testing.T) {
	stub := &streamStub{replies: []string{strings.Replace(sseText(t, "Le solde est de -48,00 €."), "end_turn", "max_tokens", 1)}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	done := ev.of("done")
	require.Len(t, done, 1)
	assert.Contains(t, done[0]["html"], "Le solde est de -48,00 €.")
	assert.Contains(t, done[0]["html"], "<em>Réponse coupée : limite de longueur atteinte.</em>")
	var outcome string
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT outcome FROM assistant_usage`).Scan(&outcome))
	assert.Equal(t, "ok", outcome)
}

// A tool call the model wrote as text is no answer: the stream clears it.
func TestAssistantRefusesToolCallsWrittenAsText(t *testing.T) {
	bar := string(rune(0xff5c))
	stub := &streamStub{replies: []string{sseText(t, "Je regarde.\n<"+bar+bar+"DSML"+bar+bar+` invoke name="member_outings">`)}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	require.Len(t, ev.of("error"), 1)
	assert.Contains(t, ev.of("error")[0]["message"], "illisible")
	answers := ev.of("answer")
	require.NotEmpty(t, answers)
	assert.Nil(t, answers[len(answers)-1]["html"], "what streamed is cleared")
	assert.Equal(t, "error", ev[len(ev)-1]["type"])
	var outcome string
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT outcome FROM assistant_usage`).Scan(&outcome))
	assert.Equal(t, "invalid_output", outcome)
	_, page := e.page(t, cookie, "/assistant/"+ev.of("start")[0]["conversation"].(string))
	assert.NotContains(t, page, "DSML")
}

// A tool call written as text gets one more try: the markup that streamed is
// cleared at once, and the answer of the retry is what stays.
func TestAssistantRetriesAToolCallWrittenAsText(t *testing.T) {
	bar := string(rune(0xff5c))
	markup := sseText(t, "Je regarde.\n<"+bar+bar+"DSML"+bar+bar+` invoke name="member_outings">`)
	stub := &streamStub{replies: []string{markup, sseText(t, "Hugo est à jour.")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	require.Len(t, ev.of("done"), 1)
	assert.Empty(t, ev.of("error"))
	answers := ev.of("answer")
	require.Len(t, answers, 3, "the markup, cleared, then the retry's answer")
	assert.Nil(t, answers[1]["html"], "the markup is cleared before the retry")
	for _, a := range answers[2:] {
		assert.NotContains(t, a["html"], "DSML")
	}
	assert.NotContains(t, ev.of("done")[0]["html"], "DSML")
	assert.Contains(t, ev.of("done")[0]["html"], "Hugo est à jour.")
	assert.Len(t, stub.calls(), 2)
	var outcome string
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT outcome FROM assistant_usage`).Scan(&outcome))
	assert.Equal(t, "ok", outcome)
}

// A resolver leaving (or an erasure) during the quota check is no failure
// of ours: journaled canceled, logged below Error.
func TestAssistantCancelDuringTheQuotaCheck(t *testing.T) {
	stub := &streamStub{}
	e, _ := assistantEnv(t, stub, 50)
	c, err := e.srv.convs.Begin("tab", "alice", "", 0, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	e.srv.streamAnswer(ctx, &ndjson{enc: json.NewEncoder(rec), rc: http.NewResponseController(rec)}, c, "Q", nil, "alice")
	var outcome string
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT outcome FROM assistant_usage`).Scan(&outcome))
	assert.Equal(t, "canceled", outcome)
	assert.NotContains(t, e.logs.String(), `"level":"ERROR"`)
	assert.Empty(t, stub.calls())
}

func TestAssistantQuota(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 1)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	require.Len(t, ev.of("done"), 1)
	_, ev, _ = e.ask(t, cookie, url.Values{"text": {"Q"}})
	require.Len(t, ev.of("error"), 1)
	assert.Contains(t, ev.of("error")[0]["message"], "Quota atteint : 1 question par jour")
	assert.Equal(t, "Plus de question aujourd'hui : le quota repart à minuit.", ev.of("error")[0]["remaining"])
	assert.Len(t, stub.calls(), 1, "a refused question costs nothing")
	var outcome string
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT outcome FROM assistant_usage ORDER BY id DESC LIMIT 1`).Scan(&outcome))
	assert.Equal(t, "limit", outcome)
	e.clock.advance(24 * time.Hour)
	_, ev, _ = e.ask(t, cookie, url.Values{"text": {"Q"}})
	assert.Len(t, ev.of("done"), 1, "a new Paris day")
}

func TestAssistantFailureRollsBack(t *testing.T) {
	stub := &streamStub{status: http.StatusServiceUnavailable}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q1"}})
	require.Len(t, ev.of("error"), 1)
	assert.Contains(t, ev.of("error")[0]["message"], "fournisseur")
	assert.Equal(t, "Il te reste 49 questions aujourd'hui.", ev.of("error")[0]["remaining"], "a failed question counted: the page says so")
	id := ev.of("start")[0]["conversation"].(string)
	stub.script(sseText(t, "R"))
	_, ev, _ = e.ask(t, cookie, url.Values{"text": {"Q2"}, "conversation": {id}})
	require.Len(t, ev.of("done"), 1)
	calls := stub.calls()
	assert.NotContains(t, calls[len(calls)-1], "Q1", "the failed turn left no trace")
}

func TestAssistantFromARequest(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "### Adhérent\n\nHugo.")}}
	e, cookie := assistantEnv(t, stub, 50)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	before := e.openTicket(t, cookie, hugo.ID).body
	assert.Contains(t, before, "Analyser")
	assert.Contains(t, before, `data-assistant-open aria-controls="assistant-panel" aria-expanded="false"`, "closed before the first click")
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {""}, "demande": {itoa(hugo.ID)}})
	require.Len(t, ev.of("done"), 1)
	assert.Nil(t, ev.of("start")[0]["url"], "the request page keeps its address")
	question := "Analyse de la demande " + hugo.Ref
	assert.Equal(t, question, ev.of("start")[0]["question"], "the live bubble reads as the reloaded one")
	first := stub.calls()[0]
	assert.Contains(t, first, `\u003cdemande\u003e\n{\"reference\":`, "the frame, its brackets escaped by JSON")
	assert.Contains(t, first, `\"adherent\":{\"ref\":\"m1\",\"nom\":\"Hugo Bernard\"`)
	page := e.openTicket(t, cookie, hugo.ID).body
	assert.Contains(t, page, "Voir l'analyse")
	assert.Contains(t, page, "<h3>Adhérent</h3>")
	assert.Contains(t, page, question+"</p>", "the reloaded thread shows the same question")

	id := ev.of("start")[0]["conversation"].(string)
	status, _, _ := e.ask(t, cookie, url.Values{"text": {""}, "demande": {itoa(hugo.ID)}, "conversation": {id}})
	assert.Equal(t, http.StatusUnprocessableEntity, status, "the analysis runs once; then questions")
	status, _, _ = e.ask(t, cookie, url.Values{"text": {""}})
	assert.Equal(t, http.StatusUnprocessableEntity, status)
}

// Re-review: the requester's name, seasons and licence come from the
// members import, free text that may hold an address or a number: they
// reach the provider masked, as find_member's do.
func TestAssistantMasksTheRequesterAsImported(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "Analyse.")}}
	e := newTestEnv(t, withAssistant(t, stub, 50))
	ctx := context.Background()
	seasons := "2026, voir hugo.perso@example.org"
	p, err := e.deps.Members.NewPreview(ctx, "alice", &members.Export{Members: []members.Member{{
		Row: 5, LastName: "Bernard 06 12 34 56 78", FirstName: "Hugo", Email: "hugo.bernard@example.org", Seasons: &seasons,
	}}})
	require.NoError(t, err)
	require.NoError(t, e.deps.Members.Confirm(ctx, p.ID, "alice", true))
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	_, ev, _ := e.ask(t, e.login(t), url.Values{"text": {""}, "demande": {itoa(hugo.ID)}})
	require.Len(t, ev.of("done"), 1)
	body := stub.calls()[0]
	assert.Contains(t, body, `\"nom\":\"Hugo Bernard [téléphone]\"`)
	assert.Contains(t, body, `\"saisons\":\"2026, voir [email 1]\"`)
	assert.NotContains(t, body, "56 78")
	assert.NotContains(t, body, "hugo.perso@")
}

func TestAssistantLogoutErases(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	id := ev.of("start")[0]["conversation"].(string)
	_, found := e.srv.convs.Find(sessionOf(cookie), id)
	require.True(t, found)
	rec := e.postAs(t, cookie, "/deconnexion", url.Values{"csrf": {e.csrf(t, cookie, "/assistant")}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	_, found = e.srv.convs.Find(sessionOf(cookie), id)
	assert.False(t, found, "logout drops the session's conversations")
	again := e.login(t)
	status, _ := e.page(t, again, "/assistant/"+id)
	assert.Equal(t, http.StatusNotFound, status)
}

// Codex review: after an erasure, no conversation keeps the erased data,
// in the page or in the history the next question would send.
func TestAssistantErasureForgetsConversations(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Où en est Hugo Bernard ?"}})
	id := ev.of("start")[0]["conversation"].(string)
	csrf := e.csrf(t, cookie, "/effacement")
	rec := e.postAs(t, cookie, "/effacement", url.Values{"csrf": {csrf}, "email": {"hugo.bernard@example.org"}, "etape": {"confirmer"}})
	require.Equal(t, http.StatusOK, rec.Code)
	status, _ := e.page(t, cookie, "/assistant/"+id)
	assert.Equal(t, http.StatusNotFound, status)
	status, _, _ = e.ask(t, cookie, url.Values{"text": {"Et ses paiements ?"}, "conversation": {id}})
	assert.Equal(t, http.StatusNotFound, status)
}

func TestAssistantDeletionForgetsConversations(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 50)
	hugo := e.submitTicket(t, "hugo.bernard@example.org")
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {""}, "demande": {itoa(hugo.ID)}})
	id := ev.of("start")[0]["conversation"].(string)
	rec := e.act(t, cookie, hugo.ID, e.openTicket(t, cookie, hugo.ID), url.Values{"action": {"delete"}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	_, found := e.srv.convs.Find(sessionOf(cookie), id)
	assert.False(t, found, "the analysis of a deleted request goes with it")
}

func TestAssistantRefusesASecondAnswerInFlight(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, err := e.srv.convs.Begin("another-tab", "alice", "", 0, func() {})
	require.NoError(t, err)
	status, _, body := e.ask(t, cookie, url.Values{"text": {"Q"}})
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, body, "déjà en cours")
	assert.Empty(t, stub.calls())
}

func TestAssistantFreesTheSlotWhenTheResolverLeaves(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}, delay: time.Minute}
	e, cookie := assistantEnv(t, stub, 50)
	csrf := e.csrf(t, cookie, "/assistant")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	e.askUntil(ctx, cookie, url.Values{"csrf": {csrf}, "text": {"Q"}})
	var outcome string
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT outcome FROM assistant_usage`).Scan(&outcome))
	assert.Equal(t, "canceled", outcome)
	stub.mu.Lock()
	stub.delay = 0
	stub.mu.Unlock()
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	assert.Len(t, ev.of("done"), 1, "the slot is free again")
}

// answerInFlight starts a question as cookie's session on a provider that
// never answers, waits until it reaches the provider, and returns a channel
// closed when the answer ends.
func (e *testEnv) answerInFlight(t *testing.T, stub *streamStub, cookie *http.Cookie) <-chan struct{} {
	t.Helper()
	stub.mu.Lock()
	stub.delay = time.Minute
	stub.mu.Unlock()
	v := url.Values{"csrf": {e.csrf(t, cookie, "/assistant")}, "text": {"Q"}}
	before := len(stub.calls())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.askUntil(context.Background(), cookie, v)
	}()
	require.Eventually(t, func() bool { return len(stub.calls()) > before }, 5*time.Second, 5*time.Millisecond)
	return done
}

// stoppedAnswer checks that the answer ended once its session did: journaled
// canceled, with the provider's delay lifted for what follows.
func (e *testEnv) stoppedAnswer(t *testing.T, stub *streamStub, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the answer goes on after its session ended")
	}
	var outcome string
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT outcome FROM assistant_usage ORDER BY id DESC LIMIT 1`).Scan(&outcome))
	assert.Equal(t, "canceled", outcome)
	stub.mu.Lock()
	stub.delay = 0
	stub.mu.Unlock()
}

// Codex review: logout stops the session's answer in flight and frees the
// account's slot.
func TestAssistantLogoutStopsTheAnswer(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 50)
	done := e.answerInFlight(t, stub, cookie)
	rec := e.postAs(t, cookie, "/deconnexion", url.Values{"csrf": {e.csrf(t, cookie, "/assistant")}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	e.stoppedAnswer(t, stub, done)
	_, ev, _ := e.ask(t, e.login(t), url.Values{"text": {"Q"}})
	assert.Len(t, ev.of("done"), 1, "the slot is free again")
}

// Codex review: a session revoked by a password reset (or a deleted
// account) stops its answer in flight; the other sessions keep theirs.
func TestAssistantRevokedSessionStopsTheAnswer(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, alice := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, alice, url.Values{"text": {"Q"}})
	kept := ev.of("start")[0]["conversation"].(string)
	bob := e.loginBob(t)
	done := e.answerInFlight(t, stub, bob)
	_, err := e.deps.Admins.ResetPassword(context.Background(), "bob")
	require.NoError(t, err)
	e.stoppedAnswer(t, stub, done)
	_, found := e.srv.convs.Find(sessionOf(alice), kept)
	assert.True(t, found, "alice's session is untouched")
}

// Re-review: a question that passed the session check just before its
// session ended (a logout here) registers its answer after endSessions ran.
// It is refused as signed out, keeps no conversation and frees the slot.
func TestAssistantRefusesASessionEndedBeforeBegin(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 50)
	v := url.Values{"csrf": {e.csrf(t, cookie, "/assistant")}, "text": {"Q"}}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/assistant/messages", formBody(v))
	req.Host = adminHost
	formType(req)
	req.AddCookie(cookie)
	sess, ok := e.srv.sessionOf(req) // what signedIn did
	require.True(t, ok)
	e.srv.deleteSession(context.Background(), sess.hash) // then the logout
	e.srv.endSessions(string(sess.hash))
	rec := httptest.NewRecorder()
	e.srv.assistantAsk(rec, req.WithContext(context.WithValue(req.Context(), ctxSession, sess)))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "Session expirée")
	assert.Empty(t, stub.calls())
	_, found := e.srv.convs.ForTicket(sessionOf(cookie), 0)
	assert.False(t, found, "no conversation left on the ended session")
	busy, err := e.srv.convs.Begin("another-tab", "alice", "", 0, nil)
	require.NoError(t, err, "the slot is free")
	e.srv.convs.Abort(busy)
}

// Codex review: « Analyser » registers its answer before it reads the
// request, so that an erasure or a deletion from then on stops it (DropAll)
// before the copy it read reaches the model: a busy account is refused
// before the request is even looked up.
func TestAssistantRegistersTheAnalysisBeforeReadingTheRequest(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 50)
	busy, err := e.srv.convs.Begin("another-tab", "alice", "", 0, nil)
	require.NoError(t, err)
	status, _, _ := e.ask(t, cookie, url.Values{"text": {""}, "demande": {"999"}})
	assert.Equal(t, http.StatusConflict, status, "registered first")
	e.srv.convs.Abort(busy)
	status, _, _ = e.ask(t, cookie, url.Values{"text": {""}, "demande": {"999"}})
	assert.Equal(t, http.StatusNotFound, status, "then the request is read")
	assert.Empty(t, stub.calls())
}

func TestAssistantThrottlesAnswerEvents(t *testing.T) {
	deltas := make([]string, 300)
	for i := range deltas {
		deltas[i] = "mot "
	}
	stub := &streamStub{replies: []string{sseText(t, deltas...)}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	assert.LessOrEqual(t, len(ev.of("answer")), 3, "300 deltas in a few milliseconds: one or two renders, then done")
	assert.Len(t, ev.of("done"), 1)
}

func TestAssistantSpansCarryNoContent(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	stub := &streamStub{replies: []string{sseTool(t, "find_member", `{"query":"Hugo Bernard"}`), sseText(t, "Hugo.")}}
	e, cookie := assistantEnv(t, stub, 50)
	e.ask(t, cookie, url.Values{"text": {"Hugo Bernard a un souci."}})
	names := map[string]bool{}
	for _, s := range rec.Ended() {
		names[s.Name()] = true
		for _, a := range s.Attributes() {
			assert.NotContains(t, a.Value.String(), "Hugo", s.Name())
		}
	}
	for _, want := range []string{"assistant.answer", "llm.messages", "assistant.tool", "POST /assistant/messages"} {
		assert.True(t, names[want], want)
	}
	for _, s := range rec.Ended() {
		if s.Name() != "assistant.answer" {
			continue
		}
		attrs := map[string]string{}
		for _, a := range s.Attributes() {
			attrs[string(a.Key)] = a.Value.String()
		}
		assert.Equal(t, "test-model", attrs["assistant.model"])
		assert.Equal(t, "false", attrs["assistant.thinking"])
		assert.Equal(t, "1", attrs["assistant.tools"], "tool calls")
	}
}

// replayScenario is one invented exchange of TestAssistantReplay.
type replayScenario struct {
	name    string
	text    string
	follows bool     // continues the previous scenario's conversation
	replies []string // the model's turns, scripted
	steps   []string // the tools run, as the resolver sees them
	people  []string // the conversation's people by ref (m1, m2…), by address
}

// The spec's replay: invented scenarios scripted through the stub. Each runs
// the expected tools, lets no address, phone number or IBAN reach the
// provider (a line break before one included), and binds its refs to the
// expected people.
func TestAssistantReplay(t *testing.T) {
	stub := &streamStub{}
	e, cookie := assistantEnv(t, stub, 50)
	e.importPayments(t)
	e.importMollie(t)
	e.importCalendar(t, "calendar_view.json")
	martins := []string{"lea.martin2@example.org", "lea.martin@example.org"} // Search's order
	scenarios := []replayScenario{{
		name: "pasted member message",
		text: "Bonjour Alice,\nHugo Bernard m'écrit :\n« Mon carnet affiche 0 € alors que j'ai payé.\nhugo.bernard@example.org\n06 12 34 56 78\nFR76 3000 6000 0112 3456 7890 189 »",
		replies: []string{sseTool(t, "find_member", `{"query":"[email 1]"}`), sseTool(t, "member_payments", `{"ref":"m1"}`),
			sseText(t, "### Adhérent\n\nm1.")},
		steps:  []string{"Recherche « [email 1] » : 1 candidat", "Paiements de Hugo Bernard lus"},
		people: []string{"hugo.bernard@example.org"},
	}, {
		name:    "question about one person",
		text:    "Chloé Petit est-elle inscrite à des sorties ?",
		replies: []string{sseTool(t, "find_member", `{"query":"Chloé Petit"}`), sseTool(t, "member_outings", `{"ref":"m1"}`), sseText(t, "Non.")},
		steps:   []string{"Recherche « Chloé Petit » : 1 candidat", "Sorties de Chloé Petit lues"},
		people:  []string{"chloe.petit@example.org"},
	}, {
		name:    "homonyms",
		text:    "Léa Martin dit que son paiement n'apparaît pas.",
		replies: []string{sseTool(t, "find_member", `{"query":"Léa Martin"}`), sseText(t, "Deux homonymes : m1 ou m2 ?")},
		steps:   []string{"Recherche « Léa Martin » : 2 candidats"},
		people:  martins,
	}, {
		name:    "follow-up",
		text:    "Je parle de m2 (Léa Martin).",
		follows: true,
		replies: []string{sseTool(t, "member_payments", `{"ref":"m2"}`), sseText(t, "Rien à signaler.")},
		steps:   []string{"Paiements de Léa Martin lus"},
		people:  martins,
	}, {
		name: "outing question",
		text: "Qui n'a pas encore réglé la sortie Cap Garonne ?",
		replies: []string{sseTool(t, "find_outings", `{"du":"2026-09-01","au":"2026-09-30","texte":"garonne"}`),
			sseTool(t, "outing", `{"id":"evt-cap"}`), sseText(t, "Voici les inscrits.")},
		steps: []string{"Recherche des sorties du 01/09/2026 : 1 sortie", "Sortie « Sortie Cap Garonne » lue"},
	}, {
		name:    "unknown sender",
		text:    "Message de zoe.inconnue@example.org :\nJe n'arrive pas à m'inscrire.\n+33 6 98 76 54 32",
		replies: []string{sseTool(t, "find_member", `{"query":"[email 1]"}`), sseText(t, "Expéditeur inconnu : demande-lui son nom.")},
		steps:   []string{"Recherche « [email 1] » : 0 candidats"},
	}}
	var id string
	for _, sc := range scenarios {
		before := len(stub.calls())
		stub.script(sc.replies...)
		v := url.Values{"text": {sc.text}}
		if sc.follows {
			v.Set("conversation", id)
		}
		status, ev, body := e.ask(t, cookie, v)
		require.Equal(t, http.StatusOK, status, sc.name+": "+body)
		require.Len(t, ev.of("done"), 1, sc.name)
		id = ev.of("start")[0]["conversation"].(string)

		var steps []string
		for _, step := range ev.of("step") {
			steps = append(steps, step["label"].(string))
		}
		assert.Equal(t, sc.steps, steps, sc.name)
		bodies := stub.calls()[before:]
		assert.Len(t, bodies, len(sc.replies), sc.name)
		for _, b := range bodies {
			assert.NotContains(t, b, "@", sc.name)
			for _, leak := range []string{"12 34 56 78", "98 76 54 32", "FR76", "0112 3456"} {
				assert.NotContains(t, b, leak, sc.name)
			}
		}
		c, ok := e.srv.convs.Find(sessionOf(cookie), id)
		require.True(t, ok, sc.name)
		var people []string
		for i, p := range c.People {
			assert.Equal(t, "m"+itoa(int64(i+1)), p.Ref, sc.name)
			people = append(people, p.Email)
		}
		assert.Equal(t, sc.people, people, sc.name)
	}
}
