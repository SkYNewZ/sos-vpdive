// Package ffessm signs in to the federation's club portal with the club
// account and searches its licence register. Queries, results and tokens are
// personal data: nothing here logs them.
package ffessm

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

const (
	clientID    = "mon_club_dev" // the portal's public OIDC client, as monclub.ffessm.fr uses it
	scope       = "openid profile api"
	appOrigin   = "https://monclub.ffessm.fr"
	redirectURI = appOrigin + "/index.html" // where the login chain stops
	bodyLimit   = 4 << 20
	maxResults  = 10
	minQuery    = 3 // the portal refuses shorter searches
	tokenMargin = time.Minute
	timeout     = 20 * time.Second
	maxRedirect = 10
	// licenseeType is the search row type that has a licence record.
	licenseeType = "Licencie"
)

var (
	// ErrBadCredentials reports that the identity server refused the club account.
	ErrBadCredentials = errors.New("ffessm: club account refused")
	// ErrQueryTooShort reports a search under three characters.
	ErrQueryTooShort = errors.New("ffessm: query too short")
	// ErrUnexpected reports a response the client does not understand.
	ErrUnexpected = errors.New("ffessm: unexpected response")
)

var verificationToken = regexp.MustCompile(`<input[^>]*name="__RequestVerificationToken"[^>]*value="([^"]+)"`)

// Licence is one register entry with the holder's contact details.
type Licence struct {
	Number     string // A-13-634166
	Name       string // last name then first name, as the register spells it
	BirthDate  string // ISO date
	ValidUntil string // ISO date
	Club       string
	Active     bool
	Instructor bool
	Email      string
	Mobile     string
	Phone      string
	Address    string // one line: street, postcode, town
}

