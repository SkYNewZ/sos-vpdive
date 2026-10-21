package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func chatBody(t *testing.T, v url.Values) io.Reader {
	t.Helper()
	input := make(map[string]string)
	for k := range v {
		input[k] = v.Get(k)
	}
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	return bytes.NewReader(raw)
}
func chatJSON(r *http.Request) { r.Header.Set("Content-Type", "application/json") }

func (e *testEnv) openChat(t *testing.T, cookie *http.Cookie, cursor string) *bufio.Reader {
	t.Helper()
	server := httptest.NewServer(e.srv)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/evenements?chat=1&after="+url.QueryEscape(cursor), nil)
	require.NoError(t, err)
	req.Host = adminHost
	req.AddCookie(cookie)
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
	return bufio.NewReader(resp.Body)
}

func decodeChatEvent(t *testing.T, stream *bufio.Reader, kind string, dst any) {
	t.Helper()
	var event strings.Builder
	for {
		line, err := stream.ReadString('\n')
		require.NoError(t, err)
		if line != "\n" {
			event.WriteString(line)
			continue
		}
		if strings.Contains("\n"+event.String(), "\nevent: "+kind+"\n") {
			for field := range strings.SplitSeq(event.String(), "\n") {
				if data, ok := strings.CutPrefix(field, "data: "); ok {
					require.NoError(t, json.Unmarshal([]byte(data), dst))
					return
				}
			}
			break
		}
		event.Reset()
	}
	t.Fatal("event has no JSON data")
}

func TestChatHTTPRefusals(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	csrf := e.csrf(t, cookie, "/")
	post := func(text string, mutators ...func(*http.Request)) *httptest.ResponseRecorder {
		return e.do(t, http.MethodPost, adminHost, "/chat/messages", chatBody(t, url.Values{"csrf": {csrf}, "text": {text}, "username": {"forged"}}), append([]func(*http.Request){chatJSON, withCookie(cookie)}, mutators...)...)
	}
	require.Equal(t, 409, post("before joining").Code)
	stream := e.openChat(t, cookie, "")
	var ready chatSnapshot
	decodeChatEvent(t, stream, "chat-ready", &ready)
	for _, text := range []string{"", " \n\t ", strings.Repeat("é", 2001)} {
		assert.Equal(t, 422, post(text).Code)
	}
	assert.Equal(t, 413, post(strings.Repeat("x", 17*1024)).Code)
	assert.Equal(t, 403, post("forbidden", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }).Code)
	assert.Equal(t, 403, e.do(t, http.MethodPost, adminHost, "/chat/messages", chatBody(t, url.Values{"csrf": {"forged"}, "text": {"no"}}), chatJSON, withCookie(cookie)).Code)
	assert.Equal(t, 403, e.do(t, http.MethodPost, adminHost, "/chat/messages", nil).Code)
	assert.Equal(t, 404, e.do(t, http.MethodPost, publicHost, "/chat/messages", nil, withCookie(cookie)).Code)
	invalid := append([]byte(`{"csrf":"`+csrf+`","text":"`), 0xff)
	invalid = append(invalid, []byte(`"}`)...)
	assert.Equal(t, 422, e.do(t, http.MethodPost, adminHost, "/chat/messages", bytes.NewReader(invalid), chatJSON, withCookie(cookie)).Code)
	good := post(strings.Repeat("🤿", 2000))
	require.Equal(t, 200, good.Code, good.Body.String())
	var msg chatMessage
	require.NoError(t, json.Unmarshal(good.Body.Bytes(), &msg))
	assert.Equal(t, "alice", msg.Username)
	assert.Len(t, []rune(msg.Text), 2000)
	assert.Equal(t, "no-store", good.Header().Get("Cache-Control"))
	for range 4 {
		require.Equal(t, 200, post("ok").Code)
	}
	assert.Equal(t, 429, post("sixth").Code)
	assert.NotContains(t, e.logs.String(), msg.ID)
	assert.NotContains(t, e.logs.String(), "🤿")
}

func TestChatSSESharedRoomAndResume(t *testing.T) {
	e := newTestEnv(t)
	a := e.login(t)
	person := alice()
	person.Username = "bob"
	person.Name = "Bob"
	require.NoError(t, e.deps.Admins.Insert(context.Background(), person))
	bob := sessionCookie(t, e.postLogin(t, "bob", testPassword))
	aliceStream := e.openChat(t, a, "")
	var ready chatSnapshot
	decodeChatEvent(t, aliceStream, "chat-ready", &ready)
	bobStream := e.openChat(t, bob, "")
	decodeChatEvent(t, bobStream, "chat-ready", &ready)
	resp := e.do(t, http.MethodPost, adminHost, "/chat/messages", chatBody(t, url.Values{"csrf": {e.csrf(t, a, "/")}, "text": {"Hello <script>everyone</script>"}}), chatJSON, withCookie(a))
	require.Equal(t, 200, resp.Code, resp.Body.String())
	var sent, received chatMessage
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &sent))
	decodeChatEvent(t, bobStream, "chat-message", &received)
	assert.Equal(t, sent, received)
	resumed := e.openChat(t, a, ready.Cursor)
	decodeChatEvent(t, resumed, "chat-ready", &ready)
	require.Len(t, ready.Messages, 1)
	assert.Equal(t, sent.ID, ready.Messages[0].ID)
	fresh := e.openChat(t, a, "")
	decodeChatEvent(t, fresh, "chat-ready", &ready)
	assert.Empty(t, ready.Messages)
	assert.False(t, ready.Gap)
}

