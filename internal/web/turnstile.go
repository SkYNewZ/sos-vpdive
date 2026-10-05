package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

const (
	turnstileEndpoint = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	maxTurnstileToken = 2048
)

// Turnstile errors: a refused token, or Cloudflare not answering. The service
// never opens without the check (spec §11.3).
var (
	ErrBotCheckFailed      = errors.New("anti-bot check failed")
	ErrBotCheckUnavailable = errors.New("anti-bot service unavailable")
)

// Turnstile verifies Cloudflare Turnstile tokens server side. Cloudflare
// enforces single use and the five-minute expiry of a token.
type Turnstile struct {
	SiteKey  string
	secret   string
	endpoint string
	client   *http.Client
}

// NewTurnstile returns a verifier. An empty endpoint means Cloudflare's.
// Its plain client adds no trace header to the outgoing call.
func NewTurnstile(siteKey, secretKey, endpoint string) *Turnstile {
	if endpoint == "" {
		endpoint = turnstileEndpoint
	}
	return &Turnstile{SiteKey: siteKey, secret: secretKey, endpoint: endpoint, client: &http.Client{Timeout: 5 * time.Second}}
}

type siteverifyResponse struct {
	Success  bool   `json:"success"`
	Hostname string `json:"hostname"`
	Action   string `json:"action"`

	ErrorCodes []string `json:"error-codes"`
}

// Verify checks a token: success, expected host name, expected action.
func (t *Turnstile) Verify(ctx context.Context, token string, remoteIP netip.Addr, hostname, action string) (err error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "turnstile.verify", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	defer func() {
		switch {
		case errors.Is(err, ErrBotCheckUnavailable):
			telemetry.Fail(span, "turnstile_unavailable")
		case err != nil:
			telemetry.Fail(span, "turnstile_refused")
		}
	}()

	if token == "" || len(token) > maxTurnstileToken {
		return ErrBotCheckFailed
	}
	form := url.Values{"secret": {t.secret}, "response": {token}}
	if remoteIP.IsValid() {
		form.Set("remoteip", remoteIP.String())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBotCheckUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBotCheckUnavailable, err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("%w: %w", ErrBotCheckUnavailable, cerr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrBotCheckUnavailable, resp.StatusCode)
	}
	var out siteverifyResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return fmt.Errorf("%w: %w", ErrBotCheckUnavailable, err)
	}
	// A wrong secret or a Cloudflare fault is ours to fix, not a refused visitor.
	for _, code := range out.ErrorCodes {
		switch code {
		case "internal-error", "missing-input-secret", "invalid-input-secret":
			return fmt.Errorf("%w: %s", ErrBotCheckUnavailable, code)
		}
	}
	if !out.Success || out.Hostname != hostname || out.Action != action {
		return ErrBotCheckFailed
	}
	return nil
}

// turnstileField is the form field the Turnstile widget fills.
const turnstileField = "cf-turnstile-response"

// checkBot verifies the Turnstile token of a form posted to host. It returns
// the status and the notice to show when the check fails; Cloudflare not
// answering refuses the form too, the service never opens without the check
// (spec §11.3). Without keys (development) every form passes.
func (s *Server) checkBot(r *http.Request, token, host, action string) (int, *notice) {
	if s.turnstile == nil {
		return http.StatusOK, nil
	}
	err := s.turnstile.Verify(r.Context(), token, s.clientIP(r), host, action)
	switch {
	case errors.Is(err, ErrBotCheckUnavailable):
		s.logger.WarnContext(r.Context(), "turnstile unavailable", "error", err)
		return http.StatusServiceUnavailable, &notice{Kind: noticeError,
			Text: "Le contrôle anti-robot ne répond pas. Réessaie dans un instant, ou écris au club : " + s.cfg.NotifyEmail.Address + "."}
	case err != nil:
		return http.StatusForbidden, &notice{Kind: noticeError, Text: "Le contrôle anti-robot a échoué. Réessaie."}
	}
	return http.StatusOK, nil
}
