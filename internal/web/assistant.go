package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/suggest"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

const (
	maxQuestion   = 8000                   // characters of a question
	answerEvery   = 150 * time.Millisecond // between two renders of a streamed answer
	streamSlack   = 30 * time.Second       // write deadline beyond the answer's own limit
	assistantPath = "/assistant"
)

// starter fills the composer of an empty page; an Open one ends on a word
// the resolver completes.
type starter struct {
	Text string
	Open bool
}

var assistantStarters = []starter{
	{Text: "Qui n'a pas encore réglé la sortie de samedi ?"},
	{Text: "Quelles sorties annulées restent à supprimer dans VPDive ?"},
	{Text: "Où en est le carnet de", Open: true},
}

// exchangeView is an exchange as a page shows it.
type exchangeView struct {
	Question string
	Steps    []string
	Answer   template.HTML
	Sources  []assistant.Source
}

// assistantPanel is the thread and the composer, on /assistant and on a request.
type assistantPanel struct {
	CSRF         string
	Conversation string
	TicketID     int64
	Exchanges    []exchangeView
	Remaining    string
	Empty        exchangeView // the shape app.js clones for a new exchange
}

// dossierView is the Dossier column of /assistant: what the server knows,
// never what the model wrote.
type dossierView struct {
	People    []assistant.Person
	Sources   []assistant.Source
	Summary   string // the folded bar on a phone
	Remaining string
	Expires   string
}

type assistantPageData struct {
	Panel    assistantPanel
	Dossier  dossierView
	Starters []starter
	Gone     bool // the conversation asked for is erased
	Owner    bool // shows the link to the usage journal
}

func views(exs []assistant.Exchange) []exchangeView {
	out := make([]exchangeView, len(exs))
	for i, ex := range exs {
		out[i] = exchangeView{Question: ex.Question, Steps: ex.Steps, Answer: assistant.Render(ex.Answer), Sources: ex.Sources}
	}
	return out
}

// quotaKey counts an account's questions of the Paris day.
func (s *Server) quotaKey(account string) string {
	return "assistant:" + account + ":" + parisDay(s.now(), s.paris)
}

// remaining says how many questions account has left today.
func (s *Server) remaining(ctx context.Context, account string) (string, error) {
	_, used, err := s.limiter.counter(ctx, s.limiter.hashedKey(s.quotaKey(account)))
	if err != nil {
		return "", err
	}
	left := s.cfg.Assistant.DailyQuestions - used
	if left <= 0 {
		return "Plus de question aujourd'hui : le quota repart à minuit.", nil
	}
	return "Il te reste " + plural(left, "question", "questions") + " aujourd'hui.", nil
}

// dossier gathers what c's answers read, current's included (an answer
// still running), with the people found.
func (s *Server) dossier(c assistant.Conversation, current []assistant.Source, remaining string) dossierView {
	d := dossierView{People: c.People, Remaining: remaining}
	for _, ex := range c.Exchanges {
		for _, src := range ex.Sources {
			d.Sources = appendSource(d.Sources, src)
		}
	}
	for _, src := range current {
		d.Sources = appendSource(d.Sources, src)
	}
	parts := []string{"Dossier"}
	if n := len(d.People); n > 0 {
		parts = append(parts, plural(n, "adhérent", "adhérents"))
	}
	for _, src := range d.Sources {
		if src.Stale {
			parts = append(parts, strings.ToLower(src.Label)+" : import périmé")
		}
	}
	d.Summary = strings.Join(parts, " · ")
	if !c.Created.IsZero() {
		d.Expires = "S'efface à " + c.Expires().In(s.paris).Format("15:04") + " sans nouvelle question."
	}
	return d
}

// assistantPage is an empty /assistant.
func (s *Server) assistantPage(w http.ResponseWriter, r *http.Request) {
	s.renderAssistant(w, r, http.StatusOK, assistant.Conversation{}, false)
}

// assistantConversation is /assistant/{id}: a conversation still in memory.
func (s *Server) assistantConversation(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	c, ok := s.convs.Find(string(sess.hash), r.PathValue("id"))
	if !ok {
		s.renderAssistant(w, r, http.StatusNotFound, assistant.Conversation{}, true)
		return
	}
	s.renderAssistant(w, r, http.StatusOK, c, false)
}

