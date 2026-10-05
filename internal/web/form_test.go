package web

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

var formKeyField = regexp.MustCompile(`name="cle" value="([A-Za-z0-9_-]{43})"`)

// trackingLinkPattern finds the secret tracking link in a member mail.
var trackingLinkPattern = regexp.MustCompile(`https://sos\.example\.org/suivi/([A-Za-z0-9_-]{43})`)

// mails sends the due mails through the fake sender and returns every mail
// it accepted so far, oldest first.
func (e *testEnv) mails(t *testing.T) []mail.Message {
	t.Helper()
	_, err := e.deps.Outbox.SendDue(context.Background(), e.sender)
	require.NoError(t, err)
	e.sender.mu.Lock()
	defer e.sender.mu.Unlock()
	return slices.Clone(e.sender.sent)
}

// objects counts the screenshots in storage.
func (e *testEnv) objects(t *testing.T) int {
	t.Helper()
	list, err := e.blobs.List(context.Background())
	require.NoError(t, err)
	return len(list)
}

// formKey loads the member form and returns its idempotence key.
func (e *testEnv) formKey(t *testing.T) string {
	t.Helper()
	rec := e.do(t, http.MethodGet, publicHost, "/", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	m := formKeyField.FindStringSubmatch(rec.Body.String())
	require.NotNil(t, m, "no form key in %s", rec.Body.String())
	return m[1]
}

// validRequest is a complete request from an address of members_valid.xlsx,
// in a category without dedicated fields.
func validRequest(key string) url.Values {
	return url.Values{
		"cle": {key}, "prenom": {"Léa"}, "nom": {"Martin"}, "email": {" Lea.Martin@Example.ORG "},
		"categorie":   {"autre"},
		"description": {"Je ne vois plus mes réservations dans VPDive depuis hier soir."},
	}
}

func (e *testEnv) sendRequest(t *testing.T, v url.Values, files ...[]byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartBody(t, v, files...)
	return e.do(t, http.MethodPost, publicHost, "/demandes", body, contentType)
}

func TestMemberFormPage(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	rec := e.do(t, http.MethodGet, publicHost, "/", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	body := html.UnescapeString(rec.Body.String())
	for _, want := range []string{
		"data-category-select", `data-category="carnet"`, "N'envoie ici ni certificat médical ni document de santé",
		"Celle de ton compte VPDive", "/static/app.js", "https://vpdive.example.org/w/f-a-q",
		"https://vpdive.example.org/w/tarifs", `name="site_web"`, `enctype="multipart/form-data"`,
	} {
		assert.Contains(t, body, want)
	}
	carnet, ok := e.deps.Tickets.Catalog.Category("carnet")
	require.True(t, ok)
	assert.Contains(t, body, carnet.Help, "the category help is on the page, shown with its fields")
	assert.NotContains(t, body, `value="bug"`, "a committee-only category is never offered")
	assert.NotContains(t, body, "cf-turnstile", "no widget without Turnstile keys")
	first, second := e.formKey(t), e.formKey(t)
	assert.NotEqual(t, first, second, "every display draws a new form key")
}

func TestMemberFormFullPath(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	key := e.formKey(t)
	rec := e.sendRequest(t, validRequest(key), pngBytes(t))
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	loc := rec.Header().Get("Location")
	require.Regexp(t, `^/demandes/envoyee\?ref=CPP-\d{4}$`, loc)
	ref := strings.TrimPrefix(loc, "/demandes/envoyee?ref=")

	sent := e.do(t, http.MethodGet, publicHost, loc, nil)
	require.Equal(t, http.StatusOK, sent.Code)
	assert.Contains(t, sent.Body.String(), ref)
	assert.NotContains(t, sent.Body.String(), "/suivi/", "the tracking link only travels by mail")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/demandes/envoyee?ref=%3Cb%3E", nil).Code)

	assert.Equal(t, 1, e.count(t, "tickets"))
	assert.Equal(t, 1, e.count(t, "attachments"))
	assert.Equal(t, 1, e.objects(t))
	var member, club bool
	for _, m := range e.mails(t) {
		switch m.To {
		case "lea.martin@example.org":
			member = trackingLinkPattern.MatchString(m.Text) && strings.Contains(m.Text, ref)
		case clubEmail:
			club = strings.Contains(m.Text, ref) && !strings.Contains(m.Text, "/suivi/")
		}
	}
	assert.True(t, member, "acknowledgement with the tracking link")
	assert.True(t, club, "club alert without the secret link")

	logs := e.logs.String()
	for _, secret := range []string{"lea.martin@example.org", "Lea.Martin", "Martin", "réservations", key} {
		assert.NotContains(t, logs, secret)
	}
	noOrigin := func(r *http.Request) { r.Header.Del("Origin") }
	body, contentType := multipartBody(t, validRequest(e.formKey(t)))
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodPost, publicHost, "/demandes", body, contentType, noOrigin).Code)
}

