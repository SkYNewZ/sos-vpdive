package web

import (
	"errors"
	"net/http"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

// failedMails lists the mails the relay refused for good or could not take
// for seven days (spec §6); a resolver puts them back in the queue.
func (s *Server) failedMails(w http.ResponseWriter, r *http.Request) {
	list, err := s.outbox.Failed(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p, err := s.adminPage(r, "Envois en échec")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if r.URL.Query().Get("relance") == "1" {
		p.Notices = append(p.Notices, notice{Kind: noticeSuccess, Text: "Le mail est remis dans la file d'envoi."})
	}
	p.Data = list
	s.render(w, r, http.StatusOK, "envois", p)
}

func (s *Server) retryMail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	if !s.postForm(w, r) {
		return
	}
	err := s.outbox.Retry(r.Context(), id)
	switch {
	case errors.Is(err, mail.ErrNotFound):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, "/envois?relance=1", http.StatusSeeOther)
	}
}
