package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

const (
	clientRedirect = "https://client.example.org/callback"
	clientState    = "state-9f3c"
	pkceVerifier   = "a-pkce-verifier-of-forty-three-characters-min"
	mcpResource    = "https://" + adminHost + "/mcp"
)

func pkceChallenge() string {
	sum := sha256.Sum256([]byte(pkceVerifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newMCPEnv is a server with the MCP routes on and a host that serves
// client id documents: /client.json is a sound one, the others are wrong in
// one way each. It returns that host's base URL.
func newMCPEnv(t *testing.T, opts ...func(*Deps)) (*testEnv, string) {
	t.Helper()
	var base string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{"client_id": base + r.URL.Path, "client_name": "Claude", "redirect_uris": []string{clientRedirect}}
		switch r.URL.Path {
		case "/client.json":
		case "/loopback.json":
			doc["redirect_uris"] = []string{"http://127.0.0.1/callback"}
		case "/other.json":
			doc["client_id"] = base + "/client.json"
		case "/big.json":
			doc["client_name"] = strings.Repeat("x", clientDocLimit)
		case "/moved.json":
			http.Redirect(w, r, "/client.json", http.StatusFound)
			return
		default:
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(doc))
	}))
	t.Cleanup(ts.Close)
	base = ts.URL
	client := ts.Client()
	client.CheckRedirect = newOAuthHTTP().CheckRedirect
	on := func(d *Deps) {
		d.Config.MCP = &config.MCP{ClientHosts: []string{"127.0.0.1"}}
		d.OAuthHTTP = client
	}
	return newTestEnv(t, append([]func(*Deps){on}, opts...)...), base
}

// authorizeTarget is a sound authorization request of the client at
// clientID, changed by change.
func authorizeTarget(clientID string, change ...func(url.Values)) string {
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {clientRedirect}, "state": {clientState},
		"code_challenge": {pkceChallenge()}, "code_challenge_method": {"S256"}, "resource": {mcpResource}, "scope": {"read"},
	}
	for _, c := range change {
		c(q)
	}
	return oauthAuthorizePath + "?" + q.Encode()
}

func codeForm(clientID, code string) url.Values {
	return url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"code_verifier": {pkceVerifier}, "redirect_uri": {clientRedirect}, "resource": {mcpResource},
	}
}

func refreshForm(clientID, refresh string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}
}

var (
	demandePattern = regexp.MustCompile(`name="demande" value="([^"]+)"`)
	retourPattern  = regexp.MustCompile(`href="([^"]+)">Continuer`)
)

