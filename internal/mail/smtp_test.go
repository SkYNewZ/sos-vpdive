package mail

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

// smtpState is what the fake server saw.
type smtpState struct {
	connections int
	authed      bool
	authOverTLS bool
	mailFrom    string
	rcptTo      string
	data        []byte
}

type fakeOptions struct {
	offerStartTLS bool
	authCode      int // 0: 235
	rcptCode      int // 0: 250
	dropOnQuit    bool
}

// fakeSMTP is a minimal SMTP server: EHLO, STARTTLS, AUTH, MAIL, RCPT,
// DATA, QUIT. It never calls the testing API from its goroutines.
type fakeSMTP struct {
	tlsConfig *tls.Config
	implicit  bool
	opts      fakeOptions

	mu    sync.Mutex
	state smtpState
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// startSMTP runs a fake relay and returns a sender configured for it.
func startSMTP(t *testing.T, mode config.SMTPTLS, opts fakeOptions) (*fakeSMTP, *SMTP) {
	t.Helper()
	cert, pool := testCertificate(t)
	f := &fakeSMTP{
		tlsConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		implicit:  mode == config.SMTPImplicit,
		opts:      opts,
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ln.Close()) })
	go f.accept(ln)

	s := NewSMTP(config.SMTP{
		Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		Username: "relay", Password: "relay-secret", TLS: mode,
	}, &netmail.Address{Name: "Support du club", Address: "support@example.org"})
	s.tlsConfig.RootCAs = pool
	return f, s
}

func (f *fakeSMTP) snapshot() smtpState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeSMTP) record(fn func(*smtpState)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.state)
}

func (f *fakeSMTP) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go f.serve(conn)
	}
}

func (f *fakeSMTP) serve(conn net.Conn) {
	if f.implicit {
		conn = tls.Server(conn, f.tlsConfig)
	}
	defer conn.Close() //nolint:errcheck // test server: the client may close first
	f.record(func(s *smtpState) { s.connections++ })
	tp := textproto.NewConn(conn)
	tlsOn := f.implicit
	reply := func(format string, args ...any) bool { return tp.PrintfLine(format, args...) == nil }
	orDefault := func(code, def int) int {
		if code == 0 {
			return def
		}
		return code
	}
	if !reply("220 fake.example.org ESMTP") {
		return
	}
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		ok := true
		switch strings.ToUpper(verb) {
		case "EHLO":
			ext := []string{"fake.example.org"}
			if f.opts.offerStartTLS && !tlsOn {
				ext = append(ext, "STARTTLS")
			}
			ext = append(ext, "AUTH PLAIN")
			for i, e := range ext {
				sep := "-"
				if i == len(ext)-1 {
					sep = " "
				}
				ok = ok && reply("250%s%s", sep, e)
			}
		case "STARTTLS":
			if !reply("220 ready") {
				return
			}
			secured := tls.Server(conn, f.tlsConfig)
			if secured.HandshakeContext(context.Background()) != nil {
				return
			}
			conn, tp, tlsOn = secured, textproto.NewConn(secured), true
		case "AUTH":
			f.record(func(s *smtpState) { s.authed, s.authOverTLS = true, tlsOn })
			ok = reply("%d auth", orDefault(f.opts.authCode, 235))
		case "MAIL":
			f.record(func(s *smtpState) { s.mailFrom = arg })
			ok = reply("250 sender ok")
		case "RCPT":
			f.record(func(s *smtpState) { s.rcptTo = arg })
			ok = reply("%d recipient", orDefault(f.opts.rcptCode, 250))
		case "DATA":
			if !reply("354 go ahead") {
				return
			}
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			f.record(func(s *smtpState) { s.data = data })
			ok = reply("250 queued")
		case "QUIT":
			if !f.opts.dropOnQuit {
				reply("221 bye")
			}
			return
		default:
			ok = reply("502 not implemented")
		}
		if !ok {
			return
		}
	}
}

// readParts returns the decoded parts of a multipart/alternative message,
// keyed by content type, with LF line ends.
func readParts(t *testing.T, msg *netmail.Message) map[string]string {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/alternative", mediaType)
	mr := multipart.NewReader(msg.Body, params["boundary"])
	parts := map[string]string{}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return parts
		}
		require.NoError(t, err)
		body, err := io.ReadAll(p)
		require.NoError(t, err)
		parts[p.Header.Get("Content-Type")] = strings.ReplaceAll(string(body), "\r\n", "\n")
	}
}

func testMessage() Message {
	return Message{
		To:      "lea.martin@example.org",
		Subject: "Ta demande CPP-0042 est bien reçue",
		Text:    "Bonjour Léa,\n\nSuis ta demande ici : https://sos.example.org/suivi/abc.",
	}
}

