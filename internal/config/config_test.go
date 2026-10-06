package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"log/slog"
	"maps"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validEnv() map[string]string {
	return map[string]string{
		"BASE_URL":             "https://sos.example.org",
		"ADMIN_BASE_URL":       "https://comite.sos.example.org",
		"SECRET_KEY":           base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
		"SMTP_HOST":            "smtp.example.org",
		"SMTP_PORT":            "465",
		"SMTP_USERNAME":        "user",
		"SMTP_PASSWORD":        "pass",
		"MAIL_FROM":            "Support <support@example.org>",
		"NOTIFY_EMAIL":         "club@example.org",
		"TURNSTILE_SITE_KEY":   "1x00000000000000000000AA",
		"TURNSTILE_SECRET_KEY": "1x0000000000000000000000000000000AA",
		"S3_ENDPOINT":          "https://account.eu.r2.cloudflarestorage.com",
		"S3_BUCKET":            "captures",
		"S3_ACCESS_KEY_ID":     "id",
		"S3_SECRET_ACCESS_KEY": "secret",
	}
}

func getenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDropsDefaultPortFromURLs(t *testing.T) {
	env := validEnv()
	env["ADMIN_BASE_URL"] = "https://comite.sos.example.org:443"
	c, err := Load(getenv(env))
	require.NoError(t, err)
	assert.Equal(t, "comite.sos.example.org", c.AdminBaseURL.Host)

	env = validEnv()
	env["APP_ENV"] = "development"
	env["BASE_URL"] = "http://sos.localhost:80"
	env["ADMIN_BASE_URL"] = "http://comite.localhost:8080"
	c, err = Load(getenv(env))
	require.NoError(t, err)
	assert.Equal(t, "sos.localhost", c.BaseURL.Host)
	assert.Equal(t, "comite.localhost:8080", c.AdminBaseURL.Host)
}

func TestLoadValidProductionAppliesDefaults(t *testing.T) {
	c, err := Load(getenv(validEnv()))
	require.NoError(t, err)

	assert.Equal(t, EnvProduction, c.Env)
	assert.Equal(t, "sos.example.org", c.BaseURL.Host)
	assert.Equal(t, "comite.sos.example.org", c.AdminBaseURL.Host)
	assert.Len(t, c.SecretKey, 32)
	assert.Equal(t, 8080, c.Port)
	assert.Equal(t, "/data", c.DataDir)
	assert.Equal(t, "/config/admins.yaml", c.AdminsFile)
	assert.Equal(t, slog.LevelInfo, c.LogLevel)
	assert.Equal(t, SMTPImplicit, c.SMTP.TLS)
	assert.Equal(t, 465, c.SMTP.Port)
	assert.Equal(t, "club@example.org", c.NotifyEmail.Address)
	assert.Equal(t, 336*time.Hour, c.MembersMaxAge)
	assert.Equal(t, 168*time.Hour, c.PaymentsMaxAge)
	assert.Equal(t, 48*time.Hour, c.AgeWarnAfter)
	assert.Equal(t, 168*time.Hour, c.AgeAlertAfter)
	assert.Equal(t, 365, c.RetentionDays)
	assert.Equal(t, 20, c.FormRateLimit)
	assert.Equal(t, "https://plongee-pradet.fr", c.VPDiveBaseURL.String())
	require.NotNil(t, c.S3)
	assert.Equal(t, "auto", c.S3.Region)
	assert.True(t, c.TurnstileEnabled())
}

func TestLoadNamesEveryMissingVariable(t *testing.T) {
	m := validEnv()
	delete(m, "SECRET_KEY")
	delete(m, "SMTP_HOST")
	delete(m, "NOTIFY_EMAIL")

	_, err := Load(getenv(m))
	require.ErrorIs(t, err, ErrMissing)
	for _, name := range []string{"SECRET_KEY", "SMTP_HOST", "NOTIFY_EMAIL"} {
		assert.Contains(t, err.Error(), name)
	}
}

func TestLoadSecretKey(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	tests := []struct {
		name, value string
		ok          bool
	}{
		{"plain", good, true},
		{"trailing newline and spaces", "  " + good + "\n", true},
		{"too short", base64.StdEncoding.EncodeToString(make([]byte, 16)), false},
		{"not base64", "not base64 at all!", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validEnv()
			m["SECRET_KEY"] = tt.value
			c, err := Load(getenv(m))
			if tt.ok {
				require.NoError(t, err)
				assert.Len(t, c.SecretKey, 32)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "SECRET_KEY")
		})
	}
}

