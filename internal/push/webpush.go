package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

const (
	tracerName = "github.com/SkYNewZ/sos-vpdive/internal/push"
	// sendTimeout bounds a call to a push service or to Pushover.
	sendTimeout = 10 * time.Second
	// messageTTL is how long a push service keeps an undelivered alert.
	messageTTL = "86400"
)

var (
	// ErrGone reports a subscription the push service no longer knows.
	ErrGone = errors.New("push subscription gone")
	// ErrEndpointRefused reports an endpoint that is not HTTPS on an allowed host.
	ErrEndpointRefused = errors.New("push endpoint refused")
	// errRejected reports any other answer than success.
	errRejected = errors.New("push service refused the message")
)

// Subscription is a browser's push subscription, keys decoded.
type Subscription struct {
	ID       int64
	Endpoint string
	P256DH   []byte // the browser's P-256 public key, uncompressed
	Auth     []byte // 16-byte authentication secret
}

// HostAllowed reports whether host is in hosts (PUSH_ALLOWED_HOSTS); an
// entry with a leading dot accepts its subdomains.
func HostAllowed(hosts []string, host string) bool {
	host = strings.ToLower(host)
	for _, h := range hosts {
		if host == h || (strings.HasPrefix(h, ".") && strings.HasSuffix(host, h)) {
			return true
		}
	}
	return false
}

// Client sends encrypted messages to push services.
type Client struct {
	http  *http.Client
	vapid *vapid
	hosts []string
}

// NewClient returns a client; hosts is PUSH_ALLOWED_HOSTS.
func NewClient(cfg *config.VAPID, hosts []string, now func() time.Time) *Client {
	return &Client{http: newHTTPClient(), vapid: newVAPID(cfg, now), hosts: hosts}
}

// newHTTPClient is short and follows no redirect (spec §9.6).
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: sendTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Send encrypts payload for sub and posts it once. ErrGone when the push
// service answers 404 or 410; ErrEndpointRefused for an endpoint outside
// PUSH_ALLOWED_HOSTS.
func (c *Client) Send(ctx context.Context, sub Subscription, payload []byte) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "webpush.send", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || !HostAllowed(c.hosts, u.Hostname()) {
		telemetry.Fail(span, "push_endpoint_refused")
		return ErrEndpointRefused
	}
	span.SetAttributes(attribute.String("push.host", u.Hostname()))
	serverKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("push key: %w", err)
	}
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("push salt: %w", err)
	}
	body, err := encrypt(payload, sub.P256DH, sub.Auth, serverKey, salt)
	if err != nil {
		telemetry.Fail(span, "push_encrypt")
		return err
	}
	auth, err := c.vapid.authorization(u.Scheme + "://" + u.Host)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("push request: %w", err)
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", messageTTL)
	req.Header.Set("Authorization", auth)
	resp, err := c.http.Do(req)
	if err != nil {
		telemetry.Fail(span, "push_unavailable")
		return fmt.Errorf("push service unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		telemetry.Fail(span, "push_gone")
		return ErrGone
	default:
		telemetry.Fail(span, "push_rejected")
		return fmt.Errorf("%w: status %s", errRejected, strconv.Itoa(resp.StatusCode))
	}
}
