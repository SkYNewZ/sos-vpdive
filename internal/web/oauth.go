package web

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// The committee site is its own OAuth 2.1 authorization server for the MCP
// server it hosts (docs/development.md « MCP Server »). Clients are public:
// they name themselves by the URL of a client id document and prove the
// code is theirs with PKCE.
const (
	mcpPath            = "/mcp"
	oauthAuthorizePath = "/oauth/authorize"
	oauthTokenPath     = "/oauth/token" //nolint:gosec // a route, not a credential
	oauthServerMeta    = "/.well-known/oauth-authorization-server"
	oauthResourceMeta  = "/.well-known/oauth-protected-resource"

	// responseCode is the only response type, and the parameter that
	// carries the code back to the client.
	responseCode = "code"

	// scopeRead is the only scope so far: every tool reads. A write scope
	// would join it here, in the metadata and on the consent screen.
	scopeRead = "read"

	accessTTL     = time.Hour
	connectionTTL = sessionTTL // of the refresh token, renewed at each use
	oauthCodeTTL  = time.Minute
	consentTTL    = 10 * time.Minute
	// refreshLeeway is how long the token a refresh replaced may come back
	// without ending the connection: two refreshes that crossed.
	refreshLeeway = time.Minute

	clientDocLimit   = 64 << 10
	clientDocTimeout = 5 * time.Second
	clientNameMax    = 80 // runes
	tokenBodyLimit   = 16 << 10
	oauthTokenLimit  = 60 // token calls per hour and client address
)

// adminOrigin is the committee site's origin, the issuer of its tokens.
func (s *Server) adminOrigin() string {
	return s.cfg.AdminBaseURL.Scheme + "://" + s.cfg.AdminBaseURL.Host
}

// mcpURL is the resource the tokens are for.
func (s *Server) mcpURL() string { return s.adminOrigin() + mcpPath }

// oauthRequest is an authorization request that passed validation, kept
// from the consent screen to the exchange of its code.
type oauthRequest struct {
	clientID    string
	clientName  string
	redirectURI string
	state       string
	challenge   string
	username    string // the account the consent screen was shown to
	expires     time.Time
}

// grantKind keeps a consent nonce from passing for a code.
type grantKind string

const (
	grantConsent grantKind = "consent:"
	grantCode    grantKind = "code:"
)

// oauthGrants holds the consents awaiting an answer and the codes awaiting
// their exchange. They live in memory: ten minutes and one minute on a
// single instance, and a restart only makes the member connect again.
type oauthGrants struct {
	mu   sync.Mutex
	now  func() time.Time
	held map[string]oauthRequest // by kind and token hash
}

func newOAuthGrants(now func() time.Time) *oauthGrants {
	return &oauthGrants{now: now, held: map[string]oauthRequest{}}
}

// put holds req for ttl and returns the token that names it.
func (g *oauthGrants) put(kind grantKind, req oauthRequest, ttl time.Duration) (string, error) {
	token, err := secure.NewToken()
	if err != nil {
		return "", err
	}
	now := g.now()
	req.expires = now.Add(ttl)
	g.mu.Lock()
	defer g.mu.Unlock()
	maps.DeleteFunc(g.held, func(_ string, r oauthRequest) bool { return !r.expires.After(now) })
	g.held[string(kind)+string(secure.TokenHash(token))] = req
	return token, nil
}

// take returns the request token names, once.
func (g *oauthGrants) take(kind grantKind, token string) (oauthRequest, bool) {
	key := string(kind) + string(secure.TokenHash(token))
	g.mu.Lock()
	defer g.mu.Unlock()
	req, ok := g.held[key]
	delete(g.held, key)
	return req, ok && req.expires.After(g.now())
}

// oauthClient is what a client id document says of its client.
type oauthClient struct {
	name      string
	redirects []string
}

// errOAuthClient reports a client id the server will not deal with.
var errOAuthClient = errors.New("oauth client refused")

