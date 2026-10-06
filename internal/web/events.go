package web

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// Live board (spec §4.2): Server-Sent Events that carry a change type and a
// request id, nothing else. Like /healthz, the stream is neither traced nor
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
	sess, ok := s.sessionOf(r)
	if !ok {
		s.writeText(w, r, http.StatusForbidden, "Session expirée : reconnecte-toi.\n")
		return
	}
	sub, ok := s.broker.subscribe(sess.hash, sess.account.Username)
	if !ok {
		s.writeText(w, r, http.StatusServiceUnavailable, "Service en cours d'arrêt.\n")
		return
	}
	defer s.broker.unsubscribe(sub)

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
	ticker := time.NewTicker(s.keepAlive)
	defer ticker.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-sub.done:
			return
		case c := <-sub.ch:
			_, err = fmt.Fprintf(w, "event: %s\ndata: {\"id\":%d}\n\n", c.Type, c.TicketID)
		case <-ticker.C:
			// An expired or deleted session, or a changed password, ends the
			// stream within one keepalive.
			if _, ok := s.sessionOf(r); !ok {
				return
			}
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