func TestLoadProductionRules(t *testing.T) {
	tests := []struct {
		name   string
		change func(map[string]string)
		want   string
	}{
		{"http base url", func(m map[string]string) { m["BASE_URL"] = "http://sos.example.org" }, "BASE_URL"},
		{"no turnstile", func(m map[string]string) { delete(m, "TURNSTILE_SITE_KEY"); delete(m, "TURNSTILE_SECRET_KEY") }, "TURNSTILE_SITE_KEY"},
		{"half turnstile", func(m map[string]string) { delete(m, "TURNSTILE_SECRET_KEY") }, "TURNSTILE_SECRET_KEY"},
		{"no s3", func(m map[string]string) { delete(m, "S3_BUCKET") }, "S3_BUCKET"},
		{"same hosts", func(m map[string]string) { m["ADMIN_BASE_URL"] = "https://SOS.example.org" }, "ADMIN_BASE_URL"},
		{"same hostname other port", func(m map[string]string) { m["ADMIN_BASE_URL"] = "https://sos.example.org:8443" }, "ADMIN_BASE_URL"},
		{"url with path", func(m map[string]string) { m["ADMIN_BASE_URL"] = "https://comite.example.org/admin" }, "ADMIN_BASE_URL"},
		{"bad env", func(m map[string]string) { m["APP_ENV"] = "staging" }, "APP_ENV"},
		{"bad smtp tls", func(m map[string]string) { m["SMTP_TLS"] = "none" }, "SMTP_TLS"},
		{"bad port", func(m map[string]string) { m["PORT"] = "abc" }, "PORT"},
		{"bad smtp port", func(m map[string]string) { m["SMTP_PORT"] = "70000" }, "SMTP_PORT"},
		{"bad mail from", func(m map[string]string) { m["MAIL_FROM"] = "not an address" }, "MAIL_FROM"},
		{"bad duration", func(m map[string]string) { m["MEMBERS_MAX_AGE"] = "two weeks" }, "MEMBERS_MAX_AGE"},
		{"bad payments age", func(m map[string]string) { m["PAYMENTS_MAX_AGE"] = "a week" }, "PAYMENTS_MAX_AGE"},
		{"bad log level", func(m map[string]string) { m["LOG_LEVEL"] = "loud" }, "LOG_LEVEL"},
		{"bad proxy", func(m map[string]string) { m["TRUSTED_PROXIES"] = "10.0.0.0/8, nope" }, "TRUSTED_PROXIES"},
		{"alert before warn", func(m map[string]string) { m["AGE_WARN_AFTER"] = "72h"; m["AGE_ALERT_AFTER"] = "48h" }, "AGE_ALERT_AFTER"},
		{"bad warn age", func(m map[string]string) { m["AGE_WARN_AFTER"] = "-1h" }, "AGE_WARN_AFTER"},
		{"retention shorter than the reply window", func(m map[string]string) { m["RETENTION_DAYS"] = "14" }, "RETENTION_DAYS"},
		{"rate without unit", func(m map[string]string) { m["FORM_RATE_LIMIT"] = "20" }, "FORM_RATE_LIMIT"},
		{"rate per minute", func(m map[string]string) { m["FORM_RATE_LIMIT"] = "20/min" }, "FORM_RATE_LIMIT"},
		{"rate too high", func(m map[string]string) { m["FORM_RATE_LIMIT"] = "10001/h" }, "FORM_RATE_LIMIT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validEnv()
			tt.change(m)
			_, err := Load(getenv(m))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestLoadDevelopmentAllowsHTTPWithoutTurnstileAndS3(t *testing.T) {
	m := validEnv()
	m["APP_ENV"] = "development"
	m["BASE_URL"] = "http://sos.localhost:8080"
	m["ADMIN_BASE_URL"] = "http://comite.localhost:8080"
	for _, k := range []string{"TURNSTILE_SITE_KEY", "TURNSTILE_SECRET_KEY", "S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"} {
		delete(m, k)
	}

	c, err := Load(getenv(m))
	require.NoError(t, err)
	assert.Equal(t, EnvDevelopment, c.Env)
	assert.Equal(t, "comite.localhost:8080", c.AdminBaseURL.Host)
	assert.Nil(t, c.S3)
	assert.False(t, c.TurnstileEnabled())
}

func TestLoadTrustedProxies(t *testing.T) {
	m := validEnv()
	m["TRUSTED_PROXIES"] = "10.0.0.0/8, 192.168.1.10 ,fd00::/8"
	c, err := Load(getenv(m))
	require.NoError(t, err)
	assert.Equal(t, []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.1.10/32"),
		netip.MustParsePrefix("fd00::/8"),
	}, c.TrustedProxies)
}

func TestLoadLot2Settings(t *testing.T) {
	m := validEnv()
	m["AGE_WARN_AFTER"] = "24h"
	m["AGE_ALERT_AFTER"] = "96h"
	m["RETENTION_DAYS"] = "730"
	m["FORM_RATE_LIMIT"] = " 35/h "
	c, err := Load(getenv(m))
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, c.AgeWarnAfter)
	assert.Equal(t, 96*time.Hour, c.AgeAlertAfter)
	assert.Equal(t, 730, c.RetentionDays)
	assert.Equal(t, 35, c.FormRateLimit)
}

func TestLoadLLM(t *testing.T) {
	c, err := Load(getenv(validEnv()))
	require.NoError(t, err)
	assert.Nil(t, c.LLM, "no key: suggestions are off")

	m := validEnv()
	m["LLM_API_KEY"] = "sk-test"
	c, err = Load(getenv(m))
	require.NoError(t, err)
	require.NotNil(t, c.LLM)
	assert.Equal(t, "https://api.anthropic.com", c.LLM.BaseURL.String())
	assert.Equal(t, "claude-haiku-4-5-20251001", c.LLM.Model)
	assert.Equal(t, 8*time.Second, c.LLM.Timeout)
	assert.Equal(t, 200, c.LLM.DailyLimit)

	m["LLM_BASE_URL"] = "https://api.deepseek.com/anthropic"
	c, err = Load(getenv(m))
	require.NoError(t, err)
	assert.Equal(t, "https://api.deepseek.com/anthropic", c.LLM.BaseURL.String(), "a provider prefix is kept")

	tests := map[string]string{
		"LLM_BASE_URL":    "http://llm.example.org",
		"LLM_TIMEOUT":     "2m",
		"LLM_DAILY_LIMIT": "0",
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			m := validEnv()
			m[name] = value
			_, err := Load(getenv(m))
			require.Error(t, err, "checked even without a key")
			assert.Contains(t, err.Error(), name)
		})
	}

	m = validEnv()
	m["APP_ENV"] = "development"
	m["LLM_API_KEY"] = "sk-test"
	m["LLM_BASE_URL"] = "http://127.0.0.1:9999"
	c, err = Load(getenv(m))
	require.NoError(t, err, "development may point at a local stub")
	assert.Equal(t, "127.0.0.1:9999", c.LLM.BaseURL.Host)
}

