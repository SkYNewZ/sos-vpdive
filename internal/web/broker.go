package web

import (
	"sync"

	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// subscriberBuffer is how many changes a stream may lag behind before it is
// dropped; the browser then reconnects and refreshes the whole board.
const subscriberBuffer = 16

// Broker fans request changes out to the committee's open boards (spec
// §4.2). One instance runs, so it lives in memory. A change is a type and a
// request id: it carries no personal data.
type Broker struct {
	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	closed bool
}

// subscriber is one open event stream.
type subscriber struct {
	session  string // session token hash, to close the stream at logout
	username string // to close the streams of a revoked account
	ch       chan tickets.Change
	done     chan struct{} // closed when the broker drops the stream
}

// NewBroker returns a broker without subscribers.
func NewBroker() *Broker {
	return &Broker{subs: map[*subscriber]struct{}{}}
}

// Publish sends c to every open stream. It never blocks: a stream whose
// buffer is full is dropped.
func (b *Broker) Publish(c tickets.Change) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		select {
		case sub.ch <- c:
		default:
			b.drop(sub)
		}
	}
}

// Close ends every stream and refuses new ones: the server is stopping.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for sub := range b.subs {
		b.drop(sub)
	}
}

func (b *Broker) subscribe(sessionHash []byte, username string) (*subscriber, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, false
	}
	sub := &subscriber{
		session: string(sessionHash), username: username,
		ch: make(chan tickets.Change, subscriberBuffer), done: make(chan struct{}),
	}
	b.subs[sub] = struct{}{}
	return sub, true
}

func (b *Broker) unsubscribe(sub *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drop(sub)
}

// disconnect ends the streams that match.
func (b *Broker) disconnect(match func(*subscriber) bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		if match(sub) {
			b.drop(sub)
		}
	}
}

// drop removes sub and signals its stream once. Callers hold b.mu.
func (b *Broker) drop(sub *subscriber) {
	if _, ok := b.subs[sub]; ok {
		delete(b.subs, sub)
		close(sub.done)
	}
}
