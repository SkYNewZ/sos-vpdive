package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
)

// accountPath is « Mon compte »: every resolver changes their password there,
// and a temporary password leads there first (spec §4.1 as amended).
const accountPath = "/compte"

// accountData feeds templates/compte.html.
type accountData struct {
	Forced   bool              // a temporary password is in place: the page stands alone
	Username string            // for password managers
	Errors   map[string]string // by field: actuel, nouveau, confirmation
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	var n *notice
	if r.URL.Query().Get("change") == "1" {
		n = &notice{Kind: noticeSuccess, Text: "Mot de passe changé. Tes autres appareils sont déconnectés."}
	}
	s.renderAccount(w, r, http.StatusOK, accountData{}, n)
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, status int, d accountData, n *notice) {
	sess, _ := sessionFrom(r.Context())
	d.Forced, d.Username = sess.account.MustChangePassword, sess.account.Username
	var p page
	if d.Forced {
		p = s.newPage(r, "Choisis ton mot de passe")
		p.Account = nil // the plain shell: no navigation while every page leads here
	} else {
		var err error
		if p, err = s.adminPage(r, "Mon compte"); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if n != nil {
		p.Notices = append(p.Notices, *n)
	}
	p.Data = d
	s.render(w, r, status, "compte", p)
}

// changePassword sets the password the resolver chose. Outside the first
// sign-in it asks for the current one, under the login's rate limits. This
// session stays; the account's other sessions end.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	ctx := r.Context()
	sess, _ := sessionFrom(ctx)
	a, f := sess.account, r.PostForm
	d := accountData{Errors: map[string]string{}}
	next := f.Get("nouveau")
	if next != f.Get("confirmation") {
		d.Errors["confirmation"] = "Les deux mots de passe ne sont pas pareils."
	}

	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if !a.MustChangePassword {
		msg, status, err := s.checkCurrentPassword(r, a, f.Get("actuel"))
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if msg != "" {
			d.Errors["actuel"] = msg
		}
		if status == http.StatusTooManyRequests {
			s.renderAccount(w, r, status, d, nil)
			return
		}
	}
	if len(d.Errors) == 0 {
		switch err := s.admins.ChangePassword(ctx, a.Username, next, sess.hash); {
		case errors.Is(err, admins.ErrTooShort):
			d.Errors["nouveau"] = fmt.Sprintf("Il faut au moins %d caractères.", admins.MinPasswordLength)
		case errors.Is(err, admins.ErrSamePassword):
			d.Errors["nouveau"] = "C'est déjà ton mot de passe."
		case err != nil:
			s.serverError(w, r, err)
			return
		}
	}
	if len(d.Errors) > 0 {
		s.renderAccount(w, r, http.StatusUnprocessableEntity, d, nil)
		return
	}
	if a.MustChangePassword {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, accountPath+"?change=1", http.StatusSeeOther)
}

// checkCurrentPassword verifies the current password under the login's rate
// limits. It returns the field's error message, empty when the password is
// right, and 429 as the status when the limiter says to wait. The caller holds
// loginMu.
func (s *Server) checkCurrentPassword(r *http.Request, a admins.Account, current string) (msg string, status int, err error) {
	ctx, ip := r.Context(), s.clientIP(r)
	wait, err := s.limiter.retryAfter(ctx, ip, a.Username)
	if err != nil {
		return "", 0, err
	}
	if wait > 0 {
		return "Trop d'essais. Réessaie dans " + humanDuration(wait) + ".", http.StatusTooManyRequests, nil
	}
	ok, err := admins.VerifyPassword(a.PasswordHash, current)
	if err != nil || ok {
		return "", 0, err
	}
	if err := s.limiter.fail(ctx, ip, a.Username); err != nil {
		return "", 0, err
	}
	return "Ce n'est pas ton mot de passe actuel.", 0, nil
}