// newOAuthHTTP fetches client id documents: no redirect is followed, so a
// document cannot send the server anywhere else.
func newOAuthHTTP() *http.Client {
	return &http.Client{
		Timeout:       clientDocTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// fetchClient reads the client id document at clientID (draft-ietf-oauth-
// client-id-metadata-document): an https URL on a host MCP_CLIENT_HOSTS
// allows, which is the server's whole defence against being sent to fetch
// an address of its own network.
func (s *Server) fetchClient(ctx context.Context, clientID string) (_ oauthClient, err error) {
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || strings.Trim(u.Path, "/") == "" ||
		!slices.Contains(s.cfg.MCP.ClientHosts, strings.ToLower(u.Hostname())) {
		return oauthClient{}, fmt.Errorf("%w: not a document on an allowed host", errOAuthClient)
	}
	ctx, cancel := context.WithTimeout(ctx, clientDocTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return oauthClient{}, fmt.Errorf("%w: %w", errOAuthClient, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.oauthHTTP.Do(req)
	if err != nil {
		return oauthClient{}, fmt.Errorf("%w: fetch document: %w", errOAuthClient, err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode != http.StatusOK {
		return oauthClient{}, fmt.Errorf("%w: document answered %d", errOAuthClient, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, clientDocLimit+1))
	if err != nil || len(body) > clientDocLimit {
		return oauthClient{}, fmt.Errorf("%w: document unreadable or over %d bytes", errOAuthClient, clientDocLimit)
	}
	var doc struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.ClientID != clientID || len(doc.RedirectURIs) == 0 {
		return oauthClient{}, fmt.Errorf("%w: document does not describe this client", errOAuthClient)
	}
	name := cmp.Or(strings.TrimSpace(doc.ClientName), u.Hostname())
	if utf8.RuneCountInString(name) > clientNameMax {
		name = string([]rune(name)[:clientNameMax])
	}
	return oauthClient{name: name, redirects: doc.RedirectURIs}, nil
}

// redirectAllowed reports whether got is one of the client's redirect URIs,
// character for character. A loopback URI matches whatever its port: a
// desktop client listens on one it picks at each sign-in (RFC 8252 §7.3).
func redirectAllowed(allowed []string, got string) bool {
	if slices.Contains(allowed, got) {
		return true
	}
	g, ok := loopback(got)
	return ok && slices.ContainsFunc(allowed, func(a string) bool {
		l, ok := loopback(a)
		return ok && l == g
	})
}

// loopback returns raw without its port when raw is an http URI on the
// loopback interface.
func loopback(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
		return "", false
	}
	u.Host = u.Hostname()
	return u.String(), true
}

// consentData feeds templates/autorisation.html: the consent screen, the
// way back to the client once the member answered, or why it stops here.
type consentData struct {
	Client  string
	Host    string // of the client id document
	Demande string // nonce of the request, posted back with the answer
	Retour  string // the client's redirect URI, with the answer
	Refus   string
}

// oauthAuthorize shows the consent screen of an authorization request. A
// request whose client or redirect URI is wrong stops on a page: nothing is
// ever sent to an address the client did not publish.
func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionOf(r)
	if !ok {
		http.Redirect(w, r, "/connexion?suite="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	if sess.account.MustChangePassword {
		http.Redirect(w, r, accountPath, http.StatusSeeOther)
		return
	}
	ctx := context.WithValue(r.Context(), ctxSession, sess)
	r = r.WithContext(ctx)
	q := r.URL.Query()
	client, err := s.fetchClient(ctx, q.Get("client_id"))
	if err != nil {
		s.logger.WarnContext(ctx, "oauth client refused", "error", err)
		s.renderConsent(w, r, http.StatusBadRequest, consentData{Refus: "Cette application n'est pas reconnue. Si elle devrait l'être, préviens la personne qui gère l'outil."})
		return
	}
	req := oauthRequest{
		clientID: q.Get("client_id"), clientName: client.name, redirectURI: q.Get("redirect_uri"),
		state: q.Get("state"), challenge: q.Get("code_challenge"), username: sess.account.Username,
	}
	if !redirectAllowed(client.redirects, req.redirectURI) {
		s.logger.WarnContext(ctx, "oauth redirect uri refused")
		s.renderConsent(w, r, http.StatusBadRequest, consentData{Refus: client.name + " a demandé une adresse de retour qu'elle n'a pas déclarée. La connexion s'arrête là."})
		return
	}
	if code := s.oauthInvalid(q); code != "" {
		http.Redirect(w, r, s.oauthAnswer(req, url.Values{"error": {code}}), http.StatusSeeOther)
		return
	}
	nonce, err := s.grants.put(grantConsent, req, consentTTL)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	host := req.clientID
	if u, err := url.Parse(req.clientID); err == nil {
		host = u.Hostname()
	}
	s.renderConsent(w, r, http.StatusOK, consentData{Client: client.name, Host: host, Demande: nonce})
}

// oauthInvalid is the error code of a request the client got wrong, "" for
// a sound one. The scope asked is not read: read is the only one to grant.
func (s *Server) oauthInvalid(q url.Values) string {
	switch {
	case q.Get("response_type") != responseCode:
		return "unsupported_response_type"
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		return "invalid_request"
	case q.Has("resource") && q.Get("resource") != s.mcpURL():
		return "invalid_target"
	}
	return ""
}

// oauthAnswer is req's redirect URI carrying params, the state and the
// issuer (RFC 9207).
func (s *Server) oauthAnswer(req oauthRequest, params url.Values) string {
	u, err := url.Parse(req.redirectURI)
	if err != nil { // redirectAllowed matched it against the client's document
		return req.redirectURI
	}
	q := u.Query()
	maps.Copy(q, params)
	if req.state != "" {
		q.Set("state", req.state)
	}
	q.Set("iss", s.adminOrigin())
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) renderConsent(w http.ResponseWriter, r *http.Request, status int, d consentData) {
	p := s.newPage(r, "Autoriser une application")
	p.Bare = true // one question, no navigation around it
	p.Data = d
	s.render(w, r, status, "autorisation", p)
}

// oauthDecide records the member's answer and sends them back to the
// client. The way back is a page that redirects, not a 303: the committee's
// policy lets a form lead nowhere but to this site (form-action 'self'), and
// Chrome applies it to the redirects that follow a form too.
func (s *Server) oauthDecide(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	req, ok := s.grants.take(grantConsent, r.PostForm.Get("demande"))
	if !ok || req.username != sess.account.Username {
		s.renderConsent(w, r, http.StatusBadRequest, consentData{Refus: "Cette demande a expiré. Relance la connexion depuis l'application."})
		return
	}
	params := url.Values{"error": {"access_denied"}}
	if r.PostForm.Get("action") == "autoriser" {
		code, err := s.grants.put(grantCode, req, oauthCodeTTL)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		params = url.Values{responseCode: {code}}
	}
	s.renderConsent(w, r, http.StatusOK, consentData{Client: req.clientName, Retour: s.oauthAnswer(req, params)})
}

// tokenResponse is a successful answer of the token endpoint (RFC 6749 §5.1).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// oauthFailure is a refusal of the token endpoint (RFC 6749 §5.2).
type oauthFailure struct {
	Error string `json:"error"`
}

// errInvalidGrant refuses a code or a refresh token: unknown, used, expired,
// or not this client's.
var errInvalidGrant = errors.New("invalid grant")

// oauthToken exchanges a code for tokens, or a refresh token for new ones.
// Clients call it from their servers: no session, no Origin.
func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ok, err := s.limiter.allow(ctx, "oauth-token:"+s.clientIP(r).String(), oauthTokenLimit, time.Hour)
	if err != nil {
		s.oauthTokenError(w, r, err)
		return
	}
	if !ok {
		w.Header().Set("Retry-After", "3600")
		s.writeJSON(w, r, http.StatusTooManyRequests, oauthFailure{Error: "rate_limited"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, tokenBodyLimit)
	if err := r.ParseForm(); err != nil {
		s.writeJSON(w, r, http.StatusBadRequest, oauthFailure{Error: "invalid_request"})
		return
	}
	var pair tokenPair
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		pair, err = s.exchangeCode(ctx, r.PostForm)
	case "refresh_token":
		pair, err = s.rotateConnection(ctx, r.PostForm.Get("refresh_token"), r.PostForm.Get("client_id"))
	default:
		s.writeJSON(w, r, http.StatusBadRequest, oauthFailure{Error: "unsupported_grant_type"})
		return
	}
	switch {
	case errors.Is(err, errInvalidGrant):
		s.logger.WarnContext(ctx, "oauth grant refused")
		s.writeJSON(w, r, http.StatusBadRequest, oauthFailure{Error: "invalid_grant"})
	case err != nil:
		s.oauthTokenError(w, r, err)
	default:
		s.writeJSON(w, r, http.StatusOK, tokenResponse{AccessToken: pair.access, TokenType: "Bearer",
			ExpiresIn: int(accessTTL / time.Second), RefreshToken: pair.refresh, Scope: scopeRead})
	}
}

// oauthTokenError answers a failure of ours in the endpoint's JSON.
func (s *Server) oauthTokenError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		s.logger.WarnContext(r.Context(), "client left", "error", err)
		return
	}
	s.logger.ErrorContext(r.Context(), "internal error", "error", err)
	s.writeJSON(w, r, http.StatusInternalServerError, oauthFailure{Error: "server_error"})
}

// exchangeCode opens a connection for the code of form: once, for the
// client it was given to, on proof of its PKCE verifier.
func (s *Server) exchangeCode(ctx context.Context, form url.Values) (tokenPair, error) {
	req, ok := s.grants.take(grantCode, form.Get("code"))
	if !ok || req.clientID != form.Get("client_id") || !pkceMatches(req.challenge, form.Get("code_verifier")) ||
		(form.Has("redirect_uri") && form.Get("redirect_uri") != req.redirectURI) ||
		(form.Has("resource") && form.Get("resource") != s.mcpURL()) {
		return tokenPair{}, errInvalidGrant
	}
	a, ok := s.admins.Get(req.username)
	if !ok || a.MustChangePassword {
		return tokenPair{}, errInvalidGrant
	}
	return s.createConnection(ctx, a, req)
}

// pkceMatches checks verifier against the S256 challenge of the request
// (RFC 7636 §4.6).
func pkceMatches(challenge, verifier string) bool {
	sum := sha256.Sum256([]byte(verifier))
	return verifier != "" && subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

// oauthServerMetadata describes the authorization server (RFC 8414).
func (s *Server) oauthServerMetadata(w http.ResponseWriter, r *http.Request) {
	origin := s.adminOrigin()
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"issuer":                                         origin,
		"authorization_endpoint":                         origin + oauthAuthorizePath,
		"token_endpoint":                                 origin + oauthTokenPath,
		"response_types_supported":                       []string{responseCode},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"scopes_supported":                               []string{scopeRead},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

// oauthResourceMetadata tells a client where to get a token for the MCP
// server (RFC 9728).
func (s *Server) oauthResourceMetadata(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"resource":                 s.mcpURL(),
		"authorization_servers":    []string{s.adminOrigin()},
		"scopes_supported":         []string{scopeRead},
		"bearer_methods_supported": []string{"header"},
	})
}

// oauthReturn is where a sign-in started by an authorization request goes
// back to: raw when it is that request on this site, else "".
func (s *Server) oauthReturn(raw string) string {
	if s.cfg.MCP == nil || !strings.HasPrefix(raw, oauthAuthorizePath+"?") {
		return ""
	}
	if u, err := url.Parse(raw); err != nil || u.Scheme != "" || u.Host != "" || u.Path != oauthAuthorizePath {
		return ""
	}
	return raw
}