func (s *Server) renderAssistant(w http.ResponseWriter, r *http.Request, status int, c assistant.Conversation, gone bool) {
	sess, _ := sessionFrom(r.Context())
	left, err := s.remaining(r.Context(), sess.account.Username)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p, err := s.adminPage(r, "Assistant")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = assistantPageData{
		Panel:    assistantPanel{CSRF: p.CSRF, Conversation: c.ID, Exchanges: views(c.Exchanges), Remaining: left},
		Dossier:  s.dossier(c, nil, left),
		Starters: assistantStarters, Gone: gone, Owner: s.isOwner(sess.account.Username),
	}
	s.render(w, r, status, "assistant", p)
}

// ticketAssistant is the panel of a request page; nil when the assistant is off.
func (s *Server) ticketAssistant(r *http.Request, ticketID int64, csrf string) (*assistantPanel, error) {
	if s.convs == nil {
		return nil, nil //nolint:nilnil // no panel: the assistant is off
	}
	sess, _ := sessionFrom(r.Context())
	left, err := s.remaining(r.Context(), sess.account.Username)
	if err != nil {
		return nil, err
	}
	p := &assistantPanel{CSRF: csrf, TicketID: ticketID, Remaining: left}
	if c, ok := s.convs.ForTicket(string(sess.hash), ticketID); ok {
		p.Conversation, p.Exchanges = c.ID, views(c.Exchanges)
	}
	return p, nil
}

// forgetConversations erases every conversation and stops the answers in
// flight, after an erasure or a deletion: one of them may hold what went.
func (s *Server) forgetConversations() {
	if s.convs != nil {
		s.convs.DropAll()
	}
}

// SweepConversations erases the expired conversations every minute until
// ctx ends, when the assistant is on (a background job of serve).
func (s *Server) SweepConversations(ctx context.Context) {
	if s.convs != nil {
		s.convs.Run(ctx, time.Minute)
	}
}

// streamEvent is one line of the answer stream.
type streamEvent struct {
	Type         string        `json:"type"`
	Conversation string        `json:"conversation,omitempty"`
	URL          string        `json:"url,omitempty"`
	Label        string        `json:"label,omitempty"`
	Question     string        `json:"question,omitempty"` // start: the question « Analyser » stands for
	Summary      string        `json:"summary,omitempty"`  // dossier: the folded bar on a phone
	HTML         template.HTML `json:"html,omitempty"`
	Sources      template.HTML `json:"sources,omitempty"`
	Remaining    string        `json:"remaining,omitempty"`
	Message      string        `json:"message,omitempty"`
}

// ndjson writes one event per line and flushes it at once. The first write
// error (the resolver left) silences the rest.
type ndjson struct {
	enc *json.Encoder
	rc  *http.ResponseController
	err error
}

func (n *ndjson) send(ev streamEvent) {
	if n.err == nil {
		n.err = n.enc.Encode(ev)
	}
	if n.err == nil {
		n.err = n.rc.Flush()
	}
}

func (s *Server) openStream(w http.ResponseWriter, r *http.Request) *ndjson {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(assistant.AnswerTimeout + streamSlack)); err != nil {
		s.logger.DebugContext(r.Context(), "assistant stream write deadline", "error", err)
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // reverse proxies must not buffer the stream
	w.WriteHeader(http.StatusOK)
	return &ndjson{enc: json.NewEncoder(w), rc: rc}
}

// sendDossier streams the Dossier column of d and the summary of its phone
// bar. A render failure is logged; the column keeps its last state.
func (s *Server) sendDossier(ctx context.Context, out *ndjson, d dossierView) {
	html, err := s.fragment("assistantDossier", d)
	if err != nil {
		s.logger.ErrorContext(ctx, "render assistant dossier", "error", err)
		return
	}
	out.send(streamEvent{Type: "dossier", HTML: html, Summary: d.Summary})
}

// questionOf is the question an exchange shows: the resolver's text, or the
// request « Analyser » stands for when there is none.
func questionOf(text string, t *tickets.Detail) string {
	if text == "" && t != nil {
		return "Analyse de la demande " + t.Ref
	}
	return text
}

