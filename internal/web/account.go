package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/images"
)

// accountPath is « Mon compte »: every resolver changes their password there,
// and a temporary password leads there first (spec §4.1 as amended).
const accountPath = "/compte"

// photoField is the photo input of « Mon compte ».
const photoField = "photo"

// accountData feeds templates/compte.html.
type accountData struct {
	MinLen int               // shortest password accepted
	Errors map[string]string // by field: actuel, nouveau, confirmation, photo
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	var n *notice
	q := r.URL.Query()
	switch {
	case q.Get("change") == "1":
		n = &notice{Kind: noticeSuccess, Text: "Mot de passe changé. Tes autres appareils sont déconnectés."}
	case q.Get("photo") == "1":
		n = &notice{Kind: noticeSuccess, Text: "Photo changée."}
	case q.Get("photo") == "0":
		n = &notice{Kind: noticeSuccess, Text: "Photo retirée."}
	}
	s.renderAccount(w, r, http.StatusOK, accountData{}, n)
}

// setPhoto puts the photo the resolver sent in place of their avatar, or
// goes back to the drawn avatar (action=retirer).
func (s *Server) setPhoto(w http.ResponseWriter, r *http.Request) {
	f, err := readMultipart(w, r, photoField)
	if errors.Is(err, errFormTooLarge) { // nothing changes: no token needed to say so
		s.renderPhotoError(w, r, "Ta photo dépasse 5 Mo.")
		return
	}
	if err != nil {
		s.readError(w, r, err)
		return
	}
	if !s.csrfValid(r, f.values.Get("csrf")) {
		s.forbidCSRF(w, r)
		return
	}
	ctx := r.Context()
	sess, _ := sessionFrom(ctx)
	if f.values.Get("action") == "retirer" {
		if err := s.admins.SetAvatar(ctx, sess.account.Username, nil); err != nil {
			s.serverError(w, r, err)
			return
		}
		http.Redirect(w, r, accountPath+"?photo=0", http.StatusSeeOther)
		return
	}
	if len(f.files) == 0 {
		s.renderPhotoError(w, r, "Choisis une photo.")
		return
	}
	photo, err := images.Avatar(f.files[0])
	switch {
	case errors.Is(err, images.ErrTooBig):
		s.renderPhotoError(w, r, "Ta photo dépasse 5 Mo.")
	case errors.Is(err, images.ErrTooManyPixels):
		s.renderPhotoError(w, r, "Ta photo est trop grande : 40 millions de pixels au plus.")
	case errors.Is(err, images.ErrNotImage):
		s.renderPhotoError(w, r, "Ce fichier n'est pas une image PNG, JPEG ou WebP.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		if err := s.admins.SetAvatar(ctx, sess.account.Username, photo); err != nil {
			s.serverError(w, r, err)
			return
		}
		http.Redirect(w, r, accountPath+"?photo=1", http.StatusSeeOther)
	}
}

func (s *Server) renderPhotoError(w http.ResponseWriter, r *http.Request, msg string) {
	s.renderAccount(w, r, http.StatusUnprocessableEntity, accountData{Errors: map[string]string{photoField: msg}}, nil)
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, status int, d accountData, n *notice) {
	sess, _ := sessionFrom(r.Context())
	d.MinLen = admins.MinPasswordLength
	var p page
	if sess.account.MustChangePassword { // a temporary password is in place: the page stands alone
		p = s.newPage(r, "Choisis ton mot de passe")
		p.Bare = true // no navigation while every page leads here
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
// device gets a new session; the account's other sessions end.
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
		limited, ok, err := s.checkPassword(r, a.Username, a.PasswordHash, f.Get("actuel"), true)
		switch {
		case err != nil:
			s.serverError(w, r, err)
			return
		case limited != "":
			d.Errors["actuel"] = limited
			s.renderAccount(w, r, http.StatusTooManyRequests, d, nil)
			return
		case !ok:
			d.Errors["actuel"] = "Ce n'est pas ton mot de passe actuel."
		}
	}
	var hash string
	if len(d.Errors) == 0 {
		var err error
		hash, err = s.admins.ChangePassword(ctx, a, next, sess.hash)
		switch {
		case errors.Is(err, admins.ErrTooShort):
			d.Errors["nouveau"] = fmt.Sprintf("Il faut au moins %d caractères.", admins.MinPasswordLength)
		case errors.Is(err, admins.ErrSamePassword):
			d.Errors["nouveau"] = "C'est déjà ton mot de passe."
		case errors.Is(err, admins.ErrPasswordChanged):
			field := "actuel"
			if a.MustChangePassword {
				field = "nouveau" // the forced page has no « actuel » field
			}
			d.Errors[field] = "Ton mot de passe vient d'être changé. Reconnecte-toi."
			s.renderAccount(w, r, http.StatusConflict, d, nil)
			return
		case err != nil:
			s.serverError(w, r, err)
			return
		}
	}
	if len(d.Errors) > 0 {
		s.renderAccount(w, r, http.StatusUnprocessableEntity, d, nil)
		return
	}
	// ChangePassword kept this session valid until the new one replaces it.
	a.PasswordHash = hash
	if err := s.startSession(ctx, w, a, sess.hash); err != nil {
		s.serverError(w, r, err)
		return
	}
	if a.MustChangePassword {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, accountPath+"?change=1", http.StatusSeeOther)
}
