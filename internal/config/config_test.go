package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
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
		{"url with path", func(m map[string]string) { m["ADMIN_BASE_URL"] = "https://comite.example.org/admin" }, "ADMIN_BASE_URL"},
		{"bad env", func(m map[string]string) { m["APP_ENV"] = "staging" }, "APP_ENV"},
		{"bad smtp tls", func(m map[string]string) { m["SMTP_TLS"] = "none" }, "SMTP_TLS"},
		{"bad port", func(m map[string]string) { m["PORT"] = "abc" }, "PORT"},
		{"bad smtp port", func(m map[string]string) { m["SMTP_PORT"] = "70000" }, "SMTP_PORT"},
		{"bad mail from", func(m map[string]string) { m["MAIL_FROM"] = "not an address" }, "MAIL_FROM"},
		{"bad duration", func(m map[string]string) { m["MEMBERS_MAX_AGE"] = "two weeks" }, "MEMBERS_MAX_AGE"},
		{"bad log level", func(m map[string]string) { m["LOG_LEVEL"] = "loud" }, "LOG_LEVEL"},
		{"bad proxy", func(m map[string]string) { m["TRUSTED_PROXIES"] = "10.0.0.0/8, nope" }, "TRUSTED_PROXIES"},
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
