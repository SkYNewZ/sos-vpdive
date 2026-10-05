package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

type testTicket struct {
	ID    int64
	Ref   string
	Token string
}

// newSubmission is a valid request in category "carnet", as the form builds it.
func newSubmission(t *testing.T, email string) tickets.Submission {
	t.Helper()
	key, err := secure.NewToken()
	require.NoError(t, err)
	return tickets.Submission{
		FormKey: key, FirstName: "Léa", LastName: "Martin", Email: email,
		Fields:      tickets.Fields{Category: "carnet", Values: map[string]string{}},
		Description: "Mon carnet affiche un montant que je ne comprends pas.",
	}
}

func withCapture(t *testing.T) func(*tickets.Submission) {
	t.Helper()
	data := pngBytes(t)
	return func(s *tickets.Submission) {
		s.Captures = append(s.Captures, tickets.Upload{Data: data, MIME: "image/png"})
	}
}

// submitTicket files a request through the store, as the form does, and
// reads its tracking token back from the acknowledgement mail: the link
// travels by mail only.
func (e *testEnv) submitTicket(t *testing.T, email string, opts ...func(*tickets.Submission)) testTicket {
	t.Helper()
	ctx := context.Background()
	sub := newSubmission(t, email)
	for _, opt := range opts {
		opt(&sub)
	}
	ref, err := e.deps.Tickets.Submit(ctx, sub)
	require.NoError(t, err)
	tt := testTicket{Ref: ref}
	require.NoError(t, e.db.QueryRowContext(ctx, `SELECT id FROM tickets WHERE ref = ?`, ref).Scan(&tt.ID))
	for _, m := range e.mails(t) {
		if m.To != email || !strings.Contains(m.Text, ref) {
			continue
		}
		if found := trackingLinkPattern.FindStringSubmatch(m.Text); found != nil {
			tt.Token = found[1]
		}
	}
	require.NotEmpty(t, tt.Token, "no tracking link mailed for %s", ref)
	return tt
}

// apply runs a command as alice on the request as it stands now.
func (e *testEnv) apply(t *testing.T, id int64, cmd tickets.Command) {
	t.Helper()
	ctx := context.Background()
	d, err := e.deps.Tickets.Detail(ctx, id)
	require.NoError(t, err)
	cmd.TicketID, cmd.Version, cmd.Actor = id, d.Version, "alice"
	require.NoError(t, e.deps.Tickets.Apply(ctx, cmd))
}

// reply posts a member message from the tracking page.
func (e *testEnv) reply(t *testing.T, token, text string, files ...[]byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartBody(t, url.Values{"message": {text}}, files...)
	return e.do(t, http.MethodPost, publicHost, "/suivi/"+token+"/reponse", body, contentType)
}

// tracking loads a tracking page and returns its status and unescaped body.
func (e *testEnv) tracking(t *testing.T, token string) (int, string) {
	t.Helper()
	rec := e.do(t, http.MethodGet, publicHost, "/suivi/"+token, nil)
	return rec.Code, html.UnescapeString(rec.Body.String())
}

func (e *testEnv) detail(t *testing.T, id int64) *tickets.Detail {
	t.Helper()
	d, err := e.deps.Tickets.Detail(context.Background(), id)
	require.NoError(t, err)
	return d
}

func (e *testEnv) askLinks(t *testing.T, email string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, http.MethodPost, publicHost, "/retrouver", formBody(url.Values{"email": {email}}), formType)
}