// Client searches the register, signing in again when its token expires.
type Client struct {
	username, password string
	identity, api      string // base URLs
	http               *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// New returns a client, nil when no club account is configured.
func New(cfg *config.FFESSM) *Client {
	if cfg == nil {
		return nil
	}
	return &Client{
		username: cfg.Username, password: cfg.Password,
		identity: "https://identite.ffessm.fr", api: "https://api.ffessm.fr",
		// A redirect must never hand the bearer token to another host.
		http: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// Search finds licences by "last first" or "first last" name, licence
// number or email address, with each holder's contact details.
func (c *Client) Search(ctx context.Context, query string) ([]Licence, error) {
	query = strings.Join(strings.Fields(query), " ")
	if utf8.RuneCountInString(query) < minQuery {
		return nil, ErrQueryTooShort
	}
	var hits []hit
	var err error
	if strings.Contains(query, "@") {
		hits, err = c.byEmail(ctx, query)
	} else {
		hits, err = c.byText(ctx, query)
		if words := strings.Fields(query); err == nil && len(hits) == 0 && len(words) == 2 {
			hits, err = c.byText(ctx, words[1]+" "+words[0])
		}
	}
	if err != nil {
		return nil, err
	}
	hits = hits[:min(len(hits), maxResults)]
	licences := make([]Licence, 0, len(hits))
	for _, h := range hits {
		var d detail
		if err := c.call(ctx, "licensee detail", http.MethodGet, "/V1/Utilisateurs/Licencie/"+url.PathEscape(h.ID), nil, &d); err != nil {
			return nil, err
		}
		licences = append(licences, h.licence(d))
	}
	return licences, nil
}

// hit is one row of the name or the email search.
type hit struct {
	ID          string `json:"Id"`
	Type        string `json:"Type"`
	Number      string `json:"NumeroLicence"`
	Name        string `json:"Nom"`
	NameByEmail string `json:"NomPrenom"`
	BirthDate   string `json:"DateDeNaissance"`
	ValidUntil  string `json:"DateValiditeLicence"`
	Club        string `json:"NomStructure"`
	Active      bool   `json:"EstActif"`
	Instructor  bool   `json:"EstMoniteur"`
	Email       string `json:"Email"`
}

type detail struct {
	Address struct {
		Email  string `json:"Email"`
		Mobile string `json:"TelephonePortable"`
		Phone  string `json:"TelephoneFixe"`
		Line1  string `json:"Adresse1"`
		Line2  string `json:"Adresse2"`
		Line3  string `json:"Adresse3"`
		Code   string `json:"CodePostal"`
		Town   string `json:"Commune"`
	} `json:"Adresse"`
}

func (h hit) licence(d detail) Licence {
	a := d.Address
	return Licence{
		Number: h.Number, Name: cmp.Or(h.Name, h.NameByEmail),
		BirthDate: date(h.BirthDate), ValidUntil: date(h.ValidUntil),
		Club: h.Club, Active: h.Active, Instructor: h.Instructor,
		Email: cmp.Or(a.Email, h.Email), Mobile: a.Mobile, Phone: a.Phone,
		Address: join(", ", a.Line1, a.Line2, a.Line3, join(" ", a.Code, a.Town)),
	}
}

// date keeps the day of an ISO date or timestamp.
func date(s string) string {
	day, _, _ := strings.Cut(s, "T")
	return day
}

func join(sep string, parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

func (c *Client) byText(ctx context.Context, text string) ([]hit, error) {
	var page struct {
		Items []hit `json:"Items"`
	}
	err := c.call(ctx, "name search", http.MethodPost, "/V1/Utilisateurs/Utilisateur/Search", map[string]any{
		"SearchText": text, "PageIndex": 1, "PageSize": maxResults,
		"FiltreEstActif": 0, "FiltreTypeUtilisateurs": []int{1, 8}, "FiltreMoniteurs": 0,
	}, &page)
	if err != nil {
		return nil, err
	}
	licensees := page.Items[:0]
	for _, h := range page.Items {
		if h.Type == licenseeType { // structures and staff have no licensee record
			licensees = append(licensees, h)
		}
	}
	return licensees, nil
}

func (c *Client) byEmail(ctx context.Context, email string) ([]hit, error) {
	var hits []hit
	err := c.call(ctx, "email search", http.MethodPost, "/V1/Utilisateurs/Licencie/ListByEmail", map[string]any{
		"searchText": email, "page": 0, "pageSize": maxResults,
	}, &hits)
	return hits, err
}

// call sends one API request, signing in again once when the token is refused.
// Errors name the call, never its path: a path can carry a licensee's ID.
func (c *Client) call(ctx context.Context, name, method, path string, in, out any) error {
	for retried := false; ; retried = true {
		token, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		status, err := c.send(ctx, name, method, path, token, in, out)
		if status == http.StatusUnauthorized && !retried {
			c.forget(token)
			continue
		}
		return err
	}
}

func (c *Client) send(ctx context.Context, name, method, path, token string, in, out any) (status int, err error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("ffessm: encode request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, body)
	if err != nil {
		return 0, fmt.Errorf("ffessm: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", appOrigin)
	req.Header.Set("Referer", appOrigin+"/")
	resp, err := c.http.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok { // its message holds the full URL
			err = ue.Err
		}
		return 0, fmt.Errorf("ffessm: %s: %w", name, err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("%w: %s answered %d", ErrUnexpected, name, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, bodyLimit)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("%w: %s: %w", ErrUnexpected, name, err)
	}
	return resp.StatusCode, nil
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expiry) {
		return c.token, nil
	}
	token, lifetime, err := c.login(ctx)
	if err != nil {
		return "", err
	}
	c.token, c.expiry = token, time.Now().Add(lifetime-tokenMargin)
	return token, nil
}

func (c *Client) forget(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
	}
}

// login runs the portal's authorization code flow with PKCE: authorize,
// submit the login form, catch the code on the redirect URI, exchange it.
func (c *Client) login(ctx context.Context) (string, time.Duration, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", 0, fmt.Errorf("ffessm: cookie jar: %w", err)
	}
	identity, err := url.Parse(c.identity)
	if err != nil {
		return "", 0, fmt.Errorf("ffessm: identity URL: %w", err)
	}
	browser := &http.Client{Timeout: timeout, Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		switch {
		case strings.HasPrefix(req.URL.String(), redirectURI+"?"):
			return http.ErrUseLastResponse
		case req.URL.Host != identity.Host:
			return fmt.Errorf("%w: redirect to %s", ErrUnexpected, req.URL.Host)
		case len(via) >= maxRedirect:
			return fmt.Errorf("%w: too many redirects", ErrUnexpected)
		}
		return nil
	}}

	verifier, state, nonce := random(), random(), random()
	challenge := sha256.Sum256([]byte(verifier))
	authorize := c.identity + "/connect/authorize?" + url.Values{
		"client_id": {clientID}, "redirect_uri": {redirectURI}, "response_type": {"code"}, "scope": {scope},
		"state": {state}, "nonce": {nonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
	}.Encode()
	page, form, err := c.fetch(ctx, browser, http.MethodGet, authorize, nil)
	if err != nil {
		return "", 0, err
	}
	m := verificationToken.FindSubmatch(page)
	if m == nil {
		return "", 0, fmt.Errorf("%w: no login form", ErrUnexpected)
	}

	_, resp, err := c.fetch(ctx, browser, http.MethodPost, form.String(), url.Values{
		"ReturnUrl": {form.Query().Get("ReturnUrl")}, "Username": {c.username}, "Password": {c.password},
		"button": {"login"}, "RememberLogin": {"false"}, "__RequestVerificationToken": {string(m[1])},
	})
	if err != nil {
		return "", 0, err
	}
	if !strings.HasPrefix(resp.String(), redirectURI+"?") {
		if resp.Host == identity.Host && resp.Path == form.Path {
			return "", 0, ErrBadCredentials // the form came back with an error message
		}
		return "", 0, fmt.Errorf("%w: sign-in ended on %s", ErrUnexpected, resp.Path)
	}
	code := resp.Query().Get("code")
	if code == "" || resp.Query().Get("state") != state {
		return "", 0, fmt.Errorf("%w: callback without code or with another state", ErrUnexpected)
	}
	return c.exchange(ctx, code, verifier)
}

// fetch runs one sign-in step and returns the body and the address it ended
// at: the final page, or the redirect URI the chain stopped on.
func (c *Client) fetch(ctx context.Context, browser *http.Client, method, target string, form url.Values) (page []byte, at *url.URL, err error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, fmt.Errorf("ffessm: build login request: %w", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := browser.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("ffessm: login: %w", err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	page, err = io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return nil, nil, fmt.Errorf("ffessm: login: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return page, resp.Request.URL, nil
	case resp.StatusCode/100 == 3:
		if at, err = resp.Location(); err != nil {
			return nil, nil, fmt.Errorf("%w: redirect without location", ErrUnexpected)
		}
		return page, at, nil
	}
	return nil, nil, fmt.Errorf("%w: sign-in step answered %d", ErrUnexpected, resp.StatusCode)
}

func (c *Client) exchange(ctx context.Context, code, verifier string) (string, time.Duration, error) {
	page, _, err := c.fetch(ctx, c.http, http.MethodPost, c.identity+"/connect/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"client_id": {clientID}, "code_verifier": {verifier},
	})
	if err != nil {
		return "", 0, err
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(page, &token); err != nil || token.AccessToken == "" {
		return "", 0, fmt.Errorf("%w: token response", ErrUnexpected)
	}
	return token.AccessToken, time.Duration(token.ExpiresIn) * time.Second, nil
}

// random returns 52 unreserved characters, long enough for a PKCE verifier.
func random() string {
	return rand.Text() + rand.Text()
}
