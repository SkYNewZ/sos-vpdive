package assistant

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestStore() (*Store, *clock) {
	c := &clock{t: time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)}
	return NewStore(c.now), c
}

func nop() {}

// Codex review: an erasure must leave no conversation that still holds the
// erased person, and must stop the answers running.
func TestStoreDropAll(t *testing.T) {
	s, _ := newTestStore()
	c, err := s.Begin("s1", "alice", "", 0, nop)
	require.NoError(t, err)
	s.Finish(c)
	stopped := false
	running, err := s.Begin("s2", "bob", "", 0, func() { stopped = true })
	require.NoError(t, err)

	s.DropAll()
	assert.True(t, stopped, "the answer in flight is canceled")
	_, ok := s.Find("s1", c.ID)
	assert.False(t, ok)
	s.Finish(running)
	_, ok = s.Find("s2", running.ID)
	assert.False(t, ok, "an answer ending after the erasure keeps nothing")
	_, err = s.Begin("s2", "bob", "", 0, nop)
	require.NoError(t, err, "the slot is free again")
}

func TestStoreBeginFinish(t *testing.T) {
	s, _ := newTestStore()
	c, err := s.Begin("s1", "alice", "", 0, nop)
	require.NoError(t, err)
	require.NotEmpty(t, c.ID)
	c.Exchanges = append(c.Exchanges, Exchange{Question: "Q", Answer: "R"})
	c.AddPerson(Person{Name: "Léa Martin", Email: "lea@example.org"})
	s.Finish(c)

	got, ok := s.Find("s1", c.ID)
	require.True(t, ok)
	assert.Len(t, got.Exchanges, 1)
	assert.Equal(t, "m1", got.People[0].Ref)
	_, ok = s.Find("s2", c.ID)
	assert.False(t, ok, "another session never sees it")
	_, err = s.Begin("s2", "bob", c.ID, 0, nop)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStoreOneAnswerPerAccount(t *testing.T) {
	s, _ := newTestStore()
	c, err := s.Begin("s1", "alice", "", 0, nop)
	require.NoError(t, err)
	_, err = s.Begin("s3", "alice", "", 0, nop)
	require.ErrorIs(t, err, ErrBusy, "another tab of the same account waits")
	_, err = s.Begin("s2", "bob", "", 0, nop)
	require.NoError(t, err, "another account goes on")
	s.Abort(c)
	_, err = s.Begin("s1", "alice", c.ID, 0, nop)
	require.NoError(t, err)
}

func TestStoreExpiry(t *testing.T) {
	s, clk := newTestStore()
	c, err := s.Begin("s1", "alice", "", 0, nop)
	require.NoError(t, err)
	s.Finish(c)
	clk.t = clk.t.Add(31 * time.Minute)
	_, ok := s.Find("s1", c.ID)
	assert.False(t, ok, "30 minutes without a question")

	c, err = s.Begin("s1", "alice", "", 0, nop)
	require.NoError(t, err)
	s.Finish(c)
	for range 5 {
		clk.t = clk.t.Add(25 * time.Minute)
		if c, err = s.Begin("s1", "alice", c.ID, 0, nop); err != nil {
			break
		}
		s.Finish(c)
	}
	require.ErrorIs(t, err, ErrNotFound, "two hours at most, however active")
}

func TestStoreFull(t *testing.T) {
	s, _ := newTestStore()
	c, err := s.Begin("s1", "alice", "", 0, nop)
	require.NoError(t, err)
	c.Exchanges = make([]Exchange, MaxQuestions)
	s.Finish(c)
	_, err = s.Begin("s1", "alice", c.ID, 0, nop)
	require.ErrorIs(t, err, ErrFull)
}

func TestStoreForTicketAndDrop(t *testing.T) {
	s, _ := newTestStore()
	c, err := s.Begin("s1", "alice", "", 42, nop)
	require.NoError(t, err)
	s.Finish(c)
	again, err := s.Begin("s1", "alice", "", 42, nop)
	require.NoError(t, err)
	assert.Equal(t, c.ID, again.ID, "one analysis per request and session")
	s.Abort(again)
	_, ok := s.ForTicket("s2", 42)
	assert.False(t, ok)
	got, ok := s.ForTicket("s1", 42)
	require.True(t, ok)
	assert.Equal(t, c.ID, got.ID)

	s.Drop("s1")
	_, ok = s.Find("s1", c.ID)
	assert.False(t, ok, "logout erases")
}

func TestAddPersonOnce(t *testing.T) {
	var c Conversation
	assert.Equal(t, "m1", c.AddPerson(Person{Email: "a@example.org"}))
	assert.Equal(t, "m2", c.AddPerson(Person{Email: "b@example.org"}))
	assert.Equal(t, "m1", c.AddPerson(Person{Email: "a@example.org"}))
	p, ok := c.Person("m2")
	require.True(t, ok)
	assert.Equal(t, "b@example.org", p.Email)
	_, ok = c.Person("m9")
	assert.False(t, ok)
}

func TestStoreNilStopStillBusy(t *testing.T) {
	s, _ := newTestStore()
	_, err := s.Begin("s1", "alice", "", 0, nil)
	require.NoError(t, err)
	_, err = s.Begin("s1", "alice", "", 0, nil)
	require.ErrorIs(t, err, ErrBusy, "a nil stop still holds the slot")
	assert.NotPanics(t, s.DropAll)
}
