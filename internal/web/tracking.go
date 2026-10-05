package web

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// Tracking page (spec §3.4, §11.1), reached through the secret link only.
// Logs and spans name the route pattern, never the path that holds the token.
const (
	replyIPLimit       = 30
	replyTicketLimit   = 20
	unavailableCapture = "static/capture-indisponible.svg"
)

type trackingData struct {
	Ticket   *tickets.Detail
	Messages []tickets.Message // internal notes left out
	Assignee *admins.Account
	CanReply bool
	Token    string
	Reply    string // typed text, kept when the reply is refused
	Error    string // shown beside the reply field
}

// ticketByToken finds the request of the link. Anything else gets the
// « ce lien ne fonctionne plus » page.
func (s *Server) ticketByToken(w http.ResponseWriter, r *http.Request) (*tickets.Detail, bool) {
	return findByToken(s, w, r, s.tickets.ByToken)
}

// idByToken is ticketByToken for handlers that only need the request id.
func (s *Server) idByToken(w http.ResponseWriter, r *http.Request) (int64, bool) {
	return findByToken(s, w, r, s.tickets.IDByToken)
}

func findByToken[T any](s *Server, w http.ResponseWriter, r *http.Request, find func(context.Context, string) (T, error)) (T, bool) {
	var zero T
	token := r.PathValue("jeton")
	if !secure.IsToken(token) {
		s.renderGone(w, r)
		return zero, false
	}
	found, err := find(r.Context(), token)
	if errors.Is(err, tickets.ErrNotFound) {
		s.renderGone(w, r)
		return zero, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return zero, false
	}
	return found, true
}

func (s *Server) renderGone(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "tracking_gone", s.newPage(r, "Lien inconnu"))
}

func (s *Server) trackingPage(w http.ResponseWriter, r *http.Request) {
	d, ok := s.ticketByToken(w, r)
	if !ok {
		return
	}
	s.renderTracking(w, r, http.StatusOK, d, "", "")
}

func (s *Server) renderTracking(w http.ResponseWriter, r *http.Request, status int, d *tickets.Detail, reply, problem string) {
	p := s.newPage(r, "Demande "+d.Ref)
	p.Data = trackingData{
		Ticket: d, Messages: d.Public(), Assignee: s.accountOf(d.Assignee), CanReply: s.tickets.CanReply(d),
		Token: r.PathValue("jeton"), Reply: reply, Error: problem,
	}
	s.render(w, r, status, "tracking", p)
}

// memberReply adds the member's message with its screenshots (spec §8.1,
// rows « Réponse de l'adhérent »).
func (s *Server) memberReply(w http.ResponseWriter, r *http.Request) {
	d, ok := s.ticketByToken(w, r)
	if !ok {
		return
	}
	f, err := readMultipart(w, r, captureField)
	if err != nil {
		s.readError(w, r, err)
		return
	}
	ctx := r.Context()
	reply := f.values.Get("message")
	allowed, err := s.replyAllowed(ctx, s.clientIP(r).String(), d.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !allowed {
		s.renderTracking(w, r, http.StatusTooManyRequests, d, reply, "Trop de messages en peu de temps. Réessaie dans une heure.")
		return
	}
	body, ok := tickets.CleanText(reply, 1, tickets.MessageMax)
	if !ok {
		s.renderTracking(w, r, http.StatusUnprocessableEntity, d, reply, "Écris ton message : 4 000 caractères au plus.")
		return
	}
	uploads, msg := sanitizeCaptures(f.files)
	if msg != "" {
		s.renderTracking(w, r, http.StatusUnprocessableEntity, d, reply, msg+" Ajoute de nouveau tes captures.")
		return
	}
	err = s.tickets.MemberReply(ctx, d.ID, body, uploads)
	switch {
	case err == nil:
		http.Redirect(w, r, "/suivi/"+r.PathValue("jeton"), http.StatusSeeOther)
	case errors.Is(err, tickets.ErrNotAllowed):
		s.renderTracking(w, r, http.StatusConflict, d, reply, "")
	case errors.Is(err, tickets.ErrInvalid):
		s.renderTracking(w, r, http.StatusUnprocessableEntity, d, reply, "Écris ton message : 4 000 caractères au plus.")
	case errors.Is(err, tickets.ErrTooManyCaptures):
		s.renderTracking(w, r, http.StatusUnprocessableEntity, d, reply,
			"Une demande garde 10 captures au plus : envoie-en moins, ou ton message sans capture.")
	case errors.Is(err, tickets.ErrNotFound): // deleted since the link was opened
		s.renderGone(w, r)
	case errors.Is(err, tickets.ErrStorage):
		s.renderTracking(w, r, http.StatusServiceUnavailable, d, reply,
			"Tes captures n'ont pas pu être enregistrées, et ton message n'est pas parti. Réessaie, avec ou sans captures.")
	default:
		s.serverError(w, r, err)
	}
}

// replyAllowed applies the reply limits of spec §11.3: 30 per hour per
// address, 20 per hour per request.
func (s *Server) replyAllowed(ctx context.Context, ip string, id int64) (bool, error) {
	return s.limiter.allowAll(ctx,
		rule{"reply-ip:" + ip, replyIPLimit, time.Hour},
		rule{"reply-ticket:" + strconv.FormatInt(id, 10), replyTicketLimit, time.Hour})
}

// memberClose marks the request settled by its member. A second tap on a
// request already done changes nothing.
func (s *Server) memberClose(w http.ResponseWriter, r *http.Request) {
	id, ok := s.idByToken(w, r)
	if !ok {
		return
	}
	err := s.tickets.MemberClose(r.Context(), id)
	if errors.Is(err, tickets.ErrNotFound) { // deleted since the link was opened
		s.renderGone(w, r)
		return
	}
	if err != nil && !errors.Is(err, tickets.ErrNotAllowed) {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/suivi/"+r.PathValue("jeton"), http.StatusSeeOther)
}

func (s *Server) memberCapture(w http.ResponseWriter, r *http.Request) {
	id, ok := s.idByToken(w, r)
	if !ok {
		return
	}
	attachment, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	data, mime, err := s.tickets.Capture(r.Context(), id, attachment)
	s.writeCapture(w, r, data, mime, err)
}

// writeCapture serves a decrypted screenshot. When storage does not answer,
// a fixed « Capture indisponible » image stands in, so the page around it
// still shows (spec §9.8). Responses are no-store like every dynamic one.
func (s *Server) writeCapture(w http.ResponseWriter, r *http.Request, data []byte, mime string, err error) {
	switch {
	case errors.Is(err, tickets.ErrNotFound):
		s.notFound(w, r)
		return
	case errors.Is(err, tickets.ErrStorage): // tickets.Store.Capture already logged it
		if data, err = fs.ReadFile(embedded, unavailableCapture); err != nil {
			s.serverError(w, r, err)
			return
		}
		mime = "image/svg+xml"
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", "inline")
	if _, err := w.Write(data); err != nil {
		s.logger.DebugContext(r.Context(), "write capture", "error", err)
	}
}
