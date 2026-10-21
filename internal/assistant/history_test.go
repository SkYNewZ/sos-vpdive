package assistant

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func newTestStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	keys, err := secure.NewKeys(bytes.Repeat([]byte{1}, 32))
	require.NoError(t, err)
	for _, account := range []string{"alice", "bob"} {
		_, err = db.ExecContext(t.Context(), `INSERT INTO accounts(username,name,role,password_hash,must_change_password,created_at) VALUES (?,?,'committee','test',0,0)`, account, keys.SealString(account))
		require.NoError(t, err)
	}
	clk := &clock{time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)}
	return NewStore(db, keys, clk.now), clk
}
func nop(error) {}
func saved(t *testing.T, s *Store, account string, ticket int64) Conversation {
	t.Helper()
	c, err := s.Begin(t.Context(), "s1", account, "", ticket, nop)
	require.NoError(t, err)
	c.Exchanges = []Exchange{{Question: "Question privée", Answer: "Réponse privée"}}
	msg, err := UserText("Contexte privé")
	require.NoError(t, err)
	c.History = []Message{msg}
	c.People = []Person{{Ref: "m1", Name: "Personne synthétique", Email: "private@example.org"}}
	c.Emails = []string{"private@example.org"}
	c, err = s.Finish(t.Context(), c)
	require.NoError(t, err)
	return c
}
func TestHistoryEncryptedAndSurvivesReopen(t *testing.T) {
	s, _ := newTestStore(t)
	c := saved(t, s, "alice", 0)
	var title, transcript, model []byte
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT title,transcript,context FROM assistant_conversations WHERE id=?`, c.ID).Scan(&title, &transcript, &model))
	for _, b := range [][]byte{title, transcript, model} {
		for _, text := range []string{"Question privée", "Réponse privée", "Contexte privé", "private@example.org", c.Title} {
			assert.NotContains(t, string(b), text)
		}
	}
	reopened := NewStore(s.db, s.keys, s.now)
	got, err := reopened.Find(t.Context(), "alice", c.ID)
	require.NoError(t, err)
	assert.Equal(t, c.Exchanges, got.Exchanges)
	assert.Empty(t, got.History, "reading an archive never loads provider context")
	next, err := reopened.Begin(t.Context(), "new-session", "alice", c.ID, 0, nop)
	require.NoError(t, err)
	assert.Equal(t, c.History, next.History)
	assert.Equal(t, c.Emails, next.Emails)
	_, err = reopened.Find(t.Context(), "bob", c.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = reopened.Begin(t.Context(), "s2", "bob", c.ID, 0, nop)
	require.ErrorIs(t, err, ErrNotFound)
}
func TestHistoryResumeBoundaries(t *testing.T) {
	s, clk := newTestStore(t)
	c := saved(t, s, "alice", 0)
	clk.t = clk.t.Add(30*time.Minute - time.Nanosecond)
	got, err := s.Find(t.Context(), "alice", c.ID)
	require.NoError(t, err)
	assert.True(t, got.CanResume(clk.now()))
	clk.t = clk.t.Add(time.Nanosecond)
	got, err = s.Find(t.Context(), "alice", c.ID)
	require.NoError(t, err)
	assert.False(t, got.CanResume(clk.now()))
	_, err = s.Begin(t.Context(), "s1", "alice", c.ID, 0, nop)
	require.ErrorIs(t, err, ErrExpired)
	clk.t = c.Created.Add(7*24*time.Hour - time.Nanosecond)
	_, err = s.Find(t.Context(), "alice", c.ID)
	require.NoError(t, err)
	clk.t = clk.t.Add(time.Nanosecond)
	_, err = s.Find(t.Context(), "alice", c.ID)
	require.ErrorIs(t, err, ErrNotFound)
}
func TestHistoryAbsoluteLimitKeepsLastAnswer(t *testing.T) {
	s, clk := newTestStore(t)
	c := saved(t, s, "alice", 0)
	for range 4 {
		clk.t = clk.t.Add(25 * time.Minute)
		next, err := s.Begin(t.Context(), "s1", "alice", c.ID, 0, nop)
		require.NoError(t, err)
		next.Exchanges = append(next.Exchanges, Exchange{Question: "Suite", Answer: "Oui"})
		c, err = s.Finish(t.Context(), next)
		require.NoError(t, err)
	}
	clk.t = c.Created.Add(119 * time.Minute)
	next, err := s.Begin(t.Context(), "s1", "alice", c.ID, 0, nop)
	require.NoError(t, err)
	clk.t = clk.t.Add(2 * time.Minute)
	next.Exchanges = append(next.Exchanges, Exchange{Question: "Dernière", Answer: "Enregistrée"})
	_, err = s.Finish(t.Context(), next)
	require.NoError(t, err)
	got, err := s.Find(t.Context(), "alice", c.ID)
	require.NoError(t, err)
	assert.Len(t, got.Exchanges, 6)
	assert.False(t, got.CanResume(clk.now()))
	_, err = s.Begin(t.Context(), "s1", "alice", c.ID, 0, nop)
	require.ErrorIs(t, err, ErrExpired)
}
func TestHistoryAbortDoesNotExtendActivity(t *testing.T) {
	s, clk := newTestStore(t)
	c := saved(t, s, "alice", 0)
	clk.t = clk.t.Add(29 * time.Minute)
	next, err := s.Begin(t.Context(), "s1", "alice", c.ID, 0, nop)
	require.NoError(t, err)
	require.NoError(t, s.Abort(t.Context(), next))
	clk.t = clk.t.Add(time.Minute)
	got, err := s.Find(t.Context(), "alice", c.ID)
	require.NoError(t, err)
	assert.False(t, got.CanResume(clk.now()))
	empty, err := s.Begin(t.Context(), "s1", "bob", "", 0, nop)
	require.NoError(t, err)
	require.NoError(t, s.Abort(t.Context(), empty))
	_, err = s.Find(t.Context(), "bob", empty.ID)
	require.ErrorIs(t, err, ErrNotFound)
}
func TestHistoryReservationsAndLogout(t *testing.T) {
	s, _ := newTestStore(t)
	c := saved(t, s, "alice", 0)
	next, err := s.Begin(t.Context(), "s1", "alice", c.ID, 0, nop)
	require.NoError(t, err)
	_, err = s.Begin(t.Context(), "s2", "alice", "", 0, nop)
	require.ErrorIs(t, err, ErrBusy)
	stopped := false
	other, err := s.Begin(t.Context(), "s2", "bob", "", 0, func(error) { stopped = true })
	require.NoError(t, err)
	s.Drop("s1")
	_, err = s.Finish(t.Context(), next)
	require.ErrorIs(t, err, ErrSessionEnded)
	assert.False(t, stopped)
	got, err := s.Find(t.Context(), "alice", c.ID)
	require.NoError(t, err)
	assert.Len(t, got.Exchanges, 1)
	second, err := s.Begin(t.Context(), "new-session", "alice", c.ID, 0, nop)
	require.NoError(t, err)
	require.NoError(t, s.Abort(t.Context(), next))
	_, err = s.Finish(t.Context(), next)
	require.Error(t, err)
	_, err = s.Begin(t.Context(), "s3", "alice", "", 0, nop)
	require.ErrorIs(t, err, ErrBusy)
	require.NoError(t, s.Abort(t.Context(), second))
	require.NoError(t, s.Abort(t.Context(), other))
}
func TestHistoryLimits(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "questions", true: "bytes"}[oversized], func(t *testing.T) {
			s, _ := newTestStore(t)
			c, err := s.Begin(t.Context(), "s1", "alice", "", 0, nil)
			require.NoError(t, err)
			c.Exchanges = make([]Exchange, 20)
			if oversized {
				c.Exchanges = c.Exchanges[:1]
				m, e := UserText(strings.Repeat("x", 256<<10))
				require.NoError(t, e)
				c.History = []Message{m}
			}
			_, err = s.Finish(t.Context(), c)
			require.NoError(t, err)
			_, err = s.Begin(t.Context(), "s1", "alice", c.ID, 0, nop)
			require.ErrorIs(t, err, ErrFull)
			got, err := s.Find(t.Context(), "alice", c.ID)
			require.NoError(t, err)
			assert.False(t, got.CanResume(s.now()))
		})
	}
}
func TestHistoryLateFinishCannotRestoreErasedRow(t *testing.T) {
	s, _ := newTestStore(t)
	c := saved(t, s, "alice", 0)
	next, err := s.Begin(t.Context(), "s1", "alice", c.ID, 0, nop)
	require.NoError(t, err)
	require.NoError(t, store.Tx(t.Context(), s.db, "test.erase", EraseAllTx))
	_, err = s.Finish(t.Context(), next)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = NewStore(s.db, s.keys, s.now).Find(t.Context(), "alice", c.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestHistoryListBoundedStableAndPrivate(t *testing.T) {
	s, clk := newTestStore(t)
	var last string
	for range 31 {
		last = saved(t, s, "alice", 0).ID
	}
	saved(t, s, "bob", 0)
	items, more, err := s.List(t.Context(), "alice", 0)
	require.NoError(t, err)
	require.Len(t, items, 30)
	assert.True(t, more)
	assert.Equal(t, last, items[0].ID)
	items, more, err = s.List(t.Context(), "alice", 1)
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.False(t, more)
	clk.t = clk.t.Add(30 * time.Minute)
	items, _, err = s.List(t.Context(), "alice", 0)
	require.NoError(t, err)
	assert.False(t, items[0].CanResume)
	clk.t = clk.t.Add(7 * 24 * time.Hour)
	items, _, err = s.List(t.Context(), "alice", 0)
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestHistoryTitleClaimAndLateUpdate(t *testing.T) {
	s, _ := newTestStore(t)
	c := saved(t, s, "alice", 0)
	claimed, err := s.ClaimTitle(t.Context(), "bob", c.ID)
	require.NoError(t, err)
	assert.False(t, claimed)
	claimed, err = s.ClaimTitle(t.Context(), "alice", c.ID)
	require.NoError(t, err)
	assert.True(t, claimed)
	claimed, err = s.ClaimTitle(t.Context(), "alice", c.ID)
	require.NoError(t, err)
	assert.False(t, claimed)
	require.NoError(t, s.SetTitle(t.Context(), "alice", c.ID, "Titre privé"))
	got, err := s.Find(t.Context(), "alice", c.ID)
	require.NoError(t, err)
	assert.Equal(t, "Titre privé", got.Title)
	assert.True(t, c.Seen.Equal(got.Seen), "title updates do not extend activity")
	require.NoError(t, store.Tx(t.Context(), s.db, "test.erase", EraseAllTx))
	require.NoError(t, s.SetTitle(t.Context(), "alice", c.ID, "Late title"))
	_, err = s.Find(t.Context(), "alice", c.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestHistoryForTicketKeepsNewestAnalysis(t *testing.T) {
	s, _ := newTestStore(t)
	first := saved(t, s, "alice", 42)
	newest := saved(t, s, "alice", 42)
	got, err := s.ForTicket(t.Context(), "alice", 42)
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, got.ID)
	assert.Equal(t, newest.ID, got.ID)
	_, err = s.ForTicket(t.Context(), "bob", 42)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestHistoryAccountDeletionAndRetention(t *testing.T) {
	s, clk := newTestStore(t)
	c := saved(t, s, "alice", 0)
	_, err := s.db.ExecContext(t.Context(), `DELETE FROM accounts WHERE username='alice'`)
	require.NoError(t, err)
	_, err = s.Find(t.Context(), "alice", c.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO accounts(username,name,role,password_hash,must_change_password,created_at) VALUES('alice',?,'committee','test',0,0)`, s.keys.SealString("New account"))
	require.NoError(t, err)
	_, err = s.Find(t.Context(), "alice", c.ID)
	require.ErrorIs(t, err, ErrNotFound)
	old := saved(t, s, "alice", 0)
	clk.t = clk.t.Add(24 * time.Hour)
	recent := saved(t, s, "bob", 0)
	clk.t = old.Created.Add(7 * 24 * time.Hour)
	require.NoError(t, PurgeHistory(t.Context(), s.db, clk.now()))
	_, err = s.Find(t.Context(), "alice", old.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.Find(t.Context(), "bob", recent.ID)
	require.NoError(t, err)
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM assistant_conversations`).Scan(&count))
	assert.Equal(t, 1, count)
}