// vapidPair is a fixed P-256 key pair in the base64url form of .env.
func vapidPair(t *testing.T, seed byte) (public, private string) {
	t.Helper()
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), bytes.Repeat([]byte{seed}, 32))
	require.NoError(t, err)
	pub, err := key.PublicKey.Bytes()
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(pub), base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

func TestLoadPush(t *testing.T) {
	c, err := Load(getenv(validEnv()))
	require.NoError(t, err)
	assert.Empty(t, c.PushoverToken, "Pushover is off by default")
	assert.Nil(t, c.VAPID, "Web Push is off by default")
	assert.Equal(t, []string{"fcm.googleapis.com", "web.push.apple.com", "updates.push.services.mozilla.com", ".notify.windows.com"},
		c.PushAllowedHosts)

	public, private := vapidPair(t, 3)
	m := validEnv()
	m["PUSHOVER_APP_TOKEN"] = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
	m["VAPID_PUBLIC_KEY"] = public
	m["VAPID_PRIVATE_KEY"] = private + "=" // padded base64url is accepted too
	m["VAPID_SUBJECT"] = "mailto:club@example.org"
	m["PUSH_ALLOWED_HOSTS"] = " FCM.googleapis.com, .push.example.org "
	c, err = Load(getenv(m))
	require.NoError(t, err)
	assert.Equal(t, "azGDORePK8gMaC0QOYAMyEEuzJnyUi", c.PushoverToken)
	require.NotNil(t, c.VAPID)
	assert.Equal(t, public, c.VAPID.PublicKey)
	assert.Equal(t, "mailto:club@example.org", c.VAPID.Subject)
	raw, err := c.VAPID.PrivateKey.Bytes()
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte{3}, 32), raw)
	assert.Equal(t, []string{"fcm.googleapis.com", ".push.example.org"}, c.PushAllowedHosts)

	m["VAPID_SUBJECT"] = "https://sos.example.org"
	_, err = Load(getenv(m))
	require.NoError(t, err, "an https subject is accepted")
}

