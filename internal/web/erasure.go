package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// Erasure of a person's data (spec §4.5): from an address, every request of
// it and its members list row. The address travels in forms only, never in a
// URL, so it reaches no log.
const (
	stepPreview = "apercu"
	stepConfirm = "confirmer"
	stepDone    = "fait"
)

type erasureData struct {
	Email  string
	Step   string // "", stepPreview or stepDone
	Counts tickets.Erasure
	Error  string
}

// erasurePage asks for the address; ?demande=<id> fills in the address of
// that request.
func (s *Server) erasurePage(w http.ResponseWriter, r *http.Request) {
	var d erasureData
	if id, err := strconv.ParseInt(r.URL.Query().Get("demande"), 10, 64); err == nil {
		t, err := s.tickets.Detail(r.Context(), id)
		switch {
		case err == nil:
			d.Email = t.Email
		case !errors.Is(err, tickets.ErrNotFound):
			s.serverError(w, r, err)
			return
		}
	}
	s.renderErasure(w, r, http.StatusOK, d)
}

// erase shows what will go, then erases it once confirmed.
func (s *Server) erase(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	ctx := r.Context()
	d := erasureData{Email: r.PostForm.Get("email")}
	email, err := secure.NormalizeEmail(d.Email)
	if err != nil || !validAddress(email) {
		d.Error = "Indique une adresse mail valide."
		s.renderErasure(w, r, http.StatusUnprocessableEntity, d)
		return
	}
	d.Email, d.Step = email, stepPreview
	if r.PostForm.Get("etape") == stepConfirm {
		sess, _ := sessionFrom(ctx)
		d.Step = stepDone
		d.Counts, err = s.tickets.Erase(ctx, email, sess.account.Username)
	} else {
		d.Counts, err = s.tickets.PreviewErasure(ctx, email)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderErasure(w, r, http.StatusOK, d)
}

func (s *Server) renderErasure(w http.ResponseWriter, r *http.Request, status int, d erasureData) {
	p, err := s.adminPage(r, "Effacer les données d'une personne")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = d
	s.render(w, r, status, "erasure", p)
}