func TestTrackingLinkFromTheAcknowledgementWorks(t *testing.T) {
	e := newTestEnv(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	rec := e.do(t, http.MethodGet, publicHost, "/suivi/"+tk.Token, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	body := html.UnescapeString(rec.Body.String())
	for _, want := range []string{tk.Ref, "À traiter", "Mon carnet affiche un montant", "Ma demande est réglée", "Envoyer ma réponse"} {
		assert.Contains(t, body, want)
	}
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "same-origin", rec.Header().Get("Referrer-Policy"))
	assert.Contains(t, body, `<meta name="robots" content="noindex, nofollow, noai, noimageai">`)

	for _, bad := range []string{strings.Repeat("A", 43), "abc"} {
		code, page := e.tracking(t, bad)
		assert.Equal(t, http.StatusNotFound, code)
		assert.Contains(t, page, "Ce lien ne fonctionne plus")
	}
}

func TestTrackingPageHidesInternalNotes(t *testing.T) {
	e := newTestEnv(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	e.apply(t, tk.ID, tickets.Command{Action: tickets.ActionTake})
	e.apply(t, tk.ID, tickets.Command{Action: tickets.ActionNote, Body: "Note secrète du comité"})
	e.apply(t, tk.ID, tickets.Command{Action: tickets.ActionReply, Body: "Voici la réponse du comité."})

	code, page := e.tracking(t, tk.Token)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, page, "Voici la réponse du comité.")
	assert.Contains(t, page, "Alice")
	assert.Contains(t, page, "Présidente")
	assert.Contains(t, page, "En cours")
	assert.NotContains(t, page, "Note secrète")
	for _, m := range e.mails(t) {
		assert.NotContains(t, m.Text, "Note secrète", "a note never reaches a mail")
	}
}

func TestMemberReplyOnWaitingResumesAndAlertsTheClub(t *testing.T) {
	e := newTestEnv(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	e.apply(t, tk.ID, tickets.Command{Action: tickets.ActionTake})
	e.apply(t, tk.ID, tickets.Command{Action: tickets.ActionWait})
	_, waiting := e.tracking(t, tk.Token)
	assert.Contains(t, waiting, "En attente de ta réponse")
	before := len(e.mails(t))

	rec := e.reply(t, tk.Token, "Voici le montant affiché : -180,00 €", pngBytes(t))
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	assert.Equal(t, "/suivi/"+tk.Token, rec.Header().Get("Location"))

	d := e.detail(t, tk.ID)
	assert.Equal(t, tickets.StatusInProgress, d.Status)
	last := d.Messages[len(d.Messages)-1]
	assert.True(t, last.FromMember)
	assert.Len(t, last.Captures, 1)
	club := 0
	for _, m := range e.mails(t)[before:] {
		if m.To == clubEmail && strings.Contains(m.Text, tk.Ref) {
			club++
		}
	}
	assert.Equal(t, 1, club, "the club is alerted once")
	_, page := e.tracking(t, tk.Token)
	assert.Contains(t, page, "Voici le montant affiché : -180,00 €")
}

// Review Focus 4: the 14-day reply window after closure.
func TestReplyWindowAfterClosure(t *testing.T) {
	e := newTestEnv(t)
	early := e.submitTicket(t, "lea.martin@example.org")
	late := e.submitTicket(t, "hugo.bernard@example.org")
	for _, tk := range []testTicket{early, late} {
		rec := e.do(t, http.MethodPost, publicHost, "/suivi/"+tk.Token+"/cloture", nil, formType)
		require.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, tickets.StatusDone, e.detail(t, tk.ID).Status)
	}
	_, done := e.tracking(t, early.Token)
	assert.Contains(t, done, "Fait")
	assert.NotContains(t, done, "Ma demande est réglée", "no second closing button")

	e.clock.advance(tickets.ReplyWindow - time.Second)
	_, page := e.tracking(t, early.Token)
	assert.Contains(t, page, "Envoyer ma réponse")
	require.Equal(t, http.StatusSeeOther, e.reply(t, early.Token, "Le problème est revenu ce matin.").Code)
	reopened := e.detail(t, early.ID)
	assert.Equal(t, tickets.StatusTodo, reopened.Status, "closed without resolver: back to todo")
	assert.True(t, reopened.ClosedAt.IsZero())

	e.clock.advance(2 * time.Second)
	_, page = e.tracking(t, late.Token)
	assert.NotContains(t, page, "Envoyer ma réponse")
	assert.Contains(t, page, "ouvre une nouvelle demande")
	refused := e.reply(t, late.Token, "Le problème est revenu ce matin.")
	assert.Equal(t, http.StatusConflict, refused.Code)
	assert.Equal(t, tickets.StatusDone, e.detail(t, late.ID).Status)
}

func TestMemberCapturesStayWithTheirRequest(t *testing.T) {
	e := newTestEnv(t)
	with := e.submitTicket(t, "lea.martin@example.org", withCapture(t))
	other := e.submitTicket(t, "hugo.bernard@example.org")
	var capID int64
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		`SELECT id FROM attachments WHERE ticket_id = ?`, with.ID).Scan(&capID))
	path := func(token string) string { return "/suivi/" + token + "/captures/" + strconv.FormatInt(capID, 10) }

	own := e.do(t, http.MethodGet, publicHost, path(with.Token), nil)
	require.Equal(t, http.StatusOK, own.Code)
	assert.Equal(t, "image/png", own.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", own.Header().Get("Cache-Control"))
	assert.Equal(t, pngBytes(t), own.Body.Bytes())

	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, path(other.Token), nil).Code)
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/suivi/"+with.Token+"/captures/999", nil).Code)
	_, page := e.tracking(t, with.Token)
	assert.Contains(t, page, path(with.Token))
}

