package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

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

// Send pushes m to the subscriptions of the moment. A subscription that
// cannot be used again (unreadable, unknown to the push service, or on a host
// that left PUSH_ALLOWED_HOSTS) is deleted; other failures are logged. The
// alert fails only when no browser got it.
func (w *WebPush) Send(ctx context.Context, m mail.Message) error {
	payload, err := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		URL   string `json:"url"`
	}{m.Subject, m.Text, "/demandes/" + strconv.FormatInt(m.TicketID, 10)})
	if err != nil {
		return fmt.Errorf("encode push payload: %w", err)
	}
	subs, unreadable, err := w.store.List(ctx)
	if err != nil {
		return err
	}
	for _, id := range unreadable {
		w.drop(ctx, id, errUnreadable)
	}
	errs := sendAll(len(subs), func(i int) error { return w.client.Send(ctx, subs[i], payload) })
	delivered := 0
	for i, sub := range subs {
		switch err := errs[i]; {
		case err == nil:
			delivered++
			if err := w.store.Touch(ctx, sub.ID); err != nil {
				w.logger.ErrorContext(ctx, "push subscription update", "subscription_id", sub.ID, "error", err)
			}
		case errors.Is(err, errGone), errors.Is(err, errEndpointRefused):
			w.drop(ctx, sub.ID, err)
		default:
			w.logger.WarnContext(ctx, "push alert not delivered", "subscription_id", sub.ID, "error", err)
		}
	}
	if len(subs) > 0 && delivered == 0 {
		return fmt.Errorf("push alert reached none of %d subscriptions", len(subs))
	}
	return nil
}

// drop deletes a subscription that cannot be used again.
func (w *WebPush) drop(ctx context.Context, id int64, cause error) {
	w.logger.InfoContext(ctx, "push subscription unusable, deleted", "subscription_id", id, "error", cause)
	if err := w.store.Delete(ctx, id); err != nil {
		w.logger.ErrorContext(ctx, "push subscription update", "subscription_id", id, "error", err)
	}
}

// sendAll makes the n calls of one alert at once, so a recipient that does
// not answer delays no other, and returns the error of each.
func sendAll(n int, send func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = send(i) })
	}
	wg.Wait()
	return errs
}