func TestLoadPushRefusals(t *testing.T) {
	public, private := vapidPair(t, 3)
	other, _ := vapidPair(t, 4)
	complete := func() map[string]string {
		m := validEnv()
		m["VAPID_PUBLIC_KEY"], m["VAPID_PRIVATE_KEY"], m["VAPID_SUBJECT"] = public, private, "mailto:club@example.org"
		return m
	}
	cases := map[string]struct {
		set  map[string]string
		name string
	}{
		"token too short":     {map[string]string{"PUSHOVER_APP_TOKEN": "abc"}, "PUSHOVER_APP_TOKEN"},
		"token with symbols":  {map[string]string{"PUSHOVER_APP_TOKEN": "azGDORePK8gMaC0QOYAMyEEuzJny-i"}, "PUSHOVER_APP_TOKEN"},
		"public key missing":  {map[string]string{"VAPID_PUBLIC_KEY": ""}, "VAPID_PUBLIC_KEY"},
		"private key missing": {map[string]string{"VAPID_PRIVATE_KEY": ""}, "VAPID_PRIVATE_KEY"},
		"subject missing":     {map[string]string{"VAPID_SUBJECT": ""}, "VAPID_SUBJECT"},
		"keys do not match":   {map[string]string{"VAPID_PUBLIC_KEY": other}, "VAPID_PUBLIC_KEY"},
		"private not base64":  {map[string]string{"VAPID_PRIVATE_KEY": "not a key!"}, "VAPID_PRIVATE_KEY"},
		"private too short":   {map[string]string{"VAPID_PRIVATE_KEY": "AQID"}, "VAPID_PRIVATE_KEY"},
		"subject not mailto":  {map[string]string{"VAPID_SUBJECT": "club@example.org"}, "VAPID_SUBJECT"},
		"subject http":        {map[string]string{"VAPID_SUBJECT": "http://sos.example.org"}, "VAPID_SUBJECT"},
		"bad mailto":          {map[string]string{"VAPID_SUBJECT": "mailto:not an address"}, "VAPID_SUBJECT"},
		"host with a path":    {map[string]string{"PUSH_ALLOWED_HOSTS": "fcm.googleapis.com/fcm"}, "PUSH_ALLOWED_HOSTS"},
		"host with a port":    {map[string]string{"PUSH_ALLOWED_HOSTS": "fcm.googleapis.com:443"}, "PUSH_ALLOWED_HOSTS"},
		"no host at all":      {map[string]string{"PUSH_ALLOWED_HOSTS": " , "}, "PUSH_ALLOWED_HOSTS"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := complete()
			maps.Copy(m, tc.set)
			_, err := Load(getenv(m))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.name)
			assert.NotContains(t, err.Error(), private, "never echo the private key")
		})
	}
}

func TestLoadSentry(t *testing.T) {
	c, err := Load(getenv(validEnv()))
	require.NoError(t, err)
	assert.Empty(t, c.SentryDSN, "Sentry is off by default")
	assert.Equal(t, "production", c.SentryEnvironment, "the environment defaults to APP_ENV")

	m := validEnv()
	m["SENTRY_DSN"] = " https://key@o1.ingest.example.org/42 "
	m["SENTRY_ENVIRONMENT"] = "staging"
	c, err = Load(getenv(m))
	require.NoError(t, err)
	assert.Equal(t, "https://key@o1.ingest.example.org/42", c.SentryDSN)
	assert.Equal(t, "staging", c.SentryEnvironment)

	m = validEnv()
	m["APP_ENV"] = "development"
	m["SENTRY_DSN"] = "https://key@o1.ingest.example.org/42"
	c, err = Load(getenv(m))
	require.NoError(t, err)
	assert.Empty(t, c.SentryDSN, "Sentry is fully off in development")
	assert.Equal(t, "development", c.SentryEnvironment)
	assert.Empty(t, c.Warnings)

	m = validEnv()
	m["SENTRY_DSN"] = "not a dsn"
	c, err = Load(getenv(m))
	require.NoError(t, err, "never a startup failure")
	assert.Empty(t, c.SentryDSN, "an invalid DSN turns Sentry off")
	require.Len(t, c.Warnings, 1)
	require.ErrorContains(t, c.Warnings[0], "SENTRY_DSN")
}