func TestSendImplicitTLS(t *testing.T) {
	f, s := startSMTP(t, config.SMTPImplicit, fakeOptions{})
	require.NoError(t, s.Send(context.Background(), testMessage()))

	st := f.snapshot()
	assert.True(t, st.authOverTLS)
	assert.Equal(t, "FROM:<support@example.org>", st.mailFrom)
	assert.Equal(t, "TO:<lea.martin@example.org>", st.rcptTo)

	msg, err := netmail.ReadMessage(bytes.NewReader(st.data))
	require.NoError(t, err)
	raw := msg.Header.Get("Subject")
	subject, err := new(mime.WordDecoder).DecodeHeader(raw)
	require.NoError(t, err)
	assert.Equal(t, testMessage().Subject, subject)
	assert.NotEqual(t, subject, raw, "a non-ASCII subject is encoded")
	assert.Contains(t, msg.Header.Get("From"), "<support@example.org>")
	assert.True(t, strings.HasSuffix(msg.Header.Get("Message-ID"), "@example.org>"))
	assert.Empty(t, msg.Header.Get("Reply-To"))

	parts := readParts(t, msg)
	assert.Equal(t, testMessage().Text, parts["text/plain; charset=utf-8"])
	assert.Contains(t, parts["text/html; charset=utf-8"],
		`<a href="https://sos.example.org/suivi/abc">https://sos.example.org/suivi/abc</a>.`)
}

func TestSendStartTLS(t *testing.T) {
	f, s := startSMTP(t, config.SMTPStartTLS, fakeOptions{offerStartTLS: true})
	require.NoError(t, s.Send(context.Background(), testMessage()))
	st := f.snapshot()
	assert.True(t, st.authed)
	assert.True(t, st.authOverTLS, "authentication only after STARTTLS")
}

func TestSendRefusesARelayWithoutStartTLS(t *testing.T) {
	f, s := startSMTP(t, config.SMTPStartTLS, fakeOptions{offerStartTLS: false})
	err := s.Send(context.Background(), testMessage())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPermanent, "the mail waits for a relay that encrypts")
	st := f.snapshot()
	assert.False(t, st.authed, "no password sent in clear")
	assert.Empty(t, st.mailFrom)
}

func TestSendClassifiesRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      fakeOptions
		permanent bool
		code      string
	}{
		{"recipient busy", fakeOptions{rcptCode: 450}, false, "smtp_rejected"},
		{"recipient unknown", fakeOptions{rcptCode: 550}, true, "smtp_rejected"},
		{"wrong relay password", fakeOptions{authCode: 535}, false, "smtp_auth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := startSMTP(t, config.SMTPImplicit, tc.opts)
			err := s.Send(context.Background(), testMessage())
			require.Error(t, err)
			assert.Equal(t, tc.permanent, errors.Is(err, ErrPermanent))
			assert.Equal(t, tc.code, stageOf(err))
		})
	}
}

func TestSendNamesTheTLSStageOfAnUntrustedRelay(t *testing.T) {
	for _, mode := range []config.SMTPTLS{config.SMTPImplicit, config.SMTPStartTLS} {
		t.Run(string(mode), func(t *testing.T) {
			f, s := startSMTP(t, mode, fakeOptions{offerStartTLS: true})
			s.tlsConfig.RootCAs = x509.NewCertPool() // the relay's certificate is not trusted
			err := s.Send(context.Background(), testMessage())
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrPermanent)
			assert.Equal(t, "smtp_tls", stageOf(err))
			assert.False(t, f.snapshot().authed)
		})
	}
}

func TestSendIgnoresAFailedQuit(t *testing.T) {
	f, s := startSMTP(t, config.SMTPImplicit, fakeOptions{dropOnQuit: true})
	require.NoError(t, s.Send(context.Background(), testMessage()), "the message was accepted: no retry")
	assert.NotEmpty(t, f.snapshot().data)
}

func TestSendRefusesLineBreaksInHeaders(t *testing.T) {
	f, s := startSMTP(t, config.SMTPImplicit, fakeOptions{})
	for _, m := range []Message{
		{To: "lea.martin@example.org\r\nBcc: spy@example.org", Subject: "Objet", Text: "t"},
		{To: "lea.martin@example.org", Subject: "Objet\nBcc: spy@example.org", Text: "t"},
	} {
		require.ErrorIs(t, s.Send(context.Background(), m), ErrPermanent)
	}
	assert.Zero(t, f.snapshot().connections, "refused before connecting")
}

func TestRenderHTML(t *testing.T) {
	got, err := renderHTML("Bonjour <script>alert(1)</script>\nligne deux\r\n\r\n" +
		"Lien : https://sos.example.org/suivi/a-b_c, merci.\n\n\n")
	require.NoError(t, err)
	assert.Contains(t, got, "<p>Bonjour &lt;script&gt;alert(1)&lt;/script&gt;<br>")
	assert.Contains(t, got, "ligne deux</p>")
	assert.Contains(t, got,
		`<p>Lien : <a href="https://sos.example.org/suivi/a-b_c">https://sos.example.org/suivi/a-b_c</a>, merci.</p>`)
	assert.Equal(t, 2, strings.Count(got, "<p>"))
	assert.NotContains(t, got, "<script>")
}
