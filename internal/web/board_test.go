package web

import (
	"bufio"
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

func inCategory(id string) func(*tickets.Submission) {
	return func(s *tickets.Submission) { s.Fields = tickets.Fields{Category: id, Values: map[string]string{}} }
}

// boardRow returns the unescaped row of request id on the board.
func boardRow(t *testing.T, body string, id int64) string {
	t.Helper()
	start := strings.Index(body, `data-ticket="`+strconv.FormatInt(id, 10)+`"`)
	require.NotEqual(t, -1, start, "row %d missing", id)
	end := strings.Index(body[start:], "</tr>")
	require.NotEqual(t, -1, end)
	return html.UnescapeString(body[start : start+end])
}

func TestBoardFilters(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	todo := e.submitTicket(t, "lea.martin@example.org")
	mine := e.submitTicket(t, "hugo.bernard@example.org", inCategory("autre"))
	done := e.submitTicket(t, "chloe.petit@example.org")
	e.apply(t, mine.ID, tickets.Command{Action: tickets.ActionTake})
	e.apply(t, done.ID, tickets.Command{Action: tickets.ActionClose})
	require.NoError(t, e.deps.Tickets.MemberReply(context.Background(), todo.ID, "Je précise ma demande.", nil))

	board := func(query string) string {
		t.Helper()
		rec := e.do(t, http.MethodGet, adminHost, "/"+query, nil, withCookie(cookie))
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Body.String()
	}
	shows := func(body string, want ...testTicket) {
		t.Helper()
		for _, tk := range []testTicket{todo, mine, done} {
			if containsTicket(want, tk) {
				assert.Contains(t, body, tk.Ref)
			} else {
				assert.NotContains(t, body, tk.Ref)
			}
		}
	}
	def := board("")
	shows(def, todo, mine)
	assert.Contains(t, boardRow(t, def, todo.ID), "a répondu")
	assert.NotContains(t, boardRow(t, def, mine.ID), "a répondu")
	assert.Contains(t, boardRow(t, def, mine.ID), "Alice")
	assert.Contains(t, boardRow(t, def, todo.ID), "Personne")
	assert.Contains(t, boardRow(t, def, todo.ID), "Mon carnet affiche un montant", "description start until lot 3's summary")

	shows(board("?statut=done"), done)
	shows(board("?statut=ouvertes"), todo, mine)
	shows(board("?statut=in_progress"), mine)
	shows(board("?resolveur=moi"), mine)
	shows(board("?resolveur=-"), todo)
	shows(board("?resolveur=alice&statut=done"), done)
	shows(board("?categorie=autre"), mine)
	shows(board("?statut=nimportequoi"), todo, mine)
	assert.Contains(t, board("?statut=done"), `<option value="done" selected>`)
}

func containsTicket(list []testTicket, tk testTicket) bool {
	for _, x := range list {
		if x.ID == tk.ID {
			return true
		}
	}
	return false
}

func TestBoardAgeColors(t *testing.T) {
	e := newTestEnv(t)
	tk := e.submitTicket(t, "lea.martin@example.org")
	row := func() string {
		t.Helper()
		cookie := e.login(t) // logins expire after 30 days of test clock
		rec := e.do(t, http.MethodGet, adminHost, "/", nil, withCookie(cookie))
		require.Equal(t, http.StatusOK, rec.Code)
		return boardRow(t, rec.Body.String(), tk.ID)
	}
	fresh := row()
	assert.Contains(t, fresh, "< 1 h")
	assert.NotContains(t, fresh, "text-warning")
	assert.NotContains(t, fresh, "text-error")

	e.clock.advance(3 * 24 * time.Hour)
	assert.Contains(t, row(), `<span class="text-warning">3 j</span>`)

	e.clock.advance(5 * 24 * time.Hour)
	assert.Contains(t, row(), `<span class="text-error font-bold">8 j</span>`)
}

// Review Focus 2: a category removed from the catalog after filing.
func TestBoardShowsRemovedCategories(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	tk := e.submitTicket(t, "lea.martin@example.org", inCategory("carnet"))
	reduced, err := tickets.LoadCatalog(fstest.MapFS{
		"config/categories.yaml": {Data: []byte("categories:\n  - id: autre\n    label: Autre\n")},
		"config/products.yaml":   {Data: []byte("products:\n  - id: autre\n    label: Autre\n")},
	})
	require.NoError(t, err)
	e.deps.Tickets.Catalog = reduced

	rec := e.do(t, http.MethodGet, adminHost, "/?categorie=carnet", nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code)
	body := html.UnescapeString(rec.Body.String())
	assert.Contains(t, body, tk.Ref, "filtering by a removed category still works")
	assert.Contains(t, boardRow(t, rec.Body.String(), tk.ID), "carnet (retiré)")
	assert.Contains(t, body, `<option value="carnet" selected>carnet (retiré)</option>`)
}

// nextEvent reads stream blocks until one of eventType, returned without
// its closing blank line.
func nextEvent(t *testing.T, stream *bufio.Reader, eventType string) string {
	t.Helper()
	for {
		var block strings.Builder
		for {
			line, err := stream.ReadString('\n')
			require.NoError(t, err)
			if line == "\n" {
				break
			}
			block.WriteString(line)
		}
		if strings.HasPrefix(block.String(), "event: "+eventType+"\n") {
			return block.String()
		}
	}
}

// openStream opens the event stream of cookie's session on a real server
// and returns it once subscribed: changes from then on reach it.
func (e *testEnv) openStream(t *testing.T, cookie *http.Cookie) *bufio.Reader {
	t.Helper()
	ts := httptest.NewServer(e.srv)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/evenements", nil)
	require.NoError(t, err)
	req.Host = adminHost
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, resp.Body.Close()) })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	stream := bufio.NewReader(resp.Body)
	first, err := stream.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "retry: 5000\n", first)
	return stream
}

