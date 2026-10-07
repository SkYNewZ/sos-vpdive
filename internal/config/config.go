// Package config reads the deployment configuration from environment
// variables (spec §10). Business content lives in embedded files instead.
package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// Env is the deployment environment.
type Env string

// Deployment environments. Development allows running without Turnstile and S3.
const (
	EnvProduction  Env = "production"
	EnvDevelopment Env = "development"
)

// SMTPTLS is the SMTP encryption mode; both encrypt before authenticating.
type SMTPTLS string

// SMTP encryption modes (spec §6).
const (
	SMTPImplicit SMTPTLS = "implicit"
	SMTPStartTLS SMTPTLS = "starttls"
)

// ErrMissing reports a required variable that is unset or empty.
var ErrMissing = errors.New("required variable is missing")

const secretKeySize = 32

// defaultPushHosts are the push services of Chrome, Safari, Firefox and Edge.
// A leading dot accepts any subdomain.
const defaultPushHosts = "fcm.googleapis.com,web.push.apple.com,updates.push.services.mozilla.com,.notify.windows.com"

var (
	// pushoverTokenPattern is the shape of Pushover application tokens.
	pushoverTokenPattern = regexp.MustCompile(`^[A-Za-z0-9]{30}$`)
	// hostPattern is a lowercase DNS name, optionally led by a dot.
	hostPattern = regexp.MustCompile(`^\.?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	// uuidPattern is the shape of an Umami website ID, lowercased.
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// SMTP holds the mail relay settings.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	TLS      SMTPTLS
}

// S3 holds the screenshot storage settings, required in production.
type S3 struct {
	Endpoint        *url.URL
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	Region          string
}

// LLM holds the model provider settings (spec §5.4).
type LLM struct {
	BaseURL    *url.URL // the API root; the client appends /v1/messages
	APIKey     string
	Model      string
	Timeout    time.Duration // the request leaves without suggestions past it
	DailyLimit int           // model calls per day, Europe/Paris
}

// VAPID identifies the server to the push services (RFC 8292).
type VAPID struct {
	PrivateKey *ecdsa.PrivateKey
	PublicKey  string // base64url uncompressed point, as browsers take it
	Subject    string // mailto: or https: contact
}

// Umami selects the page-view counter of spec §9.10. A site whose ID is
// empty is not measured.
type Umami struct {
	ScriptURL      *url.URL
	WebsiteID      string // members site
	AdminWebsiteID string // committee site
}

// Origin is the scheme and host serving the script and receiving page views.
func (u *Umami) Origin() string {
	return u.ScriptURL.Scheme + "://" + u.ScriptURL.Host
}

// Config is the validated deployment configuration.
type Config struct {
	Env                Env
	BaseURL            *url.URL
	AdminBaseURL       *url.URL
	VPDiveBaseURL      *url.URL
	SecretKey          []byte
	Port               int
	DataDir            string
	Owner              string // OWNER_USERNAME: the account that manages the others (spec §4.1 as amended)
	TrustedProxies     []netip.Prefix
	LogLevel           slog.Level
	SMTP               SMTP
	MailFrom           *mail.Address
	NotifyEmail        *mail.Address
	S3                 *S3
	TurnstileSiteKey   string
	TurnstileSecretKey string
	MembersMaxAge      time.Duration
	PaymentsMaxAge     time.Duration
	VPayDiveMaxAge     time.Duration
	ImportToken        string        // turns on POST /api/imports/{type} (spec §7.6); "" when unset
	AgeWarnAfter       time.Duration // open request shown in orange from this age
	AgeAlertAfter      time.Duration // and in red from this one
	RetentionDays      int           // days a closed request is kept
	FormRateLimit      int           // form submissions per hour and IP address
	LLM                *LLM          // nil without LLM_API_KEY: no suggestions, no screen 2
	PushoverToken      string        // "" turns Pushover off; each resolver sets a user key on « Notifications »
	VAPID              *VAPID        // nil turns Web Push off
	PushAllowedHosts   []string      // push services a subscription may point at
	SentryDSN          string        // "" turns Sentry off; always "" in development
	SentryEnvironment  string
	Umami              *Umami  // nil turns Umami off
	Warnings           []error // optional tools turned off by an invalid value
}

// TurnstileEnabled reports whether the anti-bot check is configured.
func (c *Config) TurnstileEnabled() bool {
	return c.TurnstileSiteKey != ""
}

// Load reads and validates every variable. It reports all problems at once,
// each prefixed with the name of its variable.
func Load(getenv func(string) string) (*Config, error) {
	p := &parser{getenv: getenv}
	c := &Config{
		Env:            p.env(),
		SecretKey:      p.secretKey(),
		Port:           p.int("PORT", "8080", 1, 65535),
		DataDir:        p.optional("DATA_DIR", "/data"),
		TrustedProxies: p.prefixes("TRUSTED_PROXIES"),
		LogLevel:       p.level("LOG_LEVEL"),
		SMTP: SMTP{
			Host:     p.required("SMTP_HOST"),
			Port:     p.int("SMTP_PORT", "", 1, 65535),
			Username: p.required("SMTP_USERNAME"),
			Password: p.required("SMTP_PASSWORD"),
			TLS:      p.smtpTLS(),
		},
		MailFrom:           p.address("MAIL_FROM"),
		NotifyEmail:        p.address("NOTIFY_EMAIL"),
		TurnstileSiteKey:   p.optional("TURNSTILE_SITE_KEY", ""),
		TurnstileSecretKey: p.optional("TURNSTILE_SECRET_KEY", ""),
		MembersMaxAge:      p.duration("MEMBERS_MAX_AGE", "336h"),
		PaymentsMaxAge:     p.duration("PAYMENTS_MAX_AGE", "168h"),
		VPayDiveMaxAge:     p.duration("VPAYDIVE_MAX_AGE", "168h"),
		AgeWarnAfter:       p.duration("AGE_WARN_AFTER", "48h"),
		AgeAlertAfter:      p.duration("AGE_ALERT_AFTER", "168h"),
		RetentionDays:      p.int("RETENTION_DAYS", "365", 15, 3650),
		FormRateLimit:      p.perHour("FORM_RATE_LIMIT", "20/h", 1, 10000),
		VPDiveBaseURL:      p.url("VPDIVE_BASE_URL", p.optional("VPDIVE_BASE_URL", "https://plongee-pradet.fr")),
	}
	c.BaseURL = p.url("BASE_URL", p.required("BASE_URL"))
	c.AdminBaseURL = p.url("ADMIN_BASE_URL", p.required("ADMIN_BASE_URL"))
	if c.BaseURL != nil && c.AdminBaseURL != nil && strings.EqualFold(c.BaseURL.Hostname(), c.AdminBaseURL.Hostname()) {
		p.fail("ADMIN_BASE_URL", errors.New("must use another host than BASE_URL"))
	}
	if c.AgeWarnAfter > 0 && c.AgeAlertAfter > 0 && c.AgeAlertAfter <= c.AgeWarnAfter {
		p.fail("AGE_ALERT_AFTER", errors.New("must be longer than AGE_WARN_AFTER"))
	}
	p.turnstile(c)
	c.S3 = p.s3(c.Env)
	c.LLM = p.llm(c.Env)
	c.PushoverToken = p.pushoverToken()
	c.VAPID = p.vapid()
	c.PushAllowedHosts = p.hosts("PUSH_ALLOWED_HOSTS", defaultPushHosts)
	c.SentryEnvironment = p.optional("SENTRY_ENVIRONMENT", string(c.Env))
	c.Umami = p.umami(c)
	c.ImportToken = p.importToken()
	c.Owner = p.owner(c.Env)
	if c.Env == EnvProduction {
		p.requireHTTPS("BASE_URL", c.BaseURL)
		p.requireHTTPS("ADMIN_BASE_URL", c.AdminBaseURL)
		c.SentryDSN = p.sentryDSN() // Sentry is fully off in development (owner decision)
	}
	if err := errors.Join(p.errs...); err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}
	c.Warnings = p.warnings
	return c, nil
}

type parser struct {
	getenv   func(string) string
	errs     []error
	warnings []error
}

func (p *parser) fail(name string, err error) {
	p.errs = append(p.errs, fmt.Errorf("%s: %w", name, err))
}

// warn records a value that turns an optional tool off without stopping
// the server.
func (p *parser) warn(name string, err error) {
	p.warnings = append(p.warnings, fmt.Errorf("%s: %w", name, err))
}

func (p *parser) value(name string) string {
	return strings.TrimSpace(p.getenv(name))
}

func (p *parser) required(name string) string {
	v := p.value(name)
	if v == "" {
		p.fail(name, ErrMissing)
	}
	return v
}

func (p *parser) optional(name, def string) string {
	if v := p.value(name); v != "" {
		return v
	}
	return def
}

func (p *parser) env() Env {
	switch e := Env(p.optional("APP_ENV", string(EnvProduction))); e {
	case EnvProduction, EnvDevelopment:
		return e
	default:
		p.fail("APP_ENV", fmt.Errorf("must be %q or %q", EnvProduction, EnvDevelopment))
		return EnvProduction
	}
}

func (p *parser) secretKey() []byte {
	raw := p.required("SECRET_KEY")
	if raw == "" {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != secretKeySize {
		p.fail("SECRET_KEY", errors.New("must be 32 bytes in standard base64 (openssl rand -base64 32)"))
		return nil
	}
	return key
}

func (p *parser) int(name, def string, minimum, maximum int) int {
	raw := p.optional(name, def)
	if raw == "" {
		p.fail(name, ErrMissing)
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < minimum || n > maximum {
		p.fail(name, fmt.Errorf("must be an integer between %d and %d", minimum, maximum))
		return 0
	}
	return n
}

// owner reads OWNER_USERNAME, required in production (spec §10 as amended).
func (p *parser) owner(env Env) string {
	v := p.value("OWNER_USERNAME")
	switch {
	case v == "" && env == EnvProduction:
		p.fail("OWNER_USERNAME", ErrMissing)
	case v != "" && !admins.ValidUsername(v):
		p.fail("OWNER_USERNAME", errors.New("must be 1 to 32 characters among a-z, 0-9, '.', '_' and '-', other than '.' and '..'"))
	}
	return v
}

// minImportToken is the shortest IMPORT_TOKEN accepted (spec §10).
const minImportToken = 32

// importToken reads IMPORT_TOKEN, never echoing it in an error.
func (p *parser) importToken() string {
	token := p.value("IMPORT_TOKEN")
	if token != "" && len(token) < minImportToken {
		p.fail("IMPORT_TOKEN", fmt.Errorf("must hold %d characters at least (openssl rand -base64 32)", minImportToken))
		return ""
	}
	return token
}

func (p *parser) duration(name, def string) time.Duration {
	d, err := time.ParseDuration(p.optional(name, def))
	if err != nil || d <= 0 {
		p.fail(name, errors.New("must be a positive Go duration such as 336h"))
		return 0
	}
	return d
}

// perHour reads a rate written "<n>/h", such as 20/h.
func (p *parser) perHour(name, def string, minimum, maximum int) int {
	count, ok := strings.CutSuffix(p.optional(name, def), "/h")
	n, err := strconv.Atoi(strings.TrimSpace(count))
	if !ok || err != nil || n < minimum || n > maximum {
		p.fail(name, fmt.Errorf("must be a rate per hour such as 20/h, between %d/h and %d/h", minimum, maximum))
		return 0
	}
	return n
}

func (p *parser) level(name string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(p.optional(name, "info"))); err != nil {
		p.fail(name, errors.New("must be debug, info, warn or error"))
	}
	return l
}

func (p *parser) address(name string) *mail.Address {
	raw := p.required(name)
	if raw == "" {
		return nil
	}
	a, err := mail.ParseAddress(raw)
	if err != nil {
		p.fail(name, errors.New("must be a mail address"))
		return nil
	}
	return a
}

// absolute parses an absolute http(s) URL without query, fragment or user.
func absolute(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	return u, err == nil && (u.Scheme == schemeHTTP || u.Scheme == schemeHTTPS) && u.Host != "" &&
		u.RawQuery == "" && u.Fragment == "" && u.User == nil
}

func (p *parser) url(name, raw string) *url.URL {
	if raw == "" {
		return nil
	}
	u, ok := absolute(raw)
	if !ok || (u.Path != "" && u.Path != "/") {
		p.fail(name, errors.New("must be an absolute http(s) URL without path"))
		return nil
	}
	u.Path = ""
	// Browsers omit a scheme's default port from Origin, which is compared as
	// an exact string.
	defaultPort := map[string]string{schemeHTTP: ":80", schemeHTTPS: ":443"}[u.Scheme]
	u.Host = strings.TrimSuffix(strings.ToLower(u.Host), defaultPort)
	return u
}

// endpoint reads an absolute http(s) URL that may carry a path: a provider
// can serve the Messages API under a prefix, such as /anthropic.
func (p *parser) endpoint(name, raw string) *url.URL {
	u, ok := absolute(raw)
	if !ok {
		p.fail(name, errors.New("must be an absolute http(s) URL without query"))
		return nil
	}
	return u
}

func (p *parser) requireHTTPS(name string, u *url.URL) {
	if u != nil && u.Scheme != schemeHTTPS {
		p.fail(name, errors.New("must use https in production"))
	}
}

func (p *parser) prefixes(name string) []netip.Prefix {
	raw := p.value(name)
	if raw == "" {
		return nil
	}
	var out []netip.Prefix
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if pfx, err := netip.ParsePrefix(part); err == nil {
			out = append(out, pfx.Masked())
			continue
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			p.fail(name, fmt.Errorf("%q is neither an address nor a CIDR range", part))
			continue
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out
}

func (p *parser) smtpTLS() SMTPTLS {
	switch m := SMTPTLS(p.optional("SMTP_TLS", string(SMTPImplicit))); m {
	case SMTPImplicit, SMTPStartTLS:
		return m
	default:
		p.fail("SMTP_TLS", fmt.Errorf("must be %q or %q", SMTPImplicit, SMTPStartTLS))
		return SMTPImplicit
	}
}

func (p *parser) turnstile(c *Config) {
	site, secret := c.TurnstileSiteKey != "", c.TurnstileSecretKey != ""
	switch {
	case site && !secret:
		p.fail("TURNSTILE_SECRET_KEY", ErrMissing)
	case secret && !site:
		p.fail("TURNSTILE_SITE_KEY", ErrMissing)
	case !site && c.Env == EnvProduction:
		p.fail("TURNSTILE_SITE_KEY", fmt.Errorf("%w (only APP_ENV=development runs without Turnstile)", ErrMissing))
		p.fail("TURNSTILE_SECRET_KEY", ErrMissing)
	}
}

// llm reads the model variables. They are checked even without a key, so a
// typo shows before the key is added; without a key suggestions are off.
func (p *parser) llm(env Env) *LLM {
	l := &LLM{
		BaseURL:    p.endpoint("LLM_BASE_URL", p.optional("LLM_BASE_URL", "https://api.anthropic.com")),
		APIKey:     p.value("LLM_API_KEY"),
		Model:      p.optional("LLM_MODEL", "claude-haiku-4-5-20251001"),
		Timeout:    p.duration("LLM_TIMEOUT", "8s"),
		DailyLimit: p.int("LLM_DAILY_LIMIT", "200", 1, 100000),
	}
	if l.Timeout > time.Minute {
		p.fail("LLM_TIMEOUT", errors.New("must be 60s at most"))
	}
	if env == EnvProduction {
		p.requireHTTPS("LLM_BASE_URL", l.BaseURL)
	}
	if l.APIKey == "" {
		return nil
	}
	return l
}

func (p *parser) s3(env Env) *S3 {
	names := []string{"S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"}
	anySet := false
	for _, n := range names {
		anySet = anySet || p.value(n) != ""
	}
	if !anySet && env != EnvProduction {
		return nil
	}
	return &S3{
		Endpoint:        p.url("S3_ENDPOINT", p.required("S3_ENDPOINT")),
		Bucket:          p.required("S3_BUCKET"),
		AccessKeyID:     p.required("S3_ACCESS_KEY_ID"),
		SecretAccessKey: p.required("S3_SECRET_ACCESS_KEY"),
		Region:          p.optional("S3_REGION", "auto"),
	}
}

func (p *parser) pushoverToken() string {
	token := p.value("PUSHOVER_APP_TOKEN")
	if token != "" && !pushoverTokenPattern.MatchString(token) {
		p.fail("PUSHOVER_APP_TOKEN", errors.New("must be the 30 letters and digits of a Pushover application token"))
		return ""
	}
	return token
}

// vapid reads the three VAPID variables: all of them, or none (spec §10).
func (p *parser) vapid() *VAPID {
	if p.value("VAPID_PUBLIC_KEY")+p.value("VAPID_PRIVATE_KEY")+p.value("VAPID_SUBJECT") == "" {
		return nil
	}
	public, private, subject := p.required("VAPID_PUBLIC_KEY"), p.required("VAPID_PRIVATE_KEY"), p.required("VAPID_SUBJECT")
	if public == "" || private == "" || subject == "" {
		return nil
	}
	v := &VAPID{Subject: p.vapidSubject(subject)}
	var want []byte
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(private, "="))
	if err == nil {
		v.PrivateKey, err = ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	}
	if err == nil {
		want, err = v.PrivateKey.PublicKey.Bytes()
	}
	if err != nil {
		p.fail("VAPID_PRIVATE_KEY", errors.New("must be a P-256 private key in base64url (sos-vpdive vapid-keys)"))
		return nil
	}
	got, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(public, "="))
	if err != nil || !bytes.Equal(got, want) {
		p.fail("VAPID_PUBLIC_KEY", errors.New("must be the public key of VAPID_PRIVATE_KEY (sos-vpdive vapid-keys)"))
		return nil
	}
	v.PublicKey = base64.RawURLEncoding.EncodeToString(want)
	if v.Subject == "" {
		return nil
	}
	return v
}

// vapidSubject accepts a mailto: address or an https: URL (RFC 8292). The
// address is bare: Apple refuses a token whose subject reads
// mailto:<club@example.org> (403 BadJwtToken).
func (p *parser) vapidSubject(raw string) string {
	if addr, ok := strings.CutPrefix(raw, "mailto:"); ok {
		if a, err := mail.ParseAddress(addr); err == nil && a.Address == addr {
			return raw
		}
	} else if u, ok := absolute(raw); ok && u.Scheme == schemeHTTPS {
		return raw
	}
	p.fail("VAPID_SUBJECT", errors.New("must be a mailto: address or an https: URL"))
	return ""
}

// hosts reads a comma-separated list of host names.
func (p *parser) hosts(name, def string) []string {
	var out []string
	for part := range strings.SplitSeq(p.optional(name, def), ",") {
		h := strings.ToLower(strings.TrimSpace(part))
		if h == "" {
			continue
		}
		if !hostPattern.MatchString(h) {
			p.fail(name, fmt.Errorf("%q is not a host name", h))
			continue
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		p.fail(name, errors.New("must name at least one host"))
	}
	return out
}

// umami reads the optional page-view counter. An invalid value turns it off,
// or leaves one site unmeasured, with a warning: the service runs fine
// without it (owner decision, 2026-10-06).
func (p *parser) umami(c *Config) *Umami {
	raw := p.value("UMAMI_SCRIPT_URL")
	if raw == "" {
		return nil
	}
	u, ok := absolute(raw)
	if !ok || u.Path == "" || u.Path == "/" || (c.Env == EnvProduction && u.Scheme != schemeHTTPS) {
		p.warn("UMAMI_SCRIPT_URL", errors.New("must be the absolute URL of the script, https in production: Umami is off"))
		return nil
	}
	// Referrer-Policy same-origin sends the full address, a tracking token
	// included, to the site's own origin: Umami must live elsewhere.
	u.Host = strings.TrimSuffix(strings.ToLower(u.Host), map[string]string{schemeHTTP: ":80", schemeHTTPS: ":443"}[u.Scheme])
	for _, site := range []*url.URL{c.BaseURL, c.AdminBaseURL} {
		if site != nil && site.Scheme == u.Scheme && site.Host == u.Host {
			p.warn("UMAMI_SCRIPT_URL", errors.New("must be on another origin than both sites, which send it their full addresses: Umami is off"))
			return nil
		}
	}
	m := &Umami{
		ScriptURL:      u,
		WebsiteID:      p.websiteID("UMAMI_WEBSITE_ID"),
		AdminWebsiteID: p.websiteID("UMAMI_ADMIN_WEBSITE_ID"),
	}
	if m.WebsiteID == "" && m.AdminWebsiteID == "" {
		p.warn("UMAMI_SCRIPT_URL", errors.New("set without a valid website ID: Umami is off"))
		return nil
	}
	return m
}

// sentryDSN reads the optional Sentry DSN. An invalid one turns Sentry off
// with a warning, like the other optional tools.
func (p *parser) sentryDSN() string {
	dsn := p.value("SENTRY_DSN")
	if dsn == "" {
		return ""
	}
	if _, err := sentry.NewDsn(dsn); err != nil {
		p.warn("SENTRY_DSN", fmt.Errorf("must be a valid DSN, Sentry is off: %w", err))
		return ""
	}
	return dsn
}

func (p *parser) websiteID(name string) string {
	id := strings.ToLower(p.value(name))
	if id != "" && !uuidPattern.MatchString(id) {
		p.warn(name, errors.New("must be a UUID: this site is not measured"))
		return ""
	}
	return id
}
