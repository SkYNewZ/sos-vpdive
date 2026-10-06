package tickets

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sosvpdive "github.com/SkYNewZ/sos-vpdive"
	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/blobs"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

const (
	clubAddress   = "club@example.org"
	memberAddress = "lea.martin@example.org"
	testBody      = "Mon carnet affiche moins de plongées que prévu depuis la sortie."
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// fakeSender records what the outbox delivers.
type fakeSender struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (f *fakeSender) Send(_ context.Context, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

type env struct {
	store    *Store
	db       *sql.DB
	keys     *secure.Keys
	clock    *testClock
	blobs    blobs.Store
	outbox   *mail.Outbox
	sender   *fakeSender
	members  *members.Store
	logs     *bytes.Buffer
	mu       sync.Mutex
	changes  []Change
	accounts map[string]admins.Account
}

func newTestStore(t *testing.T, opts ...func(*Deps)) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, store.FileName))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	keys, err := secure.NewKeys(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	require.NoError(t, store.CheckKey(ctx, db, keys))
	catalog, err := LoadCatalog(sosvpdive.Content)
	require.NoError(t, err)
	objects, err := blobs.NewDir(filepath.Join(dir, "captures"))
	require.NoError(t, err)

	e := &env{
		db: db, keys: keys, blobs: objects, sender: &fakeSender{}, logs: &bytes.Buffer{},
		clock: &testClock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)},
		accounts: map[string]admins.Account{
			"alice": {Username: "alice", Name: "Alice", Role: "Présidente"},
			"bob":   {Username: "bob", Name: "Bob", Role: "Trésorier"},
		},
	}
	e.outbox = mail.NewOutbox(db, keys, e.clock.now)
	e.members = members.NewStore(db, keys, e.clock.now)
	d := Deps{
		DB: db, Keys: keys, Catalog: catalog, Members: e.members, Outbox: e.outbox, Blobs: objects,
		Account: e.account,
		BaseURL: &url.URL{Scheme: "https", Host: "sos.example.org"}, AdminBaseURL: &url.URL{Scheme: "https", Host: "comite.example.org"},
		ClubEmail: clubAddress, RetentionDays: 365, Now: e.clock.now,
		Logger:   slog.New(slog.NewJSONHandler(e.logs, nil)),
		OnChange: e.record,
	}
	for _, opt := range opts {
		opt(&d)
	}
	e.store = NewStore(d)
	return e
}

func (e *env) account(username string) (admins.Account, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.accounts[username]
	return a, ok
}

func (e *env) record(c Change) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.changes = append(e.changes, c)
}

func (e *env) recorded() []Change {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Change(nil), e.changes...)
}

// mails delivers the due outbox rows and returns the messages sent since the
// previous call.
func (e *env) mails(t *testing.T) []mail.Message {
	t.Helper()
	_, err := e.outbox.SendDue(context.Background(), e.sender)
	require.NoError(t, err)
	e.sender.mu.Lock()
	defer e.sender.mu.Unlock()
	out := e.sender.sent
	e.sender.sent = nil
	return out
}

func submission(t *testing.T) Submission {
	t.Helper()
	key, err := secure.NewToken()
	require.NoError(t, err)
	return Submission{
		FormKey: key, FirstName: "Léa", LastName: "Martin", Email: memberAddress,
		Fields:      Fields{Category: "carnet", Values: map[string]string{}},
		Description: testBody,
	}
}

// submit files a request and returns its id and reference.
func (e *env) submit(t *testing.T, captures ...Upload) (int64, string) {
	t.Helper()
	sub := submission(t)
	sub.Captures = captures
	out, err := e.store.Submit(context.Background(), sub, nil)
	require.NoError(t, err)
	return e.idOf(t, out.Ref), out.Ref
}

func (e *env) idOf(t *testing.T, ref string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT id FROM tickets WHERE ref = ?`, ref).Scan(&id))
	return id
}

func (e *env) status(t *testing.T, id int64) (Status, string) {
	t.Helper()
	var (
		s        Status
		assignee sql.NullString
	)
	require.NoError(t, e.db.QueryRowContext(context.Background(), `SELECT status, assignee FROM tickets WHERE id = ?`, id).Scan(&s, &assignee))
	return s, assignee.String
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

func (e *env) objects(t *testing.T) []blobs.Object {
	t.Helper()
	list, err := e.blobs.List(context.Background())
	require.NoError(t, err)
	return list
}

var trackingPattern = regexp.MustCompile(`https://sos\.example\.org/suivi/([A-Za-z0-9_-]{43})`)

// tokenOf reads the tracking token from the acknowledgement mail.
func tokenOf(t *testing.T, msgs []mail.Message) string {
	t.Helper()
	for _, m := range msgs {
		if m.To == memberAddress {
			if found := trackingPattern.FindStringSubmatch(m.Text); found != nil {
				return found[1]
			}
		}
	}
	t.Fatal("no tracking link in the mails")
	return ""
}

func png() Upload { return Upload{Data: []byte("png bytes of a screenshot"), MIME: "image/png"} }

func TestCleanText(t *testing.T) {
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"crlf becomes lf", "Bonjour,\r\nmon carnet est faux.\r\n", "Bonjour,\nmon carnet est faux.", true},
		{"lone cr becomes lf", "ligne une\rligne deux et la suite", "ligne une\nligne deux et la suite", true},
		{"19 runes refused", strings.Repeat("é", 19), strings.Repeat("é", 19), false},
		{"20 runes accepted", strings.Repeat("é", 20), strings.Repeat("é", 20), true},
		{"emoji count as one rune", strings.Repeat("🤿", 20), strings.Repeat("🤿", 20), true},
		{"spaces trimmed before counting", "   " + strings.Repeat("a", 19) + "  \r\n", strings.Repeat("a", 19), false},
	}
	for _, c := range cases {
		got, ok := CleanText(c.in, DescriptionMin, DescriptionMax)
		assert.Equal(t, c.want, got, c.name)
		assert.Equal(t, c.ok, ok, c.name)
	}

	// 4 000 characters with CRLF breaks fit (4 039 before conversion); one more does not.
	line := strings.Repeat("x", 99) + "\r\n" // 100 runes once CRLF is LF
	text := strings.Repeat(line, 39) + strings.Repeat("y", 100)
	_, ok := CleanText(text, DescriptionMin, DescriptionMax)
	assert.True(t, ok)
	_, ok = CleanText(text+"z", DescriptionMin, DescriptionMax)
	assert.False(t, ok)
}

func TestStatusLabels(t *testing.T) {
	assert.Equal(t, "À traiter", StatusTodo.Label())
	assert.Equal(t, "En attente de l'adhérent", StatusWaiting.Label())
	assert.Equal(t, "En attente de ta réponse", StatusWaiting.MemberLabel())
	assert.Equal(t, "Fait", StatusDone.MemberLabel())
	for _, s := range []Status{StatusTodo, StatusInProgress, StatusWaiting} {
		assert.True(t, s.Open(), s)
	}
	assert.False(t, StatusDone.Open())
	assert.False(t, StatusDraft.Open())
}

func TestAccountName(t *testing.T) {
	e := newTestStore(t)
	assert.Equal(t, "Alice (Présidente)", e.store.AccountName("alice"))
	assert.Equal(t, "zoe", e.store.AccountName("zoe"))
}
