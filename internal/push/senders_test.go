package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

func alert() mail.Message {
	return mail.Message{Channel: mail.ChannelWebPush, TicketID: 42, Subject: "Nouvelle demande", Text: "CPP-0042 · Autre"}
}

// fanOut is a push service double whose answer depends on the path.
func fanOut(t *testing.T, status map[string]int) *pushService {
	t.Helper()
	ps := &pushService{}
	ps.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ps.mu.Lock()
		ps.requests, ps.bodies = append(ps.requests, r), append(ps.bodies, body)
		ps.mu.Unlock()
		w.WriteHeader(status[r.URL.Path])
	}))
	t.Cleanup(ps.Close)
	return ps
}

func TestWebPushSendsToEveryBrowserAndDropsTheGoneOnes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ps := fanOut(t, map[string]int{"/ok": http.StatusCreated, "/gone": http.StatusGone, "/broken": http.StatusInternalServerError})
	var logs bytes.Buffer
	sender := NewWebPush(newTestClient(newTestVAPID(t), ps), s.Store, slog.New(slog.NewTextHandler(&logs, nil)))
	browsers := map[string]*ecdh.PrivateKey{}
	for _, path := range []string{"/ok", "/gone", "/broken"} {
		sub, ua := testSubscription(t, ps.URL+path)
		browsers[path] = ua
		require.NoError(t, s.Save(ctx, s.session(t, path, "alice"), "alice", sub))
	}
	s.clock.advance(time.Minute)

	require.NoError(t, sender.Send(ctx, alert()), "one browser got it: the alert is sent")
	require.Equal(t, 3, ps.count())
	subs, err := s.List(ctx)
	require.NoError(t, err)
	assert.Len(t, subs, 2, "410 deletes the subscription")

	var body []byte
	for i, r := range ps.requests {
		if r.URL.Path == "/ok" {
			body = ps.bodies[i]
		}
	}
	var got map[string]string
	require.NoError(t, json.Unmarshal(decrypt(t, body, browsers["/ok"], bytes.Repeat([]byte{1}, 16)), &got))
	assert.Equal(t, map[string]string{"title": "Nouvelle demande", "body": "CPP-0042 · Autre", "url": "/demandes/42"}, got)

	var touched int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM push_subscriptions WHERE last_used_at > created_at`).Scan(&touched))
	assert.Equal(t, 1, touched, "only the delivered one is touched")
	assert.NotContains(t, logs.String(), strings.TrimPrefix(ps.URL, "https://"), "never the endpoint in logs")
	assert.Contains(t, logs.String(), "subscription_id=")
}

func TestWebPushOutcomeWithoutDelivery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ps := fanOut(t, map[string]int{"/broken": http.StatusInternalServerError})
	sender := NewWebPush(newTestClient(newTestVAPID(t), ps), s.Store, slog.New(slog.DiscardHandler))
	require.NoError(t, sender.Send(ctx, alert()), "no subscription: nothing to do")

	sub, _ := testSubscription(t, ps.URL+"/broken")
	require.NoError(t, s.Save(ctx, s.session(t, "x", "alice"), "alice", sub))
	require.Error(t, sender.Send(ctx, alert()), "every push failed: the alert is marked failed")
	assert.Equal(t, 1, ps.count(), "no retry")
}

// pushoverAPI is a Pushover double that refuses one user key.
type pushoverAPI struct {
	*httptest.Server

	mu    sync.Mutex
	forms []url.Values
}

func newPushoverAPI(t *testing.T, refused string) *pushoverAPI {
	t.Helper()
	api := &pushoverAPI{}
	api.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		api.mu.Lock()
		api.forms = append(api.forms, r.PostForm)
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("user") == refused {
			http.Error(w, `{"user":"invalid","errors":["user identifier is not a valid user"],"status":0}`, http.StatusBadRequest)
			return
		}
		if _, err := io.WriteString(w, `{"status":1,"request":"abc"}`); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(api.Close)
	return api
}

const (
	aliceKey = "uQiRzpo4DXghDmr9QzzfQu27cmVRsA"
	bobKey   = "uQiRzpo4DXghDmr9QzzfQu27cmVRsB"
	appToken = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
)

func newTestPushover(api *pushoverAPI, accounts []admins.Account, logs *bytes.Buffer) *Pushover {
	p := NewPushover(appToken, &url.URL{Scheme: "https", Host: "comite.example.org"},
		func() []admins.Account { return accounts }, slog.New(slog.NewTextHandler(logs, nil)))
	p.api = api.URL + "/1/messages.json"
	p.http.Transport = api.Client().Transport
	return p
}

func TestPushoverSendsToEachAccountWithAKey(t *testing.T) {
	api := newPushoverAPI(t, bobKey)
	var logs bytes.Buffer
	accounts := []admins.Account{
		{Username: "alice", PushoverUserKey: aliceKey},
		{Username: "bob", PushoverUserKey: bobKey},
		{Username: "carol"},
	}
	m := alert()
	m.Channel = mail.ChannelPushover
	require.NoError(t, newTestPushover(api, accounts, &logs).Send(context.Background(), m), "alice got it")
	require.Len(t, api.forms, 2, "carol has no key, bob's refusal does not stop alice")
	got := map[string]url.Values{}
	for _, f := range api.forms {
		got[f.Get("user")] = f
	}
	assert.Equal(t, url.Values{
		"token": {appToken}, "user": {aliceKey}, "title": {"Nouvelle demande"}, "message": {"CPP-0042 · Autre"},
		"url": {"https://comite.example.org/demandes/42"}, "url_title": {"Ouvrir la demande"},
	}, got[aliceKey])
	assert.Contains(t, logs.String(), "username=bob")
	assert.NotContains(t, logs.String(), bobKey, "never a user key in logs")
	assert.NotContains(t, logs.String(), appToken)
}

func TestPushoverOutcomes(t *testing.T) {
	api := newPushoverAPI(t, aliceKey)
	var logs bytes.Buffer
	m := alert()
	require.NoError(t, newTestPushover(api, []admins.Account{{Username: "carol"}}, &logs).Send(context.Background(), m),
		"nobody has a key: nothing to do")
	require.Error(t, newTestPushover(api, []admins.Account{{Username: "alice", PushoverUserKey: aliceKey}}, &logs).Send(context.Background(), m),
		"every call failed: the alert is marked failed")
	assert.NotContains(t, logs.String(), aliceKey)
}
