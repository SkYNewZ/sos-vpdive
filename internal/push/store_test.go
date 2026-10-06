package push

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

type testStore struct {
	*Store

	db    *sql.DB
	clock *testClock
}

func newTestStore(t *testing.T) *testStore {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	keys, err := secure.NewKeys(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	clock := &testClock{t: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
	return &testStore{Store: NewStore(db, keys, clock.now), db: db, clock: clock}
}

// session inserts a committee session and returns its token hash.
func (s *testStore) session(t *testing.T, token, username string) []byte {
	t.Helper()
	hash := secure.TokenHash(token)
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO sessions (token_hash, username, credential_hash, created_at, expires_at) VALUES (?, ?, X'00', 1, 9999999999)`,
		hash, username)
	require.NoError(t, err)
	return hash
}

func (s *testStore) count(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM push_subscriptions`).Scan(&n))
	return n
}

func sampleSubscription(endpoint string) Subscription {
	return Subscription{Endpoint: endpoint, P256DH: bytes.Repeat([]byte{4}, 65), Auth: bytes.Repeat([]byte{5}, 16)}
}

func TestStoreSavesSealedAndLists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	phone := s.session(t, "phone", "alice")
	const endpoint = "https://fcm.googleapis.com/fcm/send/witness-endpoint-token"
	require.NoError(t, s.Save(ctx, phone, "alice", sampleSubscription(endpoint)))

	var sealed []byte
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT endpoint || keys FROM push_subscriptions`).Scan(&sealed))
	assert.NotContains(t, string(sealed), "witness-endpoint-token")
	subs, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, subs, 1)
	assert.Equal(t, endpoint, subs[0].Endpoint)
	assert.Equal(t, bytes.Repeat([]byte{4}, 65), subs[0].P256DH)
	assert.Equal(t, bytes.Repeat([]byte{5}, 16), subs[0].Auth)
	has, err := s.HasSession(ctx, phone)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestStoreKeepsOneRowPerBrowserAndPerSession(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	before, after := s.session(t, "before", "alice"), s.session(t, "after", "alice")
	require.NoError(t, s.Save(ctx, before, "alice", sampleSubscription("https://fcm.googleapis.com/a")))
	require.NoError(t, s.Save(ctx, before, "alice", sampleSubscription("https://fcm.googleapis.com/a")))
	assert.Equal(t, 1, s.count(t), "the same browser twice is one row")

	require.NoError(t, s.Save(ctx, after, "alice", sampleSubscription("https://fcm.googleapis.com/a")))
	assert.Equal(t, 1, s.count(t), "logging in again moves the browser to the new session")
	has, err := s.HasSession(ctx, before)
	require.NoError(t, err)
	assert.False(t, has)

	require.NoError(t, s.Save(ctx, after, "alice", sampleSubscription("https://fcm.googleapis.com/b")))
	subs, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, subs, 1, "a session is one device: its new subscription replaces the old one")
	assert.Equal(t, "https://fcm.googleapis.com/b", subs[0].Endpoint)
}

func TestSubscriptionsLeaveWithTheirSession(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	phone, laptop := s.session(t, "phone", "alice"), s.session(t, "laptop", "alice")
	require.NoError(t, s.Save(ctx, phone, "alice", sampleSubscription("https://fcm.googleapis.com/phone")))
	require.NoError(t, s.Save(ctx, laptop, "alice", sampleSubscription("https://fcm.googleapis.com/laptop")))

	require.NoError(t, s.DeleteSession(ctx, laptop))
	assert.Equal(t, 1, s.count(t), "« Désactiver » removes this device only")
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, phone)
	require.NoError(t, err)
	assert.Zero(t, s.count(t), "logout and revocation delete the session, the cascade does the rest")
}

func TestStoreTouchDeleteAndPurge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	used, idle := s.session(t, "used", "alice"), s.session(t, "idle", "bob")
	require.NoError(t, s.Save(ctx, used, "alice", sampleSubscription("https://fcm.googleapis.com/used")))
	require.NoError(t, s.Save(ctx, idle, "bob", sampleSubscription("https://fcm.googleapis.com/idle")))
	subs, err := s.List(ctx)
	require.NoError(t, err)
	byEndpoint := map[string]int64{subs[0].Endpoint: subs[0].ID, subs[1].Endpoint: subs[1].ID}

	s.clock.advance(80 * 24 * time.Hour)
	require.NoError(t, s.Touch(ctx, byEndpoint["https://fcm.googleapis.com/used"]))
	s.clock.advance(11 * 24 * time.Hour)
	require.NoError(t, s.Purge(ctx))
	subs, err = s.List(ctx)
	require.NoError(t, err)
	require.Len(t, subs, 1, "90 days without a successful push (spec §8.3)")
	assert.Equal(t, "https://fcm.googleapis.com/used", subs[0].Endpoint)

	require.NoError(t, s.Delete(ctx, subs[0].ID))
	assert.Zero(t, s.count(t))
}
