package web

import (
	"context"
	"net/http"
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
	assert.Contains(t, page, "<strong>membre</strong>")
	assert.Equal(t, 1, e.count(t, "assistant_usage"))
}

func TestAssistantFollowUpKeepsTheConversation(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "Première.")}}
	e, cookie := assistantEnv(t, stub, 50)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q1"}})
	id := ev.of("start")[0]["conversation"].(string)
	stub.script(sseText(t, "Seconde."))
	_, ev, _ = e.ask(t, cookie, url.Values{"text": {"Q2"}, "conversation": {id}})
	assert.Equal(t, id, ev.of("start")[0]["conversation"])
	second := stub.calls()[1]
	assert.Contains(t, second, "Première.", "the history goes back")
	assert.Equal(t, 1, strings.Count(second, "Nous sommes le"), "the context comes once")
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
}

func TestAssistantQuota(t *testing.T) {
	stub := &streamStub{replies: []string{sseText(t, "R")}}
	e, cookie := assistantEnv(t, stub, 1)
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {"Q"}})
	require.Len(t, ev.of("done"), 1)
	_, ev, _ = e.ask(t, cookie, url.Values{"text": {"Q"}})
	require.Len(t, ev.of("error"), 1)
	assert.Contains(t, ev.of("error")[0]["message"], "1 question du jour")
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
	assert.Contains(t, e.openTicket(t, cookie, hugo.ID).body, "Analyser")
	_, ev, _ := e.ask(t, cookie, url.Values{"text": {""}, "demande": {itoa(hugo.ID)}})
	require.Len(t, ev.of("done"), 1)
	assert.Nil(t, ev.of("start")[0]["url"], "the request page keeps its address")
	first := stub.calls()[0]
	assert.Contains(t, first, `\u003cdemande\u003e\n{\"reference\":`, "the frame, its brackets escaped by JSON")
	assert.Contains(t, first, "m1, identifié par l'adresse de la demande")
	page := e.openTicket(t, cookie, hugo.ID).body
	assert.Contains(t, page, "Voir l'analyse")
	assert.Contains(t, page, "<h3>Adhérent</h3>")

	status, _, _ := e.ask(t, cookie, url.Values{"text": {""}, "demande": {itoa(hugo.ID)}})
	assert.Equal(t, http.StatusUnprocessableEntity, status, "the analysis runs once; then questions")
	status, _, _ = e.ask(t, cookie, url.Values{"text": {""}})
	assert.Equal(t, http.StatusUnprocessableEntity, status)
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
		steps: []string{"Sorties du 01/09/2026 cherchées : 1 sortie", "Sortie « Sortie Cap Garonne » lue"},
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
