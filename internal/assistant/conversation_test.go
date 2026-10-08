package assistant

import (
	"context"
	"strings"
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

	// Each call sends the whole history: past maxHistory, a question could
	// overflow the provider's context.
	long, err := s.Begin("s1", "alice", "", 0, nop)
	require.NoError(t, err)
	msg, err := UserText(strings.Repeat("x", maxHistory))
	require.NoError(t, err)
	long.History = []Message{msg}
	long.Exchanges = []Exchange{{Question: "Q"}}
	s.Finish(long)
	_, err = s.Begin("s1", "alice", long.ID, 0, nop)
	require.ErrorIs(t, err, ErrFull)
}

// « Nouvelle analyse », or a question after the analysis was erased or
// filled up: a request's analysis starts again; the page shows the newest.
func TestStoreForTicketAndDrop(t *testing.T) {
	s, _ := newTestStore()
	c, err := s.Begin("s1", "alice", "", 42, nop)
	require.NoError(t, err)
	c.Exchanges = make([]Exchange, MaxQuestions)
	s.Finish(c)
	again, err := s.Begin("s1", "alice", "", 42, nop)
	require.NoError(t, err, "a full analysis is never resumed")
	assert.NotEqual(t, c.ID, again.ID)
	assert.Empty(t, again.Exchanges)
	s.Finish(again)
	_, ok := s.ForTicket("s2", 42)
	assert.False(t, ok)
	got, ok := s.ForTicket("s1", 42)
	require.True(t, ok)
	assert.Equal(t, again.ID, got.ID, "the newest analysis")

	s.Drop("s1")
	_, ok = s.Find("s1", again.ID)
	assert.False(t, ok, "logout erases")
}

// Codex review: an answer in flight stops when its session ends (logout, a
// password reset, a deleted account); the deferred Abort frees its slot.
func TestStoreDropStopsTheSessionsAnswers(t *testing.T) {
	s, _ := newTestStore()
	stopped := map[string]bool{}
	alice, err := s.Begin("s1", "alice", "", 0, func() { stopped["alice"] = true })
	require.NoError(t, err)
	_, err = s.Begin("s2", "bob", "", 0, func() { stopped["bob"] = true })
	require.NoError(t, err)

	s.Drop("s1", "s3")
	assert.Equal(t, map[string]bool{"alice": true}, stopped, "only the ended sessions' answers stop")
	_, ok := s.Find("s1", alice.ID)
	assert.False(t, ok)
	s.Finish(alice)
	_, ok = s.Find("s1", alice.ID)
	assert.False(t, ok, "an answer ending after its session keeps nothing")
	_, err = s.Begin("s4", "alice", "", 0, nop)
	require.NoError(t, err, "the slot is free again")
}

// size is the number of conversations held, expired ones included.
func (s *Store) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.convs)
}

// Codex review: expired conversations go without waiting for a request, and
// the conversation an answer runs on stays, however long it was idle before.
func TestStoreRunSweepsExpiredConversations(t *testing.T) {
	s, clk := newTestStore()
	idle, err := s.Begin("s1", "alice", "", 0, nop)
	require.NoError(t, err)
	s.Finish(idle)
	resumed, err := s.Begin("s2", "bob", "", 0, nop)
	require.NoError(t, err)
	s.Finish(resumed)
	clk.t = clk.t.Add(29 * time.Minute)
	stopped := false
	resumed, err = s.Begin("s2", "bob", resumed.ID, 0, func() { stopped = true })
	require.NoError(t, err)
	clk.t = clk.t.Add(2 * time.Minute) // before Run reads the clock

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, time.Millisecond)
	}()
	require.Eventually(t, func() bool { return s.size() == 1 }, time.Second, time.Millisecond, "the idle conversation goes")
	cancel()
	<-done
	assert.False(t, stopped, "the answer in flight goes on")
	s.Finish(resumed)
	_, ok := s.Find("s2", resumed.ID)
	assert.True(t, ok, "the question reset its conversation's idle time")
}

// A late release (a deferred Abort after a panic, a double release) never
// frees the slot of the account's next answer.
func TestStoreReleasesOnlyItsOwnSlot(t *testing.T) {
	s, _ := newTestStore()
	first, err := s.Begin("s1", "alice", "", 0, nop)
	require.NoError(t, err)
	s.Finish(first)
	second, err := s.Begin("s1", "alice", first.ID, 0, nop)
	require.NoError(t, err)
	s.Abort(first)
	s.Finish(first)
	_, err = s.Begin("s2", "alice", "", 0, nop)
	require.ErrorIs(t, err, ErrBusy, "the second answer still holds the slot")
	s.Abort(second)
	_, err = s.Begin("s2", "alice", "", 0, nop)
	require.NoError(t, err)
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