// fragment renders a partial of the assistant page.
func (s *Server) fragment(name string, data any) (template.HTML, error) {
	var buf bytes.Buffer
	if err := s.pages["assistant"].ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // html/template output
}

// failureText tells the resolver why an answer stopped.
func failureText(code string, limit int) string {
	switch code {
	case outcomeLimit:
		return "Quota atteint : " + plural(limit, "question", "questions") + " par jour. Il repart à minuit."
	case outcomeTimeout:
		return "Le modèle a mis trop de temps à répondre. Réessaie, ou pose une question plus précise."
	case outcomeCanceled:
		return "Réponse arrêtée."
	case outcomeHTTP:
		return "Le fournisseur du modèle n'a pas répondu correctement. Réessaie dans un instant."
	case outcomeInvalid:
		return "La réponse du modèle est illisible ou trop longue. Réessaie."
	}
	return "Erreur interne. Réessaie dans un instant."
}

// assistantAsk answers one question as an NDJSON stream (POST /assistant/messages).
func (s *Server) assistantAsk(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	ctx := r.Context()
	sess, _ := sessionFrom(ctx)
	text := strings.TrimSpace(r.PostForm.Get("text"))
	if utf8.RuneCountInString(text) > maxQuestion {
		s.writeText(w, r, http.StatusUnprocessableEntity, "Question trop longue : 8 000 caractères au plus.\n")
		return
	}
	ticketID := max(formInt(r.PostForm, "demande"), 0)
	if text == "" && ticketID == 0 {
		s.writeText(w, r, http.StatusUnprocessableEntity, "Écris une question.\n")
		return
	}
	// stop lets an erasure (Store.DropAll) or the end of the session
	// (Store.Drop, with assistant.ErrSessionEnded) end this answer.
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	c, err := s.convs.Begin(string(sess.hash), sess.account.Username, r.PostForm.Get("conversation"), ticketID, stop)
	// On a 404 or a 410, app.js forgets the conversation: the next question
	// starts a new one, with the request on a request page.
	again := " Ta prochaine question en ouvrira une nouvelle.\n"
	if ticketID > 0 {
		again = " Ta prochaine question relancera l'analyse de la demande.\n"
	}
	switch {
	case errors.Is(err, assistant.ErrNotFound):
		s.writeText(w, r, http.StatusNotFound, "Conversation effacée : elle disparaît 30 minutes après la dernière question."+again)
		return
	case errors.Is(err, assistant.ErrBusy):
		s.writeText(w, r, http.StatusConflict, "Une réponse est déjà en cours.\n")
		return
	case errors.Is(err, assistant.ErrFull):
		s.writeText(w, r, http.StatusGone, "Conversation trop longue."+again)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	// Frees the slot whatever happens, a recovered panic included; after
	// Finish it frees nothing, not even a newer answer's slot.
	defer s.convs.Abort(c)
	// The session may have ended between signedIn and Begin: its row goes
	// before endSessions, which then found no answer to stop. Drop what Begin
	// registered, as endSessions would have.
	if _, ok := s.sessionOf(r); !ok {
		s.convs.Drop(string(sess.hash))
		s.writeText(w, r, http.StatusForbidden, "Session expirée : reconnecte-toi.\n")
		return
	}
	var ticket *tickets.Detail
	switch {
	case len(c.Exchanges) > 0 && text == "":
		s.writeText(w, r, http.StatusUnprocessableEntity, "Écris une question.\n")
		return
	case len(c.Exchanges) > 0: // the request went with the first question
	case ticketID > 0:
		// Read once the answer is registered: an erasure or a deletion from
		// now on stops it, so no copy read before reaches the model. Stopped
		// meanwhile, the answer is journaled canceled by streamAnswer.
		ticket, err = s.tickets.Detail(ctx, ticketID)
		if errors.Is(err, tickets.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		if err != nil && ctx.Err() == nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.streamAnswer(ctx, s.openStream(w, r), c, text, ticket, sess.account.Username)
}

// streamAnswer runs the answer and streams it. The journal gets its row
// whatever happens.
func (s *Server) streamAnswer(ctx context.Context, out *ndjson, c assistant.Conversation, text string, ticket *tickets.Detail, account string) {
	ctx, span := s.tracer.Start(ctx, "assistant.answer")
	defer span.End()
	start := time.Now()
	standalone := c.TicketID == 0
	entry := usageEntry{Account: account, Origin: "page", Model: s.assistant.Model, Thinking: s.assistant.Thinking}
	if !standalone {
		entry.Origin = "demande"
	}
	defer func() {
		entry.Duration = time.Since(start)
		s.recordUsage(ctx, entry)
		tools := 0
		for _, n := range entry.Result.Tools {
			tools += n
		}
		span.SetAttributes(attribute.String("assistant.outcome", entry.Outcome), attribute.String("assistant.model", entry.Model),
			attribute.Bool("assistant.thinking", entry.Thinking), attribute.Int("assistant.calls", entry.Result.Calls),
			attribute.Int("assistant.tools", tools),
			attribute.Int("assistant.input_tokens", entry.Result.Usage.Input), attribute.Int("assistant.output_tokens", entry.Result.Usage.Output))
		if entry.Outcome != outcomeOK {
			telemetry.Fail(span, entry.Outcome)
		}
	}()
	var left string // the questions left today, this one counted
	// clearAnswer takes back what streamed: a tool call written as text is no answer.
	clearAnswer := func() { out.send(streamEvent{Type: "answer"}) }
	fail := func(code string) {
		entry.Outcome = code
		if code == outcomeInvalid {
			clearAnswer()
		}
		msg := failureText(code, s.cfg.Assistant.DailyQuestions)
		if code == outcomeCanceled && errors.Is(context.Cause(ctx), assistant.ErrSessionEnded) {
			msg = "Session terminée : reconnecte-toi." // logged out in another tab, or revoked
		}
		out.send(streamEvent{Type: "error", Message: msg, Remaining: left})
	}
	allowed, err := s.limiter.allow(ctx, s.quotaKey(account), s.cfg.Assistant.DailyQuestions, counterRetention)
	if err != nil {
		fail(s.quotaFailed(ctx, err))
		return
	}
	// A stopped answer sends nothing more: the count goes out first, so that
	// the page can show it whatever happens next.
	if left, err = s.remaining(ctx, account); err != nil {
		s.quotaFailed(ctx, err)
	}
	if !allowed {
		s.logger.InfoContext(ctx, "assistant quota reached")
		fail(outcomeLimit)
		return
	}
	first := streamEvent{Type: "start", Conversation: c.ID, Remaining: left}
	if text == "" {
		first.Question = questionOf(text, ticket)
	}
	if standalone {
		first.URL = assistantPath + "/" + c.ID
	}
	out.send(first)

	events := func(tb *toolbox) assistant.Events {
		var (
			answer   strings.Builder
			rendered time.Time
		)
		return assistant.Events{
			Text: func(d string) {
				answer.WriteString(d)
				if time.Since(rendered) >= answerEvery {
					rendered = time.Now()
					out.send(streamEvent{Type: "answer", HTML: assistant.Render(answer.String())})
				}
			},
			Thinking: func() { out.send(streamEvent{Type: "thinking"}) },
			// The text so far was narration: the new turn's first words replace it.
			Turn: func() {
				answer.Reset()
				rendered = time.Time{}
			},
			Retry: clearAnswer,
			Step: func(label string) {
				out.send(streamEvent{Type: "step", Label: label})
				if standalone {
					s.sendDossier(ctx, out, s.dossier(*tb.c, tb.sources, left))
				}
			},
		}
	}
	ex, res, err := s.runAnswer(ctx, &c, text, ticket, events)
	entry.Result = res
	if err != nil {
		// Only the stable code is logged: a model error may quote what the
		// provider sent.
		code := assistant.Code(err)
		if ctx.Err() != nil {
			code = outcomeCanceled
		}
		if code == outcomeInternal {
			s.logger.ErrorContext(ctx, "assistant answer", "error", err)
		} else {
			s.logger.WarnContext(ctx, "assistant answer failed", "code", code)
		}
		fail(code)
		return
	}
	c.Exchanges = append(c.Exchanges, ex)
	c = s.convs.Finish(c) // the Dossier shows the stored expiry
	entry.Outcome = outcomeOK
	sources, err := s.fragment("assistantSources", ex.Sources)
	if err != nil {
		s.logger.ErrorContext(ctx, "render assistant sources", "error", err)
	}
	out.send(streamEvent{Type: "done", HTML: assistant.Render(ex.Answer), Sources: sources, Remaining: left})
	if standalone {
		s.sendDossier(ctx, out, s.dossier(c, nil, left))
	}
}

// quotaFailed logs a quota read that failed and returns the outcome: an
// error of ours, unless the resolver left or an erasure stopped the answer.
func (s *Server) quotaFailed(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		s.logger.WarnContext(ctx, "assistant quota check stopped", "code", outcomeCanceled)
		return outcomeCanceled
	}
	s.logger.ErrorContext(ctx, "assistant quota", "error", err)
	return outcomeInternal
}

// runAnswer asks the model text (and the request t, on « Analyser »), with
// the tools bound to c, and returns the exchange to keep. On success c's
// history holds the answer; on error the caller drops c (Abort). Each call
// gets a fresh toolbox: the cap on people read is per answer.
func (s *Server) runAnswer(ctx context.Context, c *assistant.Conversation, text string, t *tickets.Detail,
	events func(tb *toolbox) assistant.Events) (assistant.Exchange, assistant.Result, error) {
	q, err := s.questionText(ctx, c, text, t)
	if err != nil {
		return assistant.Exchange{}, assistant.Result{}, err
	}
	msg, err := assistant.UserText(q)
	if err != nil {
		return assistant.Exchange{}, assistant.Result{}, err
	}
	tb := &toolbox{s: s, c: c}
	ev := events(tb)
	ex := assistant.Exchange{Question: questionOf(text, t)}
	step := ev.Step
	ev.Step = func(label string) {
		ex.Steps = append(ex.Steps, label)
		if step != nil {
			step(label)
		}
	}
	res, err := s.assistant.Answer(ctx, s.assistantPrompt, append(slices.Clone(c.History), msg), tb.tools(), ev)
	if err != nil {
		return assistant.Exchange{}, res, err
	}
	c.History = res.History
	ex.Answer, ex.Sources = res.Text, tb.sources
	return ex, res, nil
}

// BenchResult is one answer of cmd assistant-bench.
type BenchResult struct {
	Steps    []string
	Answer   string
	Result   assistant.Result
	Duration time.Duration
	Outcome  string
}

// BenchAnswer answers text as account on a fresh conversation, as /assistant
// does, without quota, stream or journal (cmd assistant-bench).
func (s *Server) BenchAnswer(ctx context.Context, account, text string) (BenchResult, error) {
	if s.assistant == nil {
		return BenchResult{}, errors.New("the assistant is off")
	}
	start := time.Now()
	c := assistant.Conversation{Account: account}
	ex, res, err := s.runAnswer(ctx, &c, text, nil, func(*toolbox) assistant.Events { return assistant.Events{} })
	out := BenchResult{Steps: ex.Steps, Answer: ex.Answer, Result: res, Duration: time.Since(start), Outcome: outcomeOK}
	if err != nil {
		out.Outcome = assistant.Code(err)
	}
	return out, nil
}

// SuggestResult is one call of cmd suggest-bench.
type SuggestResult struct {
	suggest.Result

	Duration time.Duration
	Outcome  string
}

// BenchSuggest asks for the suggestions of sub as the member form does,
// without the daily cap (cmd suggest-bench).
func (s *Server) BenchSuggest(ctx context.Context, sub tickets.Submission) (SuggestResult, error) {
	if s.suggest == nil {
		return SuggestResult{}, errors.New("suggestions are off")
	}
	start := time.Now()
	res, err := s.suggest.Choose(ctx, s.suggestRequest(sub), s.fiches)
	out := SuggestResult{Result: res, Duration: time.Since(start), Outcome: outcomeOK}
	if err != nil {
		out.Outcome = suggest.Code(err)
	}
	return out, nil
}