func TestEventStreamCarriesIDsOnlyAndEndsAtLogout(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	stream := e.openStream(t, cookie)

	tk := e.submitTicket(t, "lea.martin@example.org")
	assert.Equal(t, "event: created\ndata: {\"id\":"+strconv.FormatInt(tk.ID, 10)+"}\n", nextEvent(t, stream, "created"))

	token := e.csrf(t, cookie, "/")
	logout := e.do(t, http.MethodPost, adminHost, "/deconnexion", formBody(url.Values{"csrf": {token}}), formType, withCookie(cookie))
	require.Equal(t, http.StatusSeeOther, logout.Code)
	rest, err := io.ReadAll(stream)
	require.NoError(t, err, "the stream ends at logout")
	for _, secret := range []string{"Martin", "Léa", "lea.martin@example.org", tk.Token} {
		assert.NotContains(t, string(rest), secret)
	}
	assert.NotContains(t, e.logs.String(), "evenements", "the stream is neither logged nor traced")
}

// The keepalive also checks the session: a stream outlives neither a deleted
// session nor an expired one by more than one keepalive.
func TestEventStreamEndsWithItsSession(t *testing.T) {
	ends := map[string]func(t *testing.T, e *testEnv){
		"deleted": func(t *testing.T, e *testEnv) {
			t.Helper()
			_, err := e.db.ExecContext(context.Background(), `DELETE FROM sessions`)
			require.NoError(t, err)
		},
		"expired": func(t *testing.T, e *testEnv) {
			t.Helper()
			e.clock.advance(31 * 24 * time.Hour)
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			e.srv.keepAlive = 20 * time.Millisecond
			stream := e.openStream(t, e.login(t))
			end(t, e)
			done := make(chan error, 1)
			go func() {
				_, err := io.ReadAll(stream)
				done <- err
			}()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the stream outlived its session")
			}
		})
	}
}

func TestEventStreamRefusals(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodGet, adminHost, "/evenements", nil).Code, "no session")
	evil := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodGet, adminHost, "/evenements", nil, withCookie(cookie), evil).Code)
	crossSite := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodGet, adminHost, "/evenements", nil, withCookie(cookie), crossSite).Code)
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, "/evenements", nil).Code)
}