func TestUnknownAddressIsBlockedBeforeAnyWrite(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	v := validRequest(e.formKey(t))
	v.Set("email", "inconnu@example.org")
	rec := e.sendRequest(t, v, pngBytes(t))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	body := html.UnescapeString(rec.Body.String())
	assert.Contains(t, body, "Cette adresse n'est pas celle d'un compte VPDive du club. Utilise l'adresse de ton compte, ou écris au club.")
	assert.Contains(t, body, `value="Léa"`, "typed values come back")
	assert.Contains(t, body, "Ajoute de nouveau tes captures")
	assert.Zero(t, e.count(t, "tickets"))
	assert.Zero(t, e.objects(t))
}

func TestCommitteeOnlyCategoryIsRefused(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	v := validRequest(e.formKey(t))
	v.Set("categorie", "bug")
	rec := e.sendRequest(t, v)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "Choisis une catégorie dans la liste.")
	assert.Zero(t, e.count(t, "tickets"))
}

func TestResendingTheSameFormFilesOneRequest(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	key := e.formKey(t)
	first := e.sendRequest(t, validRequest(key))
	second := e.sendRequest(t, validRequest(key))
	require.Equal(t, http.StatusSeeOther, first.Code)
	require.Equal(t, http.StatusSeeOther, second.Code)
	assert.Equal(t, first.Header().Get("Location"), second.Header().Get("Location"))
	assert.Equal(t, 1, e.count(t, "tickets"))
}

func TestFormRateLimitPerAddress(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	e.deps.Config.FormRateLimit = 2
	for range 2 {
		require.Equal(t, http.StatusSeeOther, e.sendRequest(t, validRequest(e.formKey(t))).Code)
	}
	rec := e.sendRequest(t, validRequest(e.formKey(t)))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Contains(t, html.UnescapeString(rec.Body.String()), "Trop d'envois en peu de temps")
	assert.Equal(t, 2, e.count(t, "tickets"))
}

func TestFormRateLimitPerEmail(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_minimal.xlsx")
	for range formEmailLimit {
		require.Equal(t, http.StatusSeeOther, e.sendRequest(t, validRequest(e.formKey(t))).Code)
	}
	assert.Equal(t, http.StatusTooManyRequests, e.sendRequest(t, validRequest(e.formKey(t))).Code)
	other := validRequest(e.formKey(t))
	other.Set("email", "hugo.bernard@example.org")
	assert.Equal(t, http.StatusSeeOther, e.sendRequest(t, other).Code)
}

func TestRequiredCategoryFieldErrorSitsBesideTheField(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	var category tickets.Category
	var field tickets.Field
	for _, c := range e.deps.Tickets.Catalog.Public() {
		for _, f := range c.Fields {
			if f.Required && field.ID == "" {
				category, field = c, f
			}
		}
	}
	require.NotEmpty(t, field.ID, "the catalog has a required field (spec §3.1)")
	v := validRequest(e.formKey(t))
	v.Set("categorie", category.ID)
	rec := e.sendRequest(t, v)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	body := rec.Body.String()
	name := tickets.FieldName(category.ID, field.ID)
	assert.Contains(t, body, `id="err-`+name+`"`)
	assert.Contains(t, body, `data-category="`+category.ID+`">`, "the chosen category's fields are shown, not hidden")
	label := strings.Index(body, `for="`+name+`"`)
	msg := strings.Index(body, `id="err-`+name+`"`)
	assert.Greater(t, msg, label, "the error follows its field")
}

