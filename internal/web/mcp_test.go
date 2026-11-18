package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mcpEnv is a server with the MCP routes on, every import in place and
// alice's connection: it returns her tokens.
func mcpEnv(t *testing.T, opts ...func(*Deps)) (*testEnv, tokenResponse) {
	t.Helper()
	e, base := newMCPEnv(t, opts...)
	e.importMembers(t, "members_valid.xlsx")
	e.importPayments(t)
	e.importMollie(t)
	e.importCalendar(t, "calendar_view.json")
	return e, e.connect(t, e.login(t), base+"/client.json")
}

// rpc posts a JSON-RPC request to the MCP endpoint as a client's server
// does: a bearer token, no cookie, no Origin.
func (e *testEnv) rpc(t *testing.T, token, method string, mutators ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"` + method + `"}`)
	return e.do(t, http.MethodPost, adminHost, mcpPath, body, append([]func(*http.Request){func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	}}, mutators...)...)
}

// asCommittee sends a client's requests to the committee host of a test
// server, with its token.
type asCommittee struct{ token string }

func (a asCommittee) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = adminHost
	r.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(r)
}

// session connects the SDK's client to the MCP endpoint with token.
func (e *testEnv) session(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(e.srv)
	t.Cleanup(ts.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL + mcpPath, HTTPClient: &http.Client{Transport: asCommittee{token}}, DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, session.Close()) })
	return session
}

// callTool calls a tool and returns its structured result as decoded JSON,
// and its error text when the tool refused.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err, name)
	if res.IsError {
		require.Len(t, res.Content, 1, name)
		text, ok := res.Content[0].(*mcp.TextContent)
		require.True(t, ok, name)
		return nil, text.Text
	}
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out), name)
	return out, ""
}

func TestMCPWithoutTokenAnswers401WithResourceMetadata(t *testing.T) {
	e, tok := mcpEnv(t)
	challenge := `Bearer resource_metadata="https://` + adminHost + `/.well-known/oauth-protected-resource"`
	for name, token := range map[string]string{"no token": "", "unknown token": strings.Repeat("a", 43), "the refresh token": tok.RefreshToken, "not a token": "x"} {
		rec := e.rpc(t, token, "tools/list")
		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.Equal(t, challenge, rec.Header().Get("WWW-Authenticate"), name)
	}
	assert.Equal(t, http.StatusUnauthorized, e.do(t, http.MethodGet, adminHost, mcpPath, nil).Code)

	rec := e.rpc(t, tok.AccessToken, "tools/list")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "comite_find_member")
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodPost, publicHost, mcpPath, nil).Code, "committee host only")
	assert.Empty(t, e.sentryEvents(t), "a refused token is expected")
}

// MCP transport security: a page of another site must not drive the
// endpoint from a member's browser.
func TestMCPRefusesForeignOrigin(t *testing.T) {
	e, tok := mcpEnv(t)
	origin := func(o string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Origin", o) }
	}
	assert.Equal(t, http.StatusForbidden, e.rpc(t, tok.AccessToken, "tools/list", origin("https://evil.example.org")).Code)
	assert.Equal(t, http.StatusForbidden, e.rpc(t, tok.AccessToken, "tools/list", origin("https://"+publicHost)).Code)
	assert.Equal(t, http.StatusOK, e.rpc(t, tok.AccessToken, "tools/list", origin("https://"+adminHost)).Code)
}

// config/robots.yaml refuses Claude-User and ChatGPT-User on every page: an
// assistant's connector may announce itself as one.
func TestMCPPassesAIRobotUserAgents(t *testing.T) {
	e, tok := mcpEnv(t)
	for _, agent := range []string{"Claude-User/1.0", "Mozilla/5.0 (compatible; ChatGPT-User/1.0)"} {
		robot := func(r *http.Request) { r.Header.Set("User-Agent", agent) }
		assert.Equal(t, http.StatusOK, e.rpc(t, tok.AccessToken, "tools/list", robot).Code, agent)
		assert.Equal(t, http.StatusUnauthorized, e.rpc(t, "", "tools/list", robot).Code, agent+": the token still decides")
		assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodGet, adminHost, "/", nil, robot).Code, agent)
	}
}

// The SDK compares a token's expiry with the machine's clock; the server
// checks it on its own, which tests move.
func TestMCPTokenExpiresOnTheServerClock(t *testing.T) {
	e, tok := mcpEnv(t)
	e.clock.advance(accessTTL - time.Second)
	assert.Equal(t, http.StatusOK, e.rpc(t, tok.AccessToken, "tools/list").Code)
	e.clock.advance(time.Second)
	assert.Equal(t, http.StatusUnauthorized, e.rpc(t, tok.AccessToken, "tools/list").Code)

	var used int64
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT last_used_at FROM mcp_connections`).Scan(&used))
	assert.Equal(t, e.clock.now().Add(-time.Second).Unix(), used, "the page shows when the connection last served")
}

func TestMCPConnectionEndsWithThePasswordAndTheAccount(t *testing.T) {
	ctx := context.Background()
	e, tok := mcpEnv(t)
	require.Equal(t, http.StatusOK, e.rpc(t, tok.AccessToken, "tools/list").Code)
	_, err := e.deps.Admins.ResetPassword(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, e.rpc(t, tok.AccessToken, "tools/list").Code, "a reset ends the connection")

	e, tok = mcpEnv(t)
	_, err = e.db.ExecContext(ctx, `UPDATE mcp_connections SET credential_hash = 'an older password hash'`)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, e.rpc(t, tok.AccessToken, "tools/list").Code, "even before RevokeStale deletes it")

	e, tok = mcpEnv(t)
	require.NoError(t, e.deps.Admins.Delete(ctx, "alice"))
	assert.Equal(t, http.StatusUnauthorized, e.rpc(t, tok.AccessToken, "tools/list").Code)
}

func TestMCPIsRateLimitedPerConnection(t *testing.T) {
	e, tok := mcpEnv(t)
	for range mcpCallLimit {
		require.Equal(t, http.StatusOK, e.rpc(t, tok.AccessToken, "ping").Code)
	}
	rec := e.rpc(t, tok.AccessToken, "ping")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "3600", rec.Header().Get("Retry-After"))
}

// Spec §9.9: a call is logged by its tool's name, never by what it asked or
// what it read.
func TestMCPLogsToolNameOnly(t *testing.T) {
	e, tok := mcpEnv(t)
	session := e.session(t, tok.AccessToken)
	got, refusal := callTool(t, session, "comite_find_member", map[string]any{"query": "hugo.bernard@example.org"})
	require.Empty(t, refusal)
	require.Len(t, got["candidats"], 1)
	_, refusal = callTool(t, session, "comite_member_payments", map[string]any{"adherent": "personne@example.org"})
	require.NotEmpty(t, refusal)

	logs := e.logs.String()
	assert.Contains(t, logs, `"msg":"mcp tool","tool":"comite_find_member","outcome":"ok"`)
	assert.Contains(t, logs, `"msg":"mcp tool","tool":"comite_member_payments","outcome":"refused"`)
	assert.Contains(t, logs, `"route":"/mcp"`)
	for _, secret := range []string{"hugo.bernard", "Hugo", "Bernard", "personne@example.org", tok.AccessToken} {
		assert.NotContains(t, logs, secret)
	}
	assert.Empty(t, e.sentryEvents(t), "a refused call is expected")
}