// consent opens the consent screen of target as cookie's account, allows
// the client and returns where it is sent back.
func (e *testEnv) consent(t *testing.T, cookie *http.Cookie, target string) *url.URL {
	t.Helper()
	rec := e.do(t, http.MethodGet, adminHost, target, nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	nonce, csrf := demandePattern.FindStringSubmatch(rec.Body.String()), csrfPattern.FindStringSubmatch(rec.Body.String())
	require.NotNil(t, nonce)
	require.NotNil(t, csrf)
	rec = e.postAs(t, cookie, oauthAuthorizePath, url.Values{"csrf": {csrf[1]}, "demande": {nonce[1]}, "action": {"autoriser"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	m := retourPattern.FindStringSubmatch(rec.Body.String())
	require.NotNil(t, m, rec.Body.String())
	back, err := url.Parse(html.UnescapeString(m[1]))
	require.NoError(t, err)
	return back
}

// token posts form to the token endpoint as a client's server does: no
// cookie, no Origin.
func (e *testEnv) token(t *testing.T, form url.Values) (*httptest.ResponseRecorder, tokenResponse) {
	t.Helper()
	rec := e.do(t, http.MethodPost, adminHost, oauthTokenPath, formBody(form), formType, func(r *http.Request) { r.Header.Del("Origin") })
	var out tokenResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return rec, out
}

// connect authorizes the client at clientID as cookie's account and returns
// its tokens.
func (e *testEnv) connect(t *testing.T, cookie *http.Cookie, clientID string) tokenResponse {
	t.Helper()
	back := e.consent(t, cookie, authorizeTarget(clientID))
	rec, tok := e.token(t, codeForm(clientID, back.Query().Get("code")))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return tok
}

func tokenRefusal(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var f oauthFailure
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &f), rec.Body.String())
	return f.Error
}

func TestOAuthFullFlow(t *testing.T) {
	e, base := newMCPEnv(t)
	cookie := e.login(t)
	clientID := base + "/client.json"

	back := e.consent(t, cookie, authorizeTarget(clientID))
	assert.Equal(t, "client.example.org", back.Host)
	assert.Equal(t, "/callback", back.Path)
	assert.Equal(t, clientState, back.Query().Get("state"))
	assert.Equal(t, "https://"+adminHost, back.Query().Get("iss"), "RFC 9207")
	code := back.Query().Get("code")
	require.True(t, secure.IsToken(code))

	rec, tok := e.token(t, codeForm(clientID, code))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "Bearer", tok.TokenType)
	assert.Equal(t, 3600, tok.ExpiresIn)
	assert.Equal(t, scopeRead, tok.Scope)
	assert.True(t, secure.IsToken(tok.AccessToken))
	assert.True(t, secure.IsToken(tok.RefreshToken))

	var (
		username, client, name, scope string
		access, refresh               []byte
		expires                       int64
	)
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		`SELECT username, client_id, client_name, scope, access_hash, refresh_hash, expires_at FROM mcp_connections`).
		Scan(&username, &client, &name, &scope, &access, &refresh, &expires))
	assert.Equal(t, "alice", username)
	assert.Equal(t, clientID, client)
	assert.Equal(t, "Claude", name)
	assert.Equal(t, scopeRead, scope)
	assert.Equal(t, secure.TokenHash(tok.AccessToken), access, "only hashes are stored")
	assert.Equal(t, secure.TokenHash(tok.RefreshToken), refresh)
	assert.Equal(t, e.clock.now().Add(30*24*time.Hour).Unix(), expires)

	for _, secret := range []string{code, tok.AccessToken, tok.RefreshToken, clientState, pkceVerifier} {
		assert.NotContains(t, e.logs.String(), secret)
	}
}

func TestOAuthAuthorizeSendsToLoginAndBack(t *testing.T) {
	e, base := newMCPEnv(t)
	target := authorizeTarget(base + "/client.json")

	rec := e.do(t, http.MethodGet, adminHost, target, nil)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	login := rec.Header().Get("Location")
	assert.Equal(t, "/connexion?suite="+url.QueryEscape(target), login)

	rec = e.do(t, http.MethodGet, adminHost, login, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `name="suite" value="`+html.EscapeString(target)+`"`)

	body := formBody(url.Values{"username": {"alice"}, "password": {testPassword}, "suite": {target}})
	rec = e.do(t, http.MethodPost, adminHost, "/connexion", body, formType)
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	assert.Equal(t, target, rec.Header().Get("Location"))

	rec = e.do(t, http.MethodGet, adminHost, login, nil, withCookie(sessionCookie(t, rec)))
	assert.Equal(t, target, rec.Header().Get("Location"), "already signed in: straight back to the request")
}

// The return path of a sign-in is the authorization request and nothing
// else: an open redirect would hand a signed-in member to any site.
func TestLoginReturnPathOnlyAllowsAuthorize(t *testing.T) {
	on, _ := newMCPEnv(t)
	for _, suite := range []string{
		"https://evil.example.org/", "//evil.example.org/oauth/authorize?x=1", "/compte", "/oauth/authorize",
		"/oauth/authorizer?x=1", "/oauth/authorize/../../compte?x=1", `/oauth/authorize\@evil.example.org?x=1`,
	} {
		body := formBody(url.Values{"username": {"alice"}, "password": {testPassword}, "suite": {suite}})
		rec := on.do(t, http.MethodPost, adminHost, "/connexion", body, formType)
		require.Equal(t, http.StatusSeeOther, rec.Code, suite)
		assert.Equal(t, "/", rec.Header().Get("Location"), suite)
	}

	off := newTestEnv(t)
	body := formBody(url.Values{"username": {"alice"}, "password": {testPassword}, "suite": {"/oauth/authorize?x=1"}})
	rec := off.do(t, http.MethodPost, adminHost, "/connexion", body, formType)
	assert.Equal(t, "/", rec.Header().Get("Location"), "no return path without the MCP server")
}

