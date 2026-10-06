package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

// WebPush delivers a committee alert to every subscribed browser: the
// outbox's sender for mail.ChannelWebPush.
type WebPush struct {
	client  *Client
	store   *Store
	current func(username string, credentialHash []byte) bool
	logger  *slog.Logger
}

// NewWebPush returns the Web Push sender. current tells whether a session's
// account is still valid (admins.Registry.Current).
func NewWebPush(client *Client, store *Store, current func(username string, credentialHash []byte) bool, logger *slog.Logger) *WebPush {
	return &WebPush{client: client, store: store, current: current, logger: logger}
}

// Send pushes m to the subscriptions of the moment. A subscription the push
// service no longer knows is deleted; other failures are logged. The alert
// fails only when no browser got it.
func (w *WebPush) Send(ctx context.Context, m mail.Message) error {
	payload, err := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		URL   string `json:"url"`
	}{m.Subject, m.Text, "/demandes/" + strconv.FormatInt(m.TicketID, 10)})
	if err != nil {
		return fmt.Errorf("encode push payload: %w", err)
	}
	subs, err := w.store.List(ctx, w.current)
	if err != nil {
		return err
	}
	delivered := 0
	for _, sub := range subs {
		err := w.client.Send(ctx, sub, payload)
		switch {
		case err == nil:
			delivered++
			err = w.store.Touch(ctx, sub.ID)
		case errors.Is(err, ErrGone):
			w.logger.InfoContext(ctx, "push subscription gone, deleted", "subscription_id", sub.ID)
			err = w.store.Delete(ctx, sub.ID)
		default:
			w.logger.WarnContext(ctx, "push alert not delivered", "subscription_id", sub.ID, "error", err)
			err = nil
		}
		if err != nil {
			w.logger.ErrorContext(ctx, "push subscription update", "subscription_id", sub.ID, "error", err)
		}
	}
	if len(subs) > 0 && delivered == 0 {
		return fmt.Errorf("push alert reached none of %d subscriptions", len(subs))
	}
	return nil
}