func TestCaptureUnavailableShowsAPlaceholder(t *testing.T) {
	e := newTestEnv(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	e.srv.writeCapture(rec, req, nil, "", fmt.Errorf("get object: %w", tickets.ErrStorage))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "Capture indisponible")
}

func TestMemberReplyValidationAndLimits(t *testing.T) {
	e := newTestEnv(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	empty := e.reply(t, tk.Token, "   ")
	assert.Equal(t, http.StatusUnprocessableEntity, empty.Code)
	assert.Contains(t, html.UnescapeString(empty.Body.String()), "Écris ton message")

	notImage := e.reply(t, tk.Token, "Voici ma capture.", []byte("pas une image"))
	assert.Equal(t, http.StatusUnprocessableEntity, notImage.Code)
	body := html.UnescapeString(notImage.Body.String())
	assert.Contains(t, body, "n'est pas une image PNG, JPEG ou WebP")
	assert.Contains(t, body, ">Voici ma capture.</textarea>", "the typed text comes back")

	// The two refused posts above count too: 18 more reach the limit of 20.
	for range replyTicketLimit - 2 {
		require.Equal(t, http.StatusSeeOther, e.reply(t, tk.Token, "Encore une précision.").Code)
	}
	limited := e.reply(t, tk.Token, "Encore une précision.")
	assert.Equal(t, http.StatusTooManyRequests, limited.Code)
	assert.Contains(t, limited.Body.String(), "Trop de messages")
}

func TestLostLinkAnswersTheSameAndResendsTheOriginalLink(t *testing.T) {
	e := newTestEnv(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	e.clock.advance(31 * 24 * time.Hour)
	require.NoError(t, e.deps.Outbox.Purge(context.Background()), "the acknowledgement leaves the outbox")
	before := len(e.mails(t))

	known := e.askLinks(t, " Lea.Martin@Example.org ")
	unknown := e.askLinks(t, "personne@example.org")
	require.Equal(t, http.StatusSeeOther, known.Code)
	assert.Equal(t, known.Code, unknown.Code)
	assert.Equal(t, known.Header().Get("Location"), unknown.Header().Get("Location"))
	page := e.do(t, http.MethodGet, publicHost, known.Header().Get("Location"), nil)
	assert.Contains(t, html.UnescapeString(page.Body.String()), "Si cette adresse a des demandes")

	resent := false
	for _, m := range e.mails(t)[before:] {
		assert.NotEqual(t, "personne@example.org", m.To)
		if m.To == "lea.martin@example.org" && strings.Contains(m.Text, tk.Ref) &&
			strings.Contains(m.Text, "https://sos.example.org/suivi/"+tk.Token) {
			resent = true
		}
	}
	assert.True(t, resent, "the original link comes back")
}

func TestLostLinkLimits(t *testing.T) {
	e := newTestEnv(t)
	for range recoverEmailLimit {
		require.Equal(t, http.StatusSeeOther, e.askLinks(t, "lea.martin@example.org").Code)
	}
	perEmail := e.askLinks(t, "lea.martin@example.org")
	assert.Equal(t, http.StatusTooManyRequests, perEmail.Code)
	assert.Contains(t, perEmail.Body.String(), "Trop de demandes de liens")
	assert.Equal(t, http.StatusSeeOther, e.askLinks(t, "hugo.bernard@example.org").Code, "5th of the hour for this address")
	assert.Equal(t, http.StatusTooManyRequests, e.askLinks(t, "chloe.petit@example.org").Code, "6th of the hour")
	invalid := e.askLinks(t, "pas une adresse")
	assert.Equal(t, http.StatusTooManyRequests, invalid.Code, "the address limit comes first")
}

func TestLostLinkPageHasTurnstile(t *testing.T) {
	reply := `{"success":false}`
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte(reply))
		assert.NoError(t, err)
	}))
	defer fake.Close()
	e := newTestEnv(t, func(d *Deps) { d.Turnstile = NewTurnstile("site-key", "secret", fake.URL) })
	page := e.do(t, http.MethodGet, publicHost, "/retrouver", nil).Body.String()
	assert.Contains(t, page, `data-action="retrouver"`)
	assert.Equal(t, http.StatusForbidden, e.askLinks(t, "lea.martin@example.org").Code)
}

