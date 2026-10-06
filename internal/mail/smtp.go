package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

// smtpTimeout bounds a whole delivery: dial, TLS, commands and data.
const smtpTimeout = 30 * time.Second

var errNoStartTLS = errors.New("smtp server does not offer STARTTLS")

// smtpStage names where a delivery failed. The values are stable: they are
// span failure codes and log attributes, and never carry the error text.
type smtpStage string

const (
	stageConnect  smtpStage = "smtp_connect"
	stageTLS      smtpStage = "smtp_tls"
	stageAuth     smtpStage = "smtp_auth"
	stageRejected smtpStage = "smtp_rejected"
	stageData     smtpStage = "smtp_data"
	stageHeader   smtpStage = "smtp_header"
	// stageOther covers every failure that carries no stage of its own.
	stageOther smtpStage = "smtp_unavailable"
)

var errHeaderBreak = atStage(stageHeader, fmt.Errorf("%w: line break in a mail header", ErrPermanent))

// stageError tags err with the stage that failed.
type stageError struct {
	stage smtpStage
	err   error
}

func (e *stageError) Error() string { return e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

func atStage(stage smtpStage, err error) error { return &stageError{stage: stage, err: err} }

// stageOf names where a delivery failed: the span failure code and the worker
// log attribute. Never the error text.
func stageOf(err error) string {
	if se, ok := errors.AsType[*stageError](err); ok {
		return string(se.stage)
	}
	return string(stageOther)
}

// SMTP delivers mails through the relay of SMTP_* (spec §6). The connection
// is always encrypted before authentication: implicit TLS or STARTTLS.
type SMTP struct {
	cfg       config.SMTP
	from      *netmail.Address
	tlsConfig *tls.Config // the package tests add their own root
}

// NewSMTP returns a sender for the relay cfg, sending as from (MAIL_FROM).
func NewSMTP(cfg config.SMTP, from *netmail.Address) *SMTP {
	return &SMTP{cfg: cfg, from: from, tlsConfig: &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}}
}

// Send delivers m. A 5xx reply to the recipient or to the message wraps
// ErrPermanent; anything else (network, 4xx, authentication, missing
// STARTTLS) is temporary, so a misconfigured relay delays mails instead of
// losing them.
func (s *SMTP) Send(ctx context.Context, m Message) (err error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "smtp.send", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	defer func() {
		if err != nil {
			telemetry.Fail(span, stageOf(err))
		}
	}()

	// Header injection: subjects never carry member text and addresses are
	// normalized; this is the last guard, before any SMTP command.
	if strings.ContainsAny(m.To+m.Subject, "\r\n") {
		return errHeaderBreak
	}
	data, err := s.compose(m)
	if err != nil {
		return err
	}
	conn, err := s.dial(ctx)
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return errors.Join(atStage(stageConnect, fmt.Errorf("smtp greeting: %w", err)), conn.Close())
	}
	if err := s.deliver(c, m.To, data); err != nil {
		return errors.Join(err, c.Close())
	}
	return nil
}

// dial connects with implicit TLS or in plain text for STARTTLS. The
// deadline covers the whole dialogue: a stalled relay cannot hold the worker
// for longer than smtpTimeout.
func (s *SMTP) dial(ctx context.Context) (net.Conn, error) {
	deadline := time.Now().Add(smtpTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	conn, err := (&net.Dialer{Deadline: deadline}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, atStage(stageConnect, fmt.Errorf("smtp connect: %w", err))
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, errors.Join(atStage(stageConnect, fmt.Errorf("smtp deadline: %w", err)), conn.Close())
	}
	if s.cfg.TLS != config.SMTPImplicit {
		return conn, nil
	}
	tc := tls.Client(conn, s.tlsConfig)
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, errors.Join(atStage(stageTLS, fmt.Errorf("smtp tls: %w", err)), conn.Close())
	}
	return tc, nil
}

// deliver runs the SMTP dialogue on c and ends it with a best-effort QUIT. The password
// only travels encrypted: implicit TLS, or after STARTTLS.
func (s *SMTP) deliver(c *smtp.Client, to string, data []byte) error {
	if s.cfg.TLS == config.SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return atStage(stageTLS, errNoStartTLS)
		}
		if err := c.StartTLS(s.tlsConfig); err != nil {
			return atStage(stageTLS, fmt.Errorf("smtp starttls: %w", err))
		}
	}
	if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
		return atStage(stageAuth, fmt.Errorf("smtp auth: %w", err))
	}
	if err := c.Mail(s.from.Address); err != nil {
		return atStage(stageRejected, fmt.Errorf("smtp mail from: %w", err))
	}
	if err := c.Rcpt(to); err != nil {
		return atStage(stageRejected, fmt.Errorf("smtp rcpt: %w", rejected(err)))
	}
	if err := writeData(c, data); err != nil {
		return atStage(stageData, fmt.Errorf("smtp data: %w", rejected(err)))
	}
	// The relay accepted the message: a failed QUIT must not trigger a retry,
	// which would deliver the mail twice. net/smtp leaves the socket open
	// when QUIT fails, so close it ourselves.
	if c.Quit() != nil {
		_ = c.Close() // best effort: the message is already accepted
	}
	return nil
}

func writeData(c *smtp.Client, data []byte) error {
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return errors.Join(err, w.Close())
	}
	return w.Close()
}

// compose builds the message: a text part and its HTML alternative, both
// quoted-printable. No image, no tracking, no Reply-To: mails are not
// answered (spec §6).
func (s *SMTP) compose(m Message) ([]byte, error) {
	html, err := renderHTML(m.Text)
	if err != nil {
		return nil, err
	}
	id, err := secure.NewToken()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, h := range [][2]string{
		{"From", s.from.String()},
		{"To", (&netmail.Address{Address: m.To}).String()},
		{"Subject", mime.QEncoding.Encode("utf-8", m.Subject)},
		{"Date", time.Now().Format(time.RFC1123Z)},
		{"Message-ID", "<" + id + "@" + domainOf(s.from.Address) + ">"},
		{"MIME-Version", "1.0"},
		{"Content-Type", `multipart/alternative; boundary="` + mw.Boundary() + `"`},
	} {
		b.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	b.WriteString("\r\n")
	for _, p := range [][2]string{{"text/plain; charset=utf-8", m.Text}, {"text/html; charset=utf-8", html}} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {p[0]},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, fmt.Errorf("mail part: %w", err)
		}
		qp := quotedprintable.NewWriter(w)
		if _, err := qp.Write([]byte(p[1])); err != nil {
			return nil, fmt.Errorf("mail part: %w", err)
		}
		if err := qp.Close(); err != nil {
			return nil, fmt.Errorf("mail part: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("mail parts: %w", err)
	}
	return b.Bytes(), nil
}

// rejected marks a 5xx reply to the recipient or to the message as
// permanent. Other refusals (authentication, sender) are configuration
// errors: the mail waits until they are fixed.
func rejected(err error) error {
	var reply *textproto.Error
	if errors.As(err, &reply) && reply.Code >= 500 {
		return fmt.Errorf("%w: %w", ErrPermanent, err)
	}
	return err
}

func domainOf(address string) string {
	return address[strings.LastIndexByte(address, '@')+1:]
}
