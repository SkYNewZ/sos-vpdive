package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

// pushoverEndpoint is Pushover's message endpoint.
const pushoverEndpoint = "https://api.pushover.net/1/messages.json"

// Pushover delivers a committee alert to each resolver who put a user key in
// the accounts file (spec §6, as amended): the outbox's sender for
// mail.ChannelPushover. Accounts are read at each alert, so a reload of the
// file applies at once.
type Pushover struct {
	http     *http.Client
	api      string
	token    string
	admin    *url.URL
	accounts func() []admins.Account
	logger   *slog.Logger
}

// NewPushover returns the Pushover sender; token is PUSHOVER_APP_TOKEN.
func NewPushover(token string, adminBaseURL *url.URL, accounts func() []admins.Account, logger *slog.Logger) *Pushover {
	return &Pushover{http: newHTTPClient(), api: pushoverEndpoint, token: token, admin: adminBaseURL, accounts: accounts, logger: logger}
}

// Send posts m once per account with a key. One account's failure does not
// stop the others; the alert fails only when every call failed.
func (p *Pushover) Send(ctx context.Context, m mail.Message) error {
	link := p.admin.String() + "/demandes/" + strconv.FormatInt(m.TicketID, 10)
	tried, sent := 0, 0
	for _, a := range p.accounts() {
		if a.PushoverUserKey == "" {
			continue
		}
		tried++
		if err := p.post(ctx, a.PushoverUserKey, m, link); err != nil {
			p.logger.WarnContext(ctx, "pushover alert not delivered", "username", a.Username, "error", err)
			continue
		}
		sent++
	}
	if tried > 0 && sent == 0 {
		return fmt.Errorf("pushover alert reached none of %d accounts", tried)
	}
	return nil
}

func (p *Pushover) post(ctx context.Context, user string, m mail.Message, link string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "pushover.send", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	form := url.Values{
		"token": {p.token}, "user": {user}, "title": {m.Subject}, "message": {m.Text},
		"url": {link}, "url_title": {"Ouvrir la demande"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.api, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("pushover request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.http.Do(req)
	if err != nil {
		telemetry.Fail(span, "pushover_unavailable")
		return fmt.Errorf("pushover unreachable: %w", unwrapURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // its errors may quote the key
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode != http.StatusOK {
		telemetry.Fail(span, "pushover_rejected")
		return fmt.Errorf("pushover refused the message: status %d", resp.StatusCode)
	}
	return nil
}

// unwrapURL drops the URL that net/http puts in its errors: a push
// endpoint carries a capability.
func unwrapURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}
