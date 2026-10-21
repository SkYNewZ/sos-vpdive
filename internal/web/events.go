package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Live board (spec §4.2): Server-Sent Events that carry a change type, a
// request id and whether the stream's resolver made the change, nothing else. Like /healthz, the stream is neither traced nor
// logged (spec §9.9).
const (
	keepAliveInterval = 25 * time.Second // default of Server.keepAlive
	reconnectDelay    = 5 * time.Second
)

// events streams request changes to a signed-in committee member until the
// request ends, the broker drops the stream (logout, revocation, slow
// reader), the session stops being valid or the server stops.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !s.streamOriginOK(r) {
		s.writeText(w, r, http.StatusForbidden, "Requête refusée : origine non valide.\n")
		return
	}
	sess, ok := s.requireChatSession(w, r)
	if !ok {
		return
	}
	sub, ok := s.broker.subscribe(sess.hash, sess.account.Username)
	if !ok {
		s.writeText(w, r, http.StatusServiceUnavailable, "Service en cours d'arrêt.\n")
		return
	}
	defer s.broker.unsubscribe(sub)

	chatSub, snapshot, err := s.subscribeChat(r, sess)
	if err != nil {
		s.writeText(w, r, http.StatusServiceUnavailable, "Chat indisponible.\n")
		return
	}
	var chatCh <-chan chatUpdate
	var chatDone <-chan struct{}
	if chatSub != nil {
		defer s.chat.unsubscribe(chatSub)
		chatCh, chatDone = chatSub.ch, chatSub.done
	}
	rc := http.NewResponseController(w)
	// The server's read and write timeouts would cut a stream that lasts hours.
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		s.logger.DebugContext(r.Context(), "event stream read deadline", "error", err)
	}
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		s.logger.DebugContext(r.Context(), "event stream write deadline", "error", err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no") // reverse proxies must not buffer the stream
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", reconnectDelay.Milliseconds()); err != nil || rc.Flush() != nil {
		return
	}
	if chatSub != nil {
		if _, ok := s.sessionOf(r); !ok {
			return
		}
		if err := writeChatEvent(w, "chat-ready", snapshot.Cursor, struct {
			chatSnapshot

			Session string `json:"session"`
		}{snapshot, s.keys.CSRFToken(sess.hash)}); err != nil || rc.Flush() != nil {
			return
		}
	}
	endChat := func() {
		if chatSub != nil {
			if _, ok, err := s.lookupSession(r); err == nil && !ok {
				s.chat.revoke(string(sess.hash))
				_ = writeChatEvent(w, "chat-session-ended", "", struct{}{})
				_ = rc.Flush()
			}
		}
	}
	ticker := time.NewTicker(s.keepAlive)
	defer ticker.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-sub.done:
			endChat()
			return
		case <-chatDone:
			endChat()
			return
		case update := <-chatCh:
			if _, ok := s.sessionOf(r); !ok {
				endChat()
				return
			}
			if update.Message != nil {
				err = writeChatEvent(w, update.Kind, update.Message.ID, update.Message)
			} else {
				err = writeChatEvent(w, update.Kind, "", update.People)
			}
		case c := <-sub.ch:
			// self spares a resolver the toast of their own change.
			self := c.Actor != "" && c.Actor == sub.username
			_, err = fmt.Fprintf(w, "event: %s\ndata: {\"id\":%d,\"self\":%t}\n\n", c.Type, c.TicketID, self)
		case <-ticker.C:
			// An expired or deleted session, or a changed password, ends the
			// stream within one keepalive.
			if _, ok := s.sessionOf(r); !ok {
				endChat()
				return
			}
			s.chat.sweep()
			_, err = io.WriteString(w, ": keepalive\n\n")
		}
		if err != nil || rc.Flush() != nil {
			return
		}
	}
}

// streamOriginOK applies the origin rule to the stream: a same-origin
// EventSource sends no Origin header, so a present Origin must be the
// committee's, and a present Sec-Fetch-Site must be same-origin.
func (s *Server) streamOriginOK(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != s.cfg.AdminBaseURL.Scheme+"://"+s.cfg.AdminBaseURL.Host {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

func writeChatEvent(w io.Writer, kind, cursor string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if cursor != "" {
		if _, err = fmt.Fprintf(w, "id: %s\n", cursor); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
	return err
}

func (s *Server) subscribeChat(r *http.Request, sess session) (*chatSubscription, chatSnapshot, error) {
	if r.URL.Query().Get("chat") != "1" {
		return nil, chatSnapshot{}, nil
	}
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after")
	}
	return s.chat.subscribe(string(sess.hash), chatIdentity(sess.account), cursor)
}
