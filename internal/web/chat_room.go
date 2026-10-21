package web

import (
	"crypto/rand"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	errChatUnavailable = errors.New("chat session is not connected")
	errChatRateLimited = errors.New("chat send rate exceeded")
	errChatClosed      = errors.New("chat is closed")
)

type chatPerson struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	Avatar   string `json:"avatar"`
}

type chatMessage struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Avatar   string `json:"avatar"`
	Text     string `json:"text"`
	At       int64  `json:"at"`
}

type chatSnapshot struct {
	Cursor   string        `json:"cursor"`
	People   []chatPerson  `json:"people"`
	Messages []chatMessage `json:"messages"`
	Gap      bool          `json:"gap"`
}

type chatUpdate struct {
	Kind    string
	Message *chatMessage
	People  []chatPerson
}

type chatSubscription struct {
	session string
	ch      chan chatUpdate
	done    chan struct{}
}

type chatPresence struct {
	person chatPerson
	count  int
	until  time.Time
}

// chatRoom owns only short-lived delivery state; browser tabs own their history.
type chatRoom struct {
	mu       sync.Mutex
	now      func() time.Time
	epoch    string
	seq      uint64
	messages []chatMessage
	subs     map[*chatSubscription]struct{}
	sessions map[string]*chatPresence
	recent   map[string][]time.Time
	timer    *time.Timer
	closed   bool
}

func newChatRoom(now func() time.Time) *chatRoom {
	return &chatRoom{now: now, epoch: rand.Text(), subs: make(map[*chatSubscription]struct{}), sessions: make(map[string]*chatPresence), recent: make(map[string][]time.Time)}
}

func (c *chatRoom) cursorLocked() string { return c.epoch + ":" + strconv.FormatUint(c.seq, 10) }

func (c *chatRoom) subscribe(session string, person chatPerson, cursor string) (*chatSubscription, chatSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, chatSnapshot{}, errChatClosed
	}
	c.sweepLocked()
	before := c.peopleLocked()
	p := c.sessions[session]
	if p == nil {
		p = &chatPresence{}
		c.sessions[session] = p
	}
	p.person, p.count, p.until = person, p.count+1, time.Time{}
	sub := &chatSubscription{session: session, ch: make(chan chatUpdate, 16), done: make(chan struct{})}
	snapshot := chatSnapshot{Cursor: c.cursorLocked(), People: c.peopleLocked(), Messages: []chatMessage{}}
	if cursor != "" {
		epoch, number, ok := strings.Cut(cursor, ":")
		n, err := strconv.ParseUint(number, 10, 64)
		if !ok || err != nil || epoch != c.epoch || n > c.seq || n < c.seq-uint64(len(c.messages)) {
			snapshot.Gap = true
		} else {
			offset := int(n - (c.seq - uint64(len(c.messages))))
			snapshot.Messages = slices.Clone(c.messages[offset:])
		}
	}
	// The snapshot and live registration share the lock: no message can fall between.
	c.subs[sub] = struct{}{}
	if !slices.Equal(before, snapshot.People) {
		c.broadcastLocked(chatUpdate{Kind: "chat-presence", People: snapshot.People})
	}
	c.scheduleLocked()
	return sub, snapshot, nil
}

func (c *chatRoom) send(session string, person chatPerson, text string) (chatMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return chatMessage{}, errChatClosed
	}
	c.sweepLocked()
	p := c.sessions[session]
	if p == nil || p.count == 0 || p.person.Username != person.Username {
		return chatMessage{}, errChatUnavailable
	}
	now := c.now()
	times := c.recent[person.Username]
	if len(times) >= 5 {
		return chatMessage{}, errChatRateLimited
	}
	c.recent[person.Username] = append(times, now)
	c.seq++
	msg := chatMessage{ID: c.cursorLocked(), Username: person.Username, Name: person.Name, Avatar: person.Avatar, Text: text, At: now.Unix()}
	c.messages = append(c.messages, msg)
	if len(c.messages) > 256 {
		c.messages = slices.Delete(c.messages, 0, len(c.messages)-256)
	}
	c.broadcastLocked(chatUpdate{Kind: "chat-message", Message: &msg})
	c.scheduleLocked()
	return msg, nil
}

func (c *chatRoom) peopleLocked() []chatPerson {
	byName := make(map[string]chatPerson)
	for _, p := range c.sessions {
		byName[p.person.Username] = p.person
	}
	people := make([]chatPerson, 0, len(byName))
	for _, p := range byName {
		people = append(people, p)
	}
	sort.Slice(people, func(i, j int) bool { return people[i].Username < people[j].Username })
	return people
}

func (c *chatRoom) dropLocked(sub *chatSubscription) {
	if _, ok := c.subs[sub]; !ok {
		return
	}
	delete(c.subs, sub)
	close(sub.done)
	if p := c.sessions[sub.session]; p != nil {
		p.count--
		if p.count == 0 {
			p.until = c.now().Add(30 * time.Second)
		}
	}
}

func (c *chatRoom) broadcastLocked(update chatUpdate) {
	for sub := range c.subs {
		select {
		case sub.ch <- update:
		default:
			c.dropLocked(sub)
		}
	}
}

func (c *chatRoom) unsubscribe(sub *chatSubscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(sub)
	c.scheduleLocked()
}

func (c *chatRoom) revoke(sessions ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := c.peopleLocked()
	for sub := range c.subs {
		if slices.Contains(sessions, sub.session) {
			c.dropLocked(sub)
		}
	}
	for _, session := range sessions {
		delete(c.sessions, session)
	}
	people := c.peopleLocked()
	if !slices.Equal(before, people) {
		c.broadcastLocked(chatUpdate{Kind: "chat-presence", People: people})
	}
	c.scheduleLocked()
}

func (c *chatRoom) sweepLocked() {
	now := c.now()
	before := c.peopleLocked()
	for session, p := range c.sessions {
		if p.count == 0 && !now.Before(p.until) {
			delete(c.sessions, session)
		}
	}
	for username, times := range c.recent {
		cut := 0
		for cut < len(times) && !now.Before(times[cut].Add(10*time.Second)) {
			cut++
		}
		times = slices.Delete(times, 0, cut)
		if len(times) == 0 {
			delete(c.recent, username)
		} else {
			c.recent[username] = times
		}
	}
	cut := 0
	for cut < len(c.messages) && c.messages[cut].At <= now.Add(-60*time.Second).Unix() {
		cut++
	}
	c.messages = slices.Delete(c.messages, 0, cut)
	people := c.peopleLocked()
	if !slices.Equal(before, people) {
		c.broadcastLocked(chatUpdate{Kind: "chat-presence", People: people})
	}
}

func (c *chatRoom) scheduleLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.closed {
		return
	}
	var next time.Time
	for _, p := range c.sessions {
		if p.count == 0 && (next.IsZero() || p.until.Before(next)) {
			next = p.until
		}
	}
	// Also discard delivery content when the room goes completely quiet.
	if len(c.messages) > 0 {
		expiry := time.Unix(c.messages[0].At, 0).Add(60 * time.Second)
		if next.IsZero() || expiry.Before(next) {
			next = expiry
		}
	}
	if !next.IsZero() {
		c.timer = time.AfterFunc(max(0, next.Sub(c.now())), c.sweep)
	}
}

func (c *chatRoom) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.sweepLocked()
	c.scheduleLocked()
}

func (c *chatRoom) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
	}
	for sub := range c.subs {
		c.dropLocked(sub)
	}
	clear(c.sessions)
	clear(c.recent)
	c.messages = nil
}
