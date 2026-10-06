package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

func ended(sub *subscriber) bool {
	select {
	case <-sub.done:
		return true
	default:
		return false
	}
}

func TestBrokerDeliversChanges(t *testing.T) {
	b := NewBroker()
	sub, ok := b.subscribe([]byte("session-1"), "alice")
	require.True(t, ok)
	change := tickets.Change{Type: tickets.ChangeCreated, TicketID: 7}
	b.Publish(change)
	assert.Equal(t, change, <-sub.ch)
	b.unsubscribe(sub)
	assert.True(t, ended(sub))
	b.Publish(change) // no receiver left: must not block
}

func TestBrokerDropsSlowSubscribers(t *testing.T) {
	b := NewBroker()
	sub, ok := b.subscribe([]byte("session-1"), "alice")
	require.True(t, ok)
	for i := range subscriberBuffer {
		b.Publish(tickets.Change{Type: tickets.ChangeUpdated, TicketID: int64(i)})
	}
	assert.False(t, ended(sub), "a full buffer is still fine")
	b.Publish(tickets.Change{Type: tickets.ChangeUpdated, TicketID: 99})
	assert.True(t, ended(sub), "one more change drops the subscriber: it reconnects and refreshes")
}

func TestBrokerDisconnectsBySessionAndByAccount(t *testing.T) {
	b := NewBroker()
	a1, _ := b.subscribe([]byte("session-1"), "alice")
	a2, _ := b.subscribe([]byte("session-2"), "alice")
	bob, _ := b.subscribe([]byte("session-3"), "bob")

	b.disconnect(func(s *subscriber) bool { return s.session == "session-1" })
	assert.True(t, ended(a1))
	assert.False(t, ended(a2))

	b.disconnect(func(s *subscriber) bool { return s.username == "alice" })
	assert.True(t, ended(a2))
	assert.False(t, ended(bob))
}

func TestBrokerClose(t *testing.T) {
	b := NewBroker()
	sub, _ := b.subscribe([]byte("session-1"), "alice")
	b.Close()
	assert.True(t, ended(sub))
	_, ok := b.subscribe([]byte("session-2"), "alice")
	assert.False(t, ok, "no new stream once the server stops")
}