func TestOAuthRefusesClientOffTheAllowlist(t *testing.T) {
	e, base := newMCPEnv(t)
	cookie := e.login(t)
	for _, clientID := range []string{
		"https://evil.example.org/client.json", strings.Replace(base, "https://", "http://", 1) + "/client.json",
		base, base + "/", "", "claude",
	} {
		rec := e.do(t, http.MethodGet, adminHost, authorizeTarget(clientID), nil, withCookie(cookie))
		assert.Equal(t, http.StatusBadRequest, rec.Code, clientID)
		assert.Empty(t, rec.Header().Get("Location"), clientID)
		assert.Contains(t, rec.Body.String(), "pas reconnue", clientID)
	}
	assert.Contains(t, e.logs.String(), `"msg":"oauth client refused"`)
	assert.Empty(t, e.sentryEvents(t), "a refused client is expected: below Error")
}

func TestOAuthClientDocumentIsBounded(t *testing.T) {
	e, base := newMCPEnv(t)
	cookie := e.login(t)
	for _, doc := range []string{"/moved.json", "/big.json", "/other.json", "/absent.json"} {
		rec := e.do(t, http.MethodGet, adminHost, authorizeTarget(base+doc), nil, withCookie(cookie))
		assert.Equal(t, http.StatusBadRequest, rec.Code, doc)
		assert.Empty(t, rec.Header().Get("Location"), doc)
	}
}

func TestOAuthRefusesRedirectURINotInDocument(t *testing.T) {
	e, base := newMCPEnv(t)
	cookie := e.login(t)
	for _, redirect := range []string{"https://evil.example.org/callback", clientRedirect + "/more", clientRedirect + "?x=1", ""} {
		rec := e.do(t, http.MethodGet, adminHost,
			authorizeTarget(base+"/client.json", func(q url.Values) { q.Set("redirect_uri", redirect) }), nil, withCookie(cookie))
		assert.Equal(t, http.StatusBadRequest, rec.Code, redirect)
		assert.Empty(t, rec.Header().Get("Location"), "nothing is sent to an address the client did not publish")
		assert.NotContains(t, rec.Body.String(), "evil.example.org")
	}
}

func TestOAuthLoopbackRedirectIgnoresPort(t *testing.T) {
	allowed := []string{"http://127.0.0.1/callback", "http://localhost/callback", "https://client.example.org/callback"}
	for uri, want := range map[string]bool{
		"http://127.0.0.1:53211/callback":          true,
		"http://localhost:8080/callback":           true,
		"http://127.0.0.1/callback":                true,
		"http://127.0.0.1:53211/other":             false,
		"http://127.0.0.1:53211/callback?x=1":      false,
		"https://127.0.0.1:53211/callback":         false,
		"http://192.168.1.10:53211/callback":       false,
		"https://client.example.org:8443/callback": false,
	} {
		assert.Equal(t, want, redirectAllowed(allowed, uri), uri)
	}

	e, base := newMCPEnv(t)
	cookie := e.login(t)
	target := authorizeTarget(base+"/loopback.json", func(q url.Values) { q.Set("redirect_uri", "http://127.0.0.1:53211/callback") })
	back := e.consent(t, cookie, target)
	assert.Equal(t, "127.0.0.1:53211", back.Host)
}

func TestOAuthRequiresPKCEAndResource(t *testing.T) {
	e, base := newMCPEnv(t)
	cookie := e.login(t)
	clientID := base + "/client.json"
	for name, c := range map[string]struct {
		change func(url.Values)
		want   string
	}{
		"no challenge":  {func(q url.Values) { q.Del("code_challenge") }, "invalid_request"},
		"plain method":  {func(q url.Values) { q.Set("code_challenge_method", "plain") }, "invalid_request"},
		"other server":  {func(q url.Values) { q.Set("resource", "https://other.example.org/mcp") }, "invalid_target"},
		"implicit flow": {func(q url.Values) { q.Set("response_type", "token") }, "unsupported_response_type"},
	} {
		rec := e.do(t, http.MethodGet, adminHost, authorizeTarget(clientID, c.change), nil, withCookie(cookie))
		require.Equal(t, http.StatusSeeOther, rec.Code, name)
		back, err := url.Parse(rec.Header().Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, "client.example.org", back.Host, name)
		assert.Equal(t, c.want, back.Query().Get("error"), name)
		assert.Equal(t, clientState, back.Query().Get("state"), name)
		assert.Equal(t, "https://"+adminHost, back.Query().Get("iss"), name)
	}

	for name, change := range map[string]func(url.Values){
		"wrong verifier": func(f url.Values) { f.Set("code_verifier", pkceVerifier+"x") },
		"no verifier":    func(f url.Values) { f.Del("code_verifier") },
		"other server":   func(f url.Values) { f.Set("resource", "https://other.example.org/mcp") },
		"other client":   func(f url.Values) { f.Set("client_id", base+"/loopback.json") },
		"other redirect": func(f url.Values) { f.Set("redirect_uri", "https://evil.example.org/callback") },
	} {
		back := e.consent(t, cookie, authorizeTarget(clientID))
		form := codeForm(clientID, back.Query().Get("code"))
		change(form)
		rec, _ := e.token(t, form)
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
		assert.Equal(t, "invalid_grant", tokenRefusal(t, rec), name)
	}
	assert.Zero(t, e.count(t, "mcp_connections"))
}