func TestLoadUmami(t *testing.T) {
	c, err := Load(getenv(validEnv()))
	require.NoError(t, err)
	assert.Nil(t, c.Umami, "Umami is off by default")
	assert.Empty(t, c.Warnings)

	m := validEnv()
	m["UMAMI_SCRIPT_URL"] = "https://analytics.example.org/script.js"
	m["UMAMI_WEBSITE_ID"] = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"
	m["UMAMI_ADMIN_WEBSITE_ID"] = "12345678-1234-4123-8123-123456789abc"
	c, err = Load(getenv(m))
	require.NoError(t, err)
	require.NotNil(t, c.Umami)
	assert.Equal(t, "https://analytics.example.org/script.js", c.Umami.ScriptURL.String())
	assert.Equal(t, "https://analytics.example.org", c.Umami.Origin())
	assert.Equal(t, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", c.Umami.WebsiteID)
	assert.Equal(t, "12345678-1234-4123-8123-123456789abc", c.Umami.AdminWebsiteID)
	assert.Empty(t, c.Warnings)

	m["UMAMI_ADMIN_WEBSITE_ID"] = ""
	c, err = Load(getenv(m))
	require.NoError(t, err)
	require.NotNil(t, c.Umami)
	assert.Empty(t, c.Umami.AdminWebsiteID, "a site without an ID is not measured")
	assert.Empty(t, c.Warnings)

	m["UMAMI_WEBSITE_ID"] = ""
	c, err = Load(getenv(m))
	require.NoError(t, err)
	assert.Nil(t, c.Umami, "a script without any site ID measures nothing: Umami is off")
	require.Len(t, c.Warnings, 1)
	require.ErrorContains(t, c.Warnings[0], "UMAMI_SCRIPT_URL")

	m = validEnv()
	m["APP_ENV"] = "development"
	m["BASE_URL"] = "http://sos.localhost:8080"
	m["ADMIN_BASE_URL"] = "http://comite.localhost:8080"
	m["UMAMI_SCRIPT_URL"] = "http://127.0.0.1:3000/script.js"
	m["UMAMI_WEBSITE_ID"] = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	c, err = Load(getenv(m))
	require.NoError(t, err)
	require.NotNil(t, c.Umami, "development accepts http")
	assert.Equal(t, "http://127.0.0.1:3000", c.Umami.Origin())
}

// An invalid optional tool never stops the server: it is turned off with a
// warning (owner decision, 2026-10-06).
func TestLoadInvalidUmamiTurnsItOffWithAWarning(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		name string
		off  bool // the whole tool is off, not just one site
	}{
		"http in production": {map[string]string{"UMAMI_SCRIPT_URL": "http://analytics.example.org/script.js"}, "UMAMI_SCRIPT_URL", true},
		"relative URL":       {map[string]string{"UMAMI_SCRIPT_URL": "/script.js"}, "UMAMI_SCRIPT_URL", true},
		"no script path":     {map[string]string{"UMAMI_SCRIPT_URL": "https://analytics.example.org"}, "UMAMI_SCRIPT_URL", true},
		"with a query":       {map[string]string{"UMAMI_SCRIPT_URL": "https://analytics.example.org/script.js?v=2"}, "UMAMI_SCRIPT_URL", true},
		// Same origin: Referrer-Policy same-origin would hand Umami the
		// tracking page's address, token included, in the Referer header.
		"members origin":   {map[string]string{"UMAMI_SCRIPT_URL": "https://SOS.example.org:443/umami/script.js"}, "UMAMI_SCRIPT_URL", true},
		"committee origin": {map[string]string{"UMAMI_SCRIPT_URL": "https://comite.sos.example.org/umami/script.js"}, "UMAMI_SCRIPT_URL", true},
		"members ID":       {map[string]string{"UMAMI_WEBSITE_ID": "not-a-uuid"}, "UMAMI_WEBSITE_ID", false},
		"committee ID":     {map[string]string{"UMAMI_ADMIN_WEBSITE_ID": "12345678123441238123123456789abc"}, "UMAMI_ADMIN_WEBSITE_ID", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := validEnv()
			m["UMAMI_SCRIPT_URL"] = "https://analytics.example.org/script.js"
			m["UMAMI_WEBSITE_ID"] = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			m["UMAMI_ADMIN_WEBSITE_ID"] = "12345678-1234-4123-8123-123456789abc"
			maps.Copy(m, tc.set)
			c, err := Load(getenv(m))
			require.NoError(t, err, "never a startup failure")
			require.Len(t, c.Warnings, 1)
			require.ErrorContains(t, c.Warnings[0], tc.name)
			if tc.off {
				assert.Nil(t, c.Umami)
				return
			}
			require.NotNil(t, c.Umami)
			ids := c.Umami.WebsiteID + "|" + c.Umami.AdminWebsiteID
			assert.NotContains(t, ids, tc.set[tc.name], "the invalid ID is dropped")
		})
	}
}
