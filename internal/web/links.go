package web

import (
	"net/http"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// Lost link (spec §3.5): the answer is the same whether the address has
// requests or not, and the links sent are the original ones.
const (
	turnstileLinks    = "retrouver"
	recoverIPLimit    = 5
	recoverEmailLimit = 3
	linksSentText     = "Si cette adresse a des demandes, tu vas recevoir un mail avec leurs liens de suivi. Regarde aussi dans les indésirables."
	tooManyLinksText  = "Trop de demandes de liens. Réessaie plus tard."
)

// The lost-link limits (spec §11.3) also bound the link resend of screen 2:
// both count on the same keys, so they share these rules.
func (s *Server) recoverIPRule(r *http.Request) rule {
	return rule{"recover-ip:" + s.clientIP(r).String(), recoverIPLimit, time.Hour}
}

func recoverEmailRule(email string) rule {
	return rule{"recover-email:" + email, recoverEmailLimit, 24 * time.Hour}
}

type linksData struct {
	SiteKey string
	Email   string
	Error   string
}

func (s *Server) linksPage(w http.ResponseWriter, r *http.Request) {
	var n *notice
	if r.URL.Query().Get("envoye") == "1" {
		n = &notice{Kind: noticeSuccess, Text: linksSentText}
	}
	s.renderLinks(w, r, http.StatusOK, linksData{}, n)
}

func (s *Server) renderLinks(w http.ResponseWriter, r *http.Request, status int, d linksData, n *notice) {
	p := s.newPage(r, "Retrouver mes demandes")
	if s.turnstile != nil {
		d.SiteKey = s.turnstile.SiteKey
	}
	if n != nil {
		p.Notices = append(p.Notices, *n)
	}
	p.Data = d
	s.render(w, r, status, "links", p)
}

// requestLinks queues the lost-link mail: anti-robot check, 5 per hour per
// address, 3 per day per email, then the same redirect in every case.
func (s *Server) requestLinks(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyLimit)
	if err := r.ParseForm(); err != nil {
		s.writeText(w, r, http.StatusBadRequest, "Formulaire illisible. Réessaie.\n")
		return
	}
	ctx := r.Context()
	d := linksData{Email: r.PostForm.Get("email")}
	if status, n := s.checkBot(r, r.PostForm.Get(turnstileField), s.cfg.BaseURL.Hostname(), turnstileLinks); n != nil {
		s.renderLinks(w, r, status, d, n)
		return
	}
	// refused answers for a rule that does not hold: true when the request is over.
	refused := func(rl rule) bool {
		ok, err := s.limiter.allowAll(ctx, rl)
		switch {
		case err != nil:
			s.serverError(w, r, err)
		case !ok:
			s.renderLinks(w, r, http.StatusTooManyRequests, d, &notice{Kind: noticeError, Text: tooManyLinksText})
		}
		return err != nil || !ok
	}
	if refused(s.recoverIPRule(r)) {
		return
	}
	email, err := secure.NormalizeEmail(d.Email)
	if err != nil || !validAddress(email) {
		d.Error = "Indique une adresse mail valide."
		s.renderLinks(w, r, http.StatusUnprocessableEntity, d, nil)
		return
	}
	if refused(recoverEmailRule(email)) {
		return
	}
	if err := s.tickets.SendLinks(ctx, email); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/retrouver?envoye=1", http.StatusSeeOther)
}
