package web

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testChatRoom(t *testing.T) (*chatRoom, *testClock) {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	room := newChatRoom(clock.now)
	t.Cleanup(room.close)
	return room, clock
}

var chatAlice = chatPerson{Username: "alice", Name: "Alice", Avatar: "data:image/png;base64,AA=="}
var chatBob = chatPerson{Username: "bob", Name: "Bob"}

func TestChatRoomResume(t *testing.T) {
	room, clock := testChatRoom(t)
	a, initial, err := room.subscribe("a", chatAlice, "")
	require.NoError(t, err)
	require.Empty(t, initial.Messages)
	first, err := room.send("a", chatAlice, "hello")
	require.NoError(t, err)
	second, err := room.send("a", chatAlice, "world")
	require.NoError(t, err)
	_, fresh, err := room.subscribe("b", chatBob, "")
	require.NoError(t, err)
	assert.Empty(t, fresh.Messages)
	_, resumed, err := room.subscribe("a", chatAlice, first.ID)
	require.NoError(t, err)
	require.Len(t, resumed.Messages, 1)
	assert.Equal(t, second.ID, resumed.Messages[0].ID)
	assert.False(t, resumed.Gap)
	clock.advance(61 * time.Second)
	_, expired, err := room.subscribe("a", chatAlice, first.ID)
	require.NoError(t, err)
	assert.True(t, expired.Gap)
	_, latest, err := room.subscribe("a", chatAlice, second.ID)
	require.NoError(t, err)
	assert.False(t, latest.Gap, "nothing missed after the last received message")
	for _, cursor := range []string{"other:1", "invalid", room.epoch + ":9999"} {
		_, snapshot, err := room.subscribe("a", chatAlice, cursor)
		require.NoError(t, err)
		assert.True(t, snapshot.Gap)
	}
	room.unsubscribe(a)
}

func TestChatRoomResumeAfterCapacityEviction(t *testing.T) {
	room, clock := testChatRoom(t)
	_, initial, err := room.subscribe("a", chatAlice, "")
	require.NoError(t, err)
	// Many distinct participants avoid quota effects while filling the shared buffer.
	for i := range 257 {
		session := string(rune(i + 1000))
		person := chatPerson{Username: session}
		sub, _, err := room.subscribe(session, person, "")
		require.NoError(t, err)
		_, err = room.send(session, person, "message")
		require.NoError(t, err)
		room.unsubscribe(sub)
	}
	_, snapshot, err := room.subscribe("a", chatAlice, initial.Cursor)
	require.NoError(t, err)
	assert.True(t, snapshot.Gap)
	assert.Empty(t, snapshot.Messages)
	clock.advance(61 * time.Second)
	room.sweep()
	assert.Empty(t, room.messages)
}

func TestChatRoomPresence(t *testing.T) {
	room, clock := testChatRoom(t)
	a, _, err := room.subscribe("a", chatAlice, "")
	require.NoError(t, err)
	other, snapshot, err := room.subscribe("a2", chatAlice, "")
	require.NoError(t, err)
	assert.Len(t, snapshot.People, 1)
	room.unsubscribe(a)
	room.unsubscribe(other)
	clock.advance(29 * time.Second)
	room.sweep()
	assert.Len(t, room.peopleLocked(), 1)
	clock.advance(time.Second)
	room.sweep()
	assert.Empty(t, room.peopleLocked())
	_, _, err = room.subscribe("a", chatAlice, "")
	require.NoError(t, err)
	_, _, err = room.subscribe("a2", chatAlice, "")
	require.NoError(t, err)
	room.revoke("a")
	assert.Len(t, room.peopleLocked(), 1, "the other valid session stays online")
	room.revoke("a2")
	assert.Empty(t, room.peopleLocked(), "revocation has no grace")
}

func TestChatRoomRateLimit(t *testing.T) {
	room, clock := testChatRoom(t)
	_, _, err := room.subscribe("a", chatAlice, "")
	require.NoError(t, err)
	_, _, err = room.subscribe("a2", chatAlice, "")
	require.NoError(t, err)
	for i := range 5 {
		session := "a"
		if i%2 == 1 {
			session = "a2"
		}
		_, err = room.send(session, chatAlice, "hi")
		require.NoError(t, err)
	}
	clock.advance(9999 * time.Millisecond)
	_, err = room.send("a2", chatAlice, "too soon")
	assert.ErrorIs(t, err, errChatRateLimited)
	clock.advance(time.Millisecond)
	_, err = room.send("a", chatAlice, "now allowed")
	require.NoError(t, err)
	room.revoke("a", "a2")
	_, err = room.send("a", chatAlice, "offline")
	assert.ErrorIs(t, err, errChatUnavailable)
}

func TestChatRoomSlowReaderAndClose(t *testing.T) {
	room, _ := testChatRoom(t)
	slow, _, err := room.subscribe("slow", chatBob, "")
	require.NoError(t, err)
	for i := range 20 {
		session := string(rune(i + 1000))
		person := chatPerson{Username: session}
		_, _, err := room.subscribe(session, person, "")
		require.NoError(t, err)
	}
	select {
	case <-slow.done:
	default:
		t.Fatal("slow reader did not disconnect")
	}
	room.close()
	room.close()
	_, _, err = room.subscribe("a", chatAlice, "")
	assert.True(t, errors.Is(err, errChatClosed))
}