func TestTrackingTokenNeverReachesLogsOrSpans(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	e := newTestEnv(t)
	tk := e.submitTicket(t, "lea.martin@example.org", withCapture(t))
	var capID int64
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		`SELECT id FROM attachments WHERE ticket_id = ?`, tk.ID).Scan(&capID))

	code, _ := e.tracking(t, tk.Token)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, http.StatusSeeOther, e.reply(t, tk.Token, "Un détail de plus.").Code)
	require.Equal(t, http.StatusOK, e.do(t, http.MethodGet, publicHost, "/suivi/"+tk.Token+"/captures/"+strconv.FormatInt(capID, 10), nil).Code)
	require.Equal(t, http.StatusSeeOther, e.do(t, http.MethodPost, publicHost, "/suivi/"+tk.Token+"/cloture", nil, formType).Code)

	var dump strings.Builder
	ended := spans.Ended()
	names := make([]string, 0, len(ended))
	for _, s := range ended {
		names = append(names, s.Name())
		dump.WriteString(s.Name())
		for _, a := range s.Attributes() {
			dump.WriteString(" " + a.Value.String())
		}
	}
	assert.Contains(t, names, "GET /suivi/{jeton}")
	assert.Contains(t, names, "POST /suivi/{jeton}/reponse")
	logs := e.logs.String()
	for _, secret := range []string{tk.Token, "lea.martin@example.org", "Un détail de plus."} {
		assert.NotContains(t, logs, secret)
		assert.NotContains(t, dump.String(), secret)
	}
}

func TestMemberReplyCaptureLimitMessage(t *testing.T) {
	e := newTestEnv(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	img := pngBytes(t)
	for range 3 { // 9 stored: fewer than 10, yet 3 more do not fit
		require.Equal(t, http.StatusSeeOther, e.reply(t, tk.Token, "Voici.", img, img, img).Code)
	}
	rec := e.reply(t, tk.Token, "Et encore.", img, img, img)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, html.UnescapeString(rec.Body.String()), "Une demande garde 10 captures au plus")
}

func TestMemberReplyOnARequestDeletedMeanwhileShowsTheGonePage(t *testing.T) {
	e := newTestEnv(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	// The reply limiter runs between the token lookup and the reply: delete
	// the request there, as a committee member would concurrently.
	_, err := e.db.ExecContext(context.Background(), `CREATE TRIGGER delete_meanwhile AFTER INSERT ON counters
		WHEN NEW.key LIKE 'reply-ticket:%' BEGIN DELETE FROM tickets; END`)
	require.NoError(t, err)
	rec := e.reply(t, tk.Token, "Trop tard.")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