func TestChatSessionRevocation(t *testing.T) {
	for _, how := range []string{"logout", "expired", "deleted", "reset"} {
		t.Run(how, func(t *testing.T) {
			e := newTestEnv(t)
			e.srv.keepAlive = 10 * time.Millisecond
			cookie := e.login(t)
			stream := e.openChat(t, cookie, "")
			var ready chatSnapshot
			decodeChatEvent(t, stream, "chat-ready", &ready)
			assert.Equal(t, 204, e.do(t, http.MethodGet, adminHost, "/chat/session", nil, withCookie(cookie)).Code)
			switch how {
			case "logout":
				require.Equal(t, 303, e.postAs(t, cookie, "/deconnexion", url.Values{"csrf": {e.csrf(t, cookie, "/")}}).Code)
			case "expired":
				e.clock.advance(31 * 24 * time.Hour)
			case "deleted":
				_, err := e.db.ExecContext(context.Background(), "DELETE FROM sessions")
				require.NoError(t, err)
			case "reset":
				_, err := e.db.ExecContext(context.Background(), "UPDATE sessions SET credential_hash = ?", []byte("revoked"))
				require.NoError(t, err)
			}
			var ended map[string]any
			decodeChatEvent(t, stream, "chat-session-ended", &ended)
			remaining, err := io.ReadAll(stream)
			require.NoError(t, err)
			assert.Empty(t, remaining)
			status := e.do(t, http.MethodGet, adminHost, "/chat/session", nil, withCookie(cookie))
			assert.Equal(t, 403, status.Code)
			assert.Empty(t, status.Header().Get("Location"))
		})
	}
}

func TestChatSSEIsolationAndShutdown(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	stream := e.openChat(t, cookie, "")
	var ready chatSnapshot
	decodeChatEvent(t, stream, "chat-ready", &ready)
	board := e.openStream(t, cookie)
	response := e.do(t, http.MethodPost, adminHost, "/chat/messages", chatBody(t, url.Values{"csrf": {e.csrf(t, cookie, "/")}, "text": {"chat-private-content"}}), chatJSON, withCookie(cookie))
	require.Equal(t, 200, response.Code)
	assert.NotEmpty(t, ready.Cursor)
	e.deps.Broker.Close()
	rest, err := io.ReadAll(stream)
	require.NoError(t, err)
	assert.NotContains(t, string(rest), "chat-session-ended")
	boardRest, err := io.ReadAll(board)
	require.NoError(t, err)
	assert.NotContains(t, string(boardRest), "chat-private-content")
	assert.NotContains(t, e.logs.String(), "chat-private-content")
}

func TestChatSessionLookupFailurePreservesHistory(t *testing.T) {
	e := newTestEnv(t)
	e.srv.keepAlive = 10 * time.Millisecond
	cookie := e.login(t)
	stream := e.openChat(t, cookie, "")
	var ready chatSnapshot
	decodeChatEvent(t, stream, "chat-ready", &ready)
	require.NoError(t, e.db.Close())
	for _, path := range []string{"/chat/session", "/evenements?chat=1"} {
		response := e.do(t, http.MethodGet, adminHost, path, nil, withCookie(cookie))
		assert.Equal(t, http.StatusServiceUnavailable, response.Code, path)
	}
	response := e.do(t, http.MethodPost, adminHost, "/chat/messages", strings.NewReader(`{}`), chatJSON, withCookie(cookie))
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	remaining, err := io.ReadAll(stream)
	require.NoError(t, err)
	assert.NotContains(t, string(remaining), "chat-session-ended", "a database outage does not prove revocation")
}

func TestChatSessionIdentity(t *testing.T) {
	e := newTestEnv(t)
	first := e.login(t)
	second := e.login(t)
	firstToken := e.csrf(t, first, "/")
	secondToken := e.csrf(t, second, "/")
	require.NotEqual(t, firstToken, secondToken, "even replacement sessions of the same account have different identities")
	for _, pair := range []struct {
		cookie *http.Cookie
		token  string
	}{{first, firstToken}, {second, secondToken}} {
		response := e.do(t, http.MethodGet, adminHost, "/chat/session", nil, withCookie(pair.cookie))
		require.Equal(t, http.StatusNoContent, response.Code)
		assert.Equal(t, pair.token, response.Header().Get("X-Chat-Session"))
		var ready struct {
			Session string `json:"session"`
		}
		decodeChatEvent(t, e.openChat(t, pair.cookie, ""), "chat-ready", &ready)
		assert.Equal(t, pair.token, ready.Session)
	}
}