func TestNonImageCaptureIsRefused(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	rec := e.sendRequest(t, validRequest(e.formKey(t)), []byte("%PDF-1.7 certificat"))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, html.UnescapeString(rec.Body.String()), "Le fichier n° 1 n'est pas une image PNG, JPEG ou WebP")
	assert.Zero(t, e.count(t, "tickets"))
	assert.Zero(t, e.objects(t))
}

func TestFormClosedWithoutMembersList(t *testing.T) {
	e := newTestEnv(t)
	page := e.do(t, http.MethodGet, publicHost, "/", nil)
	assert.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "pas encore ouvert")
	assert.NotContains(t, page.Body.String(), `name="cle"`)
	rec := e.sendRequest(t, validRequest(strings.Repeat("a", 43)))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Zero(t, e.count(t, "tickets"))
}

func TestHoneypotAndExpiredKey(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	bot := validRequest(e.formKey(t))
	bot.Set("site_web", "https://spam.example")
	assert.Equal(t, http.StatusBadRequest, e.sendRequest(t, bot).Code)
	assert.Equal(t, http.StatusBadRequest, e.sendRequest(t, validRequest("short")).Code)
	assert.Zero(t, e.count(t, "tickets"))
}

func TestTurnstileOnTheMemberForm(t *testing.T) {
	var reply string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte(reply))
		assert.NoError(t, err)
	}))
	defer fake.Close()
	e := newTestEnv(t, func(d *Deps) {
		d.Turnstile = NewTurnstile("1x00000000000000000000AA", "1x0000000000000000000000000000000AA", fake.URL)
	})
	e.importMembers(t, "members_valid.xlsx")
	page := e.do(t, http.MethodGet, publicHost, "/", nil).Body.String()
	assert.Contains(t, page, `class="cf-turnstile" data-sitekey="1x00000000000000000000AA" data-action="demande"`)
	assert.Contains(t, page, "<noscript>")

	key := e.formKey(t)
	v := validRequest(key)
	v.Set(turnstileField, "token-xyz")
	reply = `{"success":false,"error-codes":["invalid-input-response"]}`
	refused := e.sendRequest(t, v, pngBytes(t))
	assert.Equal(t, http.StatusForbidden, refused.Code)
	body := html.UnescapeString(refused.Body.String())
	assert.Contains(t, body, "Le contrôle anti-robot a échoué")
	assert.Contains(t, body, `value="`+key+`"`, "the same form key comes back")
	assert.Contains(t, body, "Ajoute de nouveau tes captures")
	assert.Zero(t, e.count(t, "tickets"))

	reply = `{"success":true,"hostname":"comite.example.org","action":"demande"}`
	assert.Equal(t, http.StatusForbidden, e.sendRequest(t, v).Code, "a token for the other host is refused")

	reply = `{"success":true,"hostname":"sos.example.org","action":"demande"}`
	assert.Equal(t, http.StatusSeeOther, e.sendRequest(t, v).Code)
	assert.Equal(t, 1, e.count(t, "tickets"))
}

func TestFormKeyIsNotRestoredByTheBrowser(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	page := e.do(t, http.MethodGet, publicHost, "/", nil).Body.String()
	assert.Regexp(t, `name="cle" value="[A-Za-z0-9_-]{43}" autocomplete="off"`, page)
}

func TestDuplicateOfFiledRequestIsNotRateLimited(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_minimal.xlsx")
	key := e.formKey(t)
	first := e.sendRequest(t, validRequest(key))
	require.Equal(t, http.StatusSeeOther, first.Code)
	for i := 1; i < formEmailLimit; i++ {
		require.Equal(t, http.StatusSeeOther, e.sendRequest(t, validRequest(e.formKey(t))).Code)
	}
	dup := e.sendRequest(t, validRequest(key))
	assert.Equal(t, http.StatusSeeOther, dup.Code)
	assert.Equal(t, first.Header().Get("Location"), dup.Header().Get("Location"))
}
