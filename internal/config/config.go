// Package config reads the deployment configuration from environment
// variables (spec §10). Business content lives in embedded files instead.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const schemeHTTPS = "https"

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

// SMTP holds the mail relay settings. Lot 2 sends mails; lot 1 validates them.
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

// Config is the validated deployment configuration.
type Config struct {
	Env                Env
	BaseURL            *url.URL
	AdminBaseURL       *url.URL
	VPDiveBaseURL      *url.URL
	SecretKey          []byte
	Port               int
	DataDir            string
	AdminsFile         string
	TrustedProxies     []netip.Prefix
	LogLevel           slog.Level
	SMTP               SMTP
	MailFrom           *mail.Address
	NotifyEmail        *mail.Address
	S3                 *S3
	TurnstileSiteKey   string
	TurnstileSecretKey string
	MembersMaxAge      time.Duration
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
		AdminsFile:     p.optional("ADMINS_FILE", "/config/admins.yaml"),
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
		VPDiveBaseURL:      p.url("VPDIVE_BASE_URL", p.optional("VPDIVE_BASE_URL", "https://plongee-pradet.fr")),
	}
	c.BaseURL = p.url("BASE_URL", p.required("BASE_URL"))
	c.AdminBaseURL = p.url("ADMIN_BASE_URL", p.required("ADMIN_BASE_URL"))
	if c.BaseURL != nil && c.AdminBaseURL != nil && strings.EqualFold(c.BaseURL.Hostname(), c.AdminBaseURL.Hostname()) {
		p.fail("ADMIN_BASE_URL", errors.New("must use another host than BASE_URL"))
	}
	p.turnstile(c)
	c.S3 = p.s3(c.Env)
	if c.Env == EnvProduction {
		p.requireHTTPS("BASE_URL", c.BaseURL)
		p.requireHTTPS("ADMIN_BASE_URL", c.AdminBaseURL)
	}
	if err := errors.Join(p.errs...); err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}
	return c, nil
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) fail(name string, err error) {
	p.errs = append(p.errs, fmt.Errorf("%s: %w", name, err))
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

func (p *parser) duration(name, def string) time.Duration {
	d, err := time.ParseDuration(p.optional(name, def))
	if err != nil || d <= 0 {
		p.fail(name, errors.New("must be a positive Go duration such as 336h"))
		return 0
	}
	return d
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

func (p *parser) url(name, raw string) *url.URL {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != schemeHTTPS) || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		p.fail(name, errors.New("must be an absolute http(s) URL without path"))
		return nil
	}
	u.Path = ""
	// Browsers omit a scheme's default port from Origin, which is compared as
	// an exact string.
	defaultPort := map[string]string{"http": ":80", schemeHTTPS: ":443"}[u.Scheme]
	u.Host = strings.TrimSuffix(strings.ToLower(u.Host), defaultPort)
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