func TestOAuthAuthorizeNeedsOriginAndCSRF(t *testing.T) {
	e, base := newMCPEnv(t)
	cookie := e.login(t)
	rec := e.do(t, http.MethodGet, adminHost, authorizeTarget(base+"/client.json"), nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code)
	nonce, csrf := demandePattern.FindStringSubmatch(rec.Body.String())[1], csrfPattern.FindStringSubmatch(rec.Body.String())[1]
	answer := url.Values{"csrf": {csrf}, "demande": {nonce}, "action": {"autoriser"}}

	rec = e.do(t, http.MethodPost, adminHost, oauthAuthorizePath, formBody(answer), formType, withCookie(cookie),
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.org") })
	assert.Equal(t, http.StatusForbidden, rec.Code, "another site cannot answer for the member")

	rec = e.postAs(t, cookie, oauthAuthorizePath, url.Values{"csrf": {"wrong"}, "demande": {nonce}, "action": {"autoriser"}})
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = e.do(t, http.MethodPost, adminHost, oauthAuthorizePath, formBody(answer), formType)
	assert.Equal(t, http.StatusForbidden, rec.Code, "no session")

	rec = e.postAs(t, cookie, oauthAuthorizePath, url.Values{"csrf": {csrf}, "demande": {"unknown"}, "action": {"autoriser"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "a expiré")

	require.Equal(t, http.StatusOK, e.postAs(t, cookie, oauthAuthorizePath, answer).Code, "the refused attempts left the request in place")
	rec = e.postAs(t, cookie, oauthAuthorizePath, answer)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a request is answered once")
}

// Chrome applies form-action to the redirects that follow a form: the way
// back to the client is a page, so the committee's policy stays whole.
func TestOAuthDecisionKeepsTheFormPolicy(t *testing.T) {
	e, base := newMCPEnv(t)
	cookie := e.login(t)
	target := authorizeTarget(base + "/client.json")

	rec := e.do(t, http.MethodGet, adminHost, target, nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.HasSuffix(rec.Header().Get("Content-Security-Policy"), "form-action 'self'"), "the policy is the site's, unchanged")
	assert.Contains(t, rec.Body.String(), "Autoriser Claude")
	assert.Contains(t, rec.Body.String(), "127.0.0.1")
	nonce, csrf := demandePattern.FindStringSubmatch(rec.Body.String())[1], csrfPattern.FindStringSubmatch(rec.Body.String())[1]

	rec = e.postAs(t, cookie, oauthAuthorizePath, url.Values{"csrf": {csrf}, "demande": {nonce}, "action": {"refuser"}})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"))
	assert.True(t, strings.HasSuffix(rec.Header().Get("Content-Security-Policy"), "form-action 'self'"), "the policy is the site's, unchanged")
	assert.Contains(t, rec.Body.String(), `<meta http-equiv="refresh" content="0; url=https://client.example.org/callback?`)
	back, err := url.Parse(html.UnescapeString(retourPattern.FindStringSubmatch(rec.Body.String())[1]))
	require.NoError(t, err)
	assert.Equal(t, "access_denied", back.Query().Get("error"))
	assert.Empty(t, back.Query().Get("code"))
	assert.Equal(t, clientState, back.Query().Get("state"))
	assert.Zero(t, e.count(t, "mcp_connections"))
}

func TestOAuthCodeIsSingleUse(t *testing.T) {
	e, base := newMCPEnv(t)
	cookie := e.login(t)
	clientID := base + "/client.json"

	back := e.consent(t, cookie, authorizeTarget(clientID))
	form := codeForm(clientID, back.Query().Get("code"))
	rec, _ := e.token(t, form)
	require.Equal(t, http.StatusOK, rec.Code)
	rec, _ = e.token(t, form)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "invalid_grant", tokenRefusal(t, rec))

	back = e.consent(t, cookie, authorizeTarget(clientID))
	e.clock.advance(oauthCodeTTL)
	rec, _ = e.token(t, codeForm(clientID, back.Query().Get("code")))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a code lives one minute")
	assert.Equal(t, 1, e.count(t, "mcp_connections"))

	rec, _ = e.token(t, url.Values{"grant_type": {"password"}})
	assert.Equal(t, "unsupported_grant_type", tokenRefusal(t, rec))
}

func TestOAuthRefreshRotates(t *testing.T) {
	e, base := newMCPEnv(t)
	clientID := base + "/client.json"
	first := e.connect(t, e.login(t), clientID)
	e.clock.advance(50 * time.Minute)

	rec, second := e.token(t, refreshForm(clientID, first.RefreshToken))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotEqual(t, first.AccessToken, second.AccessToken)
	assert.NotEqual(t, first.RefreshToken, second.RefreshToken)
	assert.Equal(t, scopeRead, second.Scope)

	var (
		access  []byte
		expires int64
	)
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT access_hash, expires_at FROM mcp_connections`).Scan(&access, &expires))
	assert.Equal(t, secure.TokenHash(second.AccessToken), access, "the first access token works no more")
	assert.Equal(t, e.clock.now().Add(30*24*time.Hour).Unix(), expires, "30 days from the last refresh")

	rec, _ = e.token(t, refreshForm(base+"/loopback.json", second.RefreshToken))
	assert.Equal(t, "invalid_grant", tokenRefusal(t, rec), "another client's refresh token")
	rec, _ = e.token(t, refreshForm(clientID, "not-a-token"))
	assert.Equal(t, "invalid_grant", tokenRefusal(t, rec))
	assert.Equal(t, 1, e.count(t, "mcp_connections"), "neither ends the connection")
}

func TestOAuthReplayedRefreshRevokesTheConnection(t *testing.T) {
	e, base := newMCPEnv(t)
	clientID := base + "/client.json"
	first := e.connect(t, e.login(t), clientID)

	rec, second := e.token(t, refreshForm(clientID, first.RefreshToken))
	require.Equal(t, http.StatusOK, rec.Code)
	rec, _ = e.token(t, refreshForm(clientID, first.RefreshToken))
	assert.Equal(t, "invalid_grant", tokenRefusal(t, rec))
	assert.Equal(t, 1, e.count(t, "mcp_connections"), "two refreshes that crossed: the connection stays")

	e.clock.advance(refreshLeeway)
	rec, _ = e.token(t, refreshForm(clientID, first.RefreshToken))
	assert.Equal(t, "invalid_grant", tokenRefusal(t, rec))
	assert.Zero(t, e.count(t, "mcp_connections"), "a replaced token that comes back late is a stolen one")
	rec, _ = e.token(t, refreshForm(clientID, second.RefreshToken))
	assert.Equal(t, "invalid_grant", tokenRefusal(t, rec), "the thief's tokens end with it")
}

func TestOAuthTokenEndpointIsRateLimited(t *testing.T) {
	e, _ := newMCPEnv(t)
	for range oauthTokenLimit {
		rec, _ := e.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}})
		require.Equal(t, http.StatusBadRequest, rec.Code)
	}
	rec, _ := e.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "3600", rec.Header().Get("Retry-After"))
}

// Clients read both documents from their servers, some under an AI robot's
// user agent: the robots refusal lets these routes through.
func TestOAuthMetadataDocuments(t *testing.T) {
	e, _ := newMCPEnv(t)
	robot := func(r *http.Request) { r.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Claude-User/1.0)") }
	origin := "https://" + adminHost

	var server map[string]any
	rec := e.do(t, http.MethodGet, adminHost, oauthServerMeta, nil, robot)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &server))
	assert.Equal(t, origin, server["issuer"])
	assert.Equal(t, origin+"/oauth/authorize", server["authorization_endpoint"])
	assert.Equal(t, origin+"/oauth/token", server["token_endpoint"])
	assert.Equal(t, []any{"S256"}, server["code_challenge_methods_supported"])
	assert.Equal(t, []any{"none"}, server["token_endpoint_auth_methods_supported"])
	assert.Equal(t, []any{"read"}, server["scopes_supported"])
	assert.Equal(t, true, server["client_id_metadata_document_supported"])
	assert.Equal(t, true, server["authorization_response_iss_parameter_supported"])
	assert.NotContains(t, server, "registration_endpoint")

	for _, path := range []string{oauthResourceMeta, oauthResourceMeta + "/mcp"} {
		var resource map[string]any
		rec := e.do(t, http.MethodGet, adminHost, path, nil, robot)
		require.Equal(t, http.StatusOK, rec.Code, path)
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resource))
		assert.Equal(t, mcpResource, resource["resource"], path)
		assert.Equal(t, []any{origin}, resource["authorization_servers"], path)
		assert.Equal(t, []any{"read"}, resource["scopes_supported"], path)
	}
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, publicHost, oauthServerMeta, nil).Code, "committee host only")
	assert.Equal(t, http.StatusForbidden, e.do(t, http.MethodGet, adminHost, "/connexion", nil, robot).Code, "the pages still refuse robots")
}

func TestTemporaryPasswordCannotAuthorize(t *testing.T) {
	e, base := newMCPEnv(t)
	clientID := base + "/client.json"
	back := e.consent(t, e.login(t), authorizeTarget(clientID))

	temporary, err := e.deps.Admins.ResetPassword(context.Background(), "alice")
	require.NoError(t, err)
	rec, _ := e.token(t, codeForm(clientID, back.Query().Get("code")))
	assert.Equal(t, "invalid_grant", tokenRefusal(t, rec), "a code granted before the reset")

	rec = e.postLogin(t, "alice", temporary)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	rec = e.do(t, http.MethodGet, adminHost, authorizeTarget(clientID), nil, withCookie(sessionCookie(t, rec)))
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, accountPath, rec.Header().Get("Location"))
	assert.Zero(t, e.count(t, "mcp_connections"))
}

// Without MCP_ENABLED none of it exists: 404, also for a program that sends
// no Origin, which the committee site would otherwise answer 403.
func TestOAuthGateOffAnswers404(t *testing.T) {
	e := newTestEnv(t)
	cookie := e.login(t)
	noOrigin := func(r *http.Request) { r.Header.Del("Origin") }
	for _, path := range []string{oauthAuthorizePath + "?client_id=x", oauthServerMeta, oauthResourceMeta, oauthResourceMeta + "/mcp", connectionsPath} {
		assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, adminHost, path, nil, withCookie(cookie)).Code, path)
	}
	for _, path := range []string{oauthTokenPath, mcpPath, oauthAuthorizePath} {
		assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodPost, adminHost, path, nil, noOrigin).Code, path)
	}
	rec := e.do(t, http.MethodGet, adminHost, accountPath, nil, withCookie(cookie))
	assert.NotContains(t, rec.Body.String(), connectionsPath)
}

func TestConnectionsPageListsAndRevokes(t *testing.T) {
	e, base := newMCPEnv(t)
	ctx := context.Background()
	cookie := e.login(t)
	clientID := base + "/client.json"

	rec := e.do(t, http.MethodGet, adminHost, accountPath, nil, withCookie(cookie))
	assert.Contains(t, rec.Body.String(), `href="/compte/connexions"`)
	rec = e.do(t, http.MethodGet, adminHost, connectionsPath, nil, withCookie(cookie))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), mcpResource)
	assert.Contains(t, rec.Body.String(), "Aucune application autorisée")

	tok := e.connect(t, cookie, clientID)
	require.NoError(t, e.deps.Admins.Insert(ctx, bob()))
	_, err := e.db.ExecContext(ctx,
		`INSERT INTO mcp_connections (username, credential_hash, client_id, client_name, scope, access_hash, access_expires_at, refresh_hash, created_at, last_used_at, expires_at)
		 VALUES ('bob', ?, 'https://chatgpt.com/oauth/client.json', 'ChatGPT de Bob', 'read', 'a', 0, 'r', 0, 0, 9999999999)`, bob().CredentialHash())
	require.NoError(t, err)

	rec = e.do(t, http.MethodGet, adminHost, connectionsPath, nil, withCookie(cookie))
	body := rec.Body.String()
	assert.Contains(t, body, "Claude")
	assert.Contains(t, body, "127.0.0.1 · Lecture seule")
	assert.NotContains(t, body, "ChatGPT de Bob", "an account sees its own connections")
	assert.NotContains(t, body, "renommer", "a connection cannot be renamed")

	var mine, his int64
	require.NoError(t, e.db.QueryRowContext(ctx, `SELECT id FROM mcp_connections WHERE username = 'alice'`).Scan(&mine))
	require.NoError(t, e.db.QueryRowContext(ctx, `SELECT id FROM mcp_connections WHERE username = 'bob'`).Scan(&his))
	csrf := e.csrf(t, cookie, connectionsPath)
	revoke := func(id int64) *httptest.ResponseRecorder {
		return e.postAs(t, cookie, connectionsPath+"/"+itoa(id)+"/revoquer", url.Values{"csrf": {csrf}})
	}
	require.Equal(t, http.StatusSeeOther, revoke(his).Code)
	assert.Equal(t, 2, e.count(t, "mcp_connections"), "nobody revokes another account's connection")
	assert.Equal(t, http.StatusForbidden, e.postAs(t, cookie, connectionsPath+"/"+itoa(mine)+"/revoquer", url.Values{"csrf": {"wrong"}}).Code)

	rec = revoke(mine)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, connectionsPath+"?revoquee=1", rec.Header().Get("Location"))
	assert.Equal(t, 1, e.count(t, "mcp_connections"))
	rec, _ = e.token(t, refreshForm(clientID, tok.RefreshToken))
	assert.Equal(t, "invalid_grant", tokenRefusal(t, rec), "its tokens work no more")
	rec = e.do(t, http.MethodGet, adminHost, connectionsPath+"?revoquee=1", nil, withCookie(cookie))
	assert.Contains(t, rec.Body.String(), "Accès retiré")
}

// A connection holds while the account keeps its password, as a session
// does, and for 30 days after its last refresh.
func TestConnectionsEndWithThePasswordAndTheAccount(t *testing.T) {
	ctx := context.Background()
	e, base := newMCPEnv(t)
	clientID := base + "/client.json"

	tok := e.connect(t, e.login(t), clientID)
	_, err := e.deps.Admins.ResetPassword(ctx, "alice")
	require.NoError(t, err)
	assert.Zero(t, e.count(t, "mcp_connections"), "a reset ends the connections")
	rec, _ := e.token(t, refreshForm(clientID, tok.RefreshToken))
	assert.Equal(t, "invalid_grant", tokenRefusal(t, rec))

	require.NoError(t, e.deps.Admins.Insert(ctx, bob()))
	insert := func(username string, credential []byte, mark string, expires int64) {
		_, err := e.db.ExecContext(ctx,
			`INSERT INTO mcp_connections (username, credential_hash, client_id, client_name, scope, access_hash, access_expires_at, refresh_hash, created_at, last_used_at, expires_at)
			 VALUES (?, ?, 'c', 'n', 'read', ?, 0, ?, 0, 0, ?)`, username, credential, "a"+mark, "r"+mark, expires)
		require.NoError(t, err)
	}
	insert("bob", bob().CredentialHash(), "kept", 9999999999)
	insert("bob", []byte("an older password hash"), "changed", 9999999999)
	insert("bob", bob().CredentialHash(), "expired", e.clock.now().Unix()-1)

	require.NoError(t, e.srv.RevokeStale(ctx))
	assert.Equal(t, 2, e.count(t, "mcp_connections"), "the one granted under another password is gone")
	require.NoError(t, e.srv.Purge(ctx))
	assert.Equal(t, 1, e.count(t, "mcp_connections"), "the one unused for 30 days is purged")
	require.NoError(t, e.deps.Admins.Delete(ctx, "bob"))
	assert.Zero(t, e.count(t, "mcp_connections"), "the account's connections go with it")
}
