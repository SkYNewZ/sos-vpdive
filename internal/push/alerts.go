package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

// WebPush delivers a committee alert to every subscribed browser: the
// outbox's sender for mail.ChannelWebPush.
type WebPush struct {
	client *Client
	store  *Store
	logger *slog.Logger
}

// NewWebPush returns the Web Push sender.
func NewWebPush(client *Client, store *Store, logger *slog.Logger) *WebPush {
	return &WebPush{client: client, store: store, logger: logger}
}

// Send pushes m to the subscriptions of the moment. A subscription the push
// service no longer knows, or whose host left PUSH_ALLOWED_HOSTS, is deleted;
// other failures are logged. The alert fails only when no browser got it.
func (w *WebPush) Send(ctx context.Context, m mail.Message) error {
	payload, err := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		URL   string `json:"url"`
	}{m.Subject, m.Text, "/demandes/" + strconv.FormatInt(m.TicketID, 10)})
	if err != nil {
		return fmt.Errorf("encode push payload: %w", err)
	}
	subs, err := w.store.List(ctx)
	if err != nil {
		return err
	}
	var (
		wg        sync.WaitGroup
		delivered atomic.Int64
	)
	for _, sub := range subs { // all at once: a device that does not answer delays no other
		wg.Go(func() {
			if w.push(ctx, sub, payload) {
				delivered.Add(1)
			}
		})
	}
	wg.Wait()
	if len(subs) > 0 && delivered.Load() == 0 {
		return fmt.Errorf("push alert reached none of %d subscriptions", len(subs))
	}
	return nil
}

// push sends payload to sub and records the outcome; true when delivered.
func (w *WebPush) push(ctx context.Context, sub Subscription, payload []byte) bool {
	err := w.client.Send(ctx, sub, payload)
	delivered := err == nil
	switch {
	case delivered:
		err = w.store.Touch(ctx, sub.ID)
	case errors.Is(err, errGone), errors.Is(err, errEndpointRefused):
		w.logger.InfoContext(ctx, "push subscription unusable, deleted", "subscription_id", sub.ID, "error", err)
		err = w.store.Delete(ctx, sub.ID)
	default:
		w.logger.WarnContext(ctx, "push alert not delivered", "subscription_id", sub.ID, "error", err)
		err = nil
	}
	if err != nil {
		w.logger.ErrorContext(ctx, "push subscription update", "subscription_id", sub.ID, "error", err)
	}
	return delivered
}
