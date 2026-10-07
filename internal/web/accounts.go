package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
)

// accountsPath lists the committee accounts for the owner (spec §4.1 as
// amended).
const accountsPath = "/comptes"

// accountsData feeds templates/comptes.html.
type accountsData struct {
	Accounts []admins.Account
	Owner    string
	New      admins.Account    // the creation form as typed
	Errors   map[string]string // by field: identifiant, nom, fonction
}

// temporaryData feeds templates/mot_de_passe_temporaire.html.
type temporaryData struct {
	Heading, Username, Password, SignInURL string
}

// isOwner reports whether username is OWNER_USERNAME.
func (s *Server) isOwner(username string) bool {
	return s.cfg.Owner != "" && username == s.cfg.Owner
}

// ownerOnly lets only OWNER_USERNAME through; anyone else gets the 404 of an
// unknown page.
func (s *Server) ownerOnly(next http.HandlerFunc) http.HandlerFunc {
	return s.signedIn(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := sessionFrom(r.Context())
		if !s.isOwner(sess.account.Username) {
			s.notFound(w, r)
			return
		}
		next(w, r)
	})
}

func (s *Server) accountsPage(w http.ResponseWriter, r *http.Request) {
	var n *notice
	if r.URL.Query().Get("supprime") == "1" {
		n = &notice{Kind: noticeSuccess, Text: "Compte supprimé."}
	}
	s.renderAccounts(w, r, http.StatusOK, accountsData{}, n)
}

func (s *Server) renderAccounts(w http.ResponseWriter, r *http.Request, status int, d accountsData, n *notice) {
	p, err := s.adminPage(r, "Comptes")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if n != nil {
		p.Notices = append(p.Notices, *n)
	}
	d.Accounts, d.Owner = s.admins.Accounts(), s.cfg.Owner
	p.Data = d
	s.render(w, r, status, "comptes", p)
}

// createAccount adds an account and shows its temporary password once.
func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	f := r.PostForm
	in := admins.Account{
		Username: admins.NormalizeUsername(f.Get("identifiant")),
		Name:     strings.TrimSpace(f.Get("nom")),
		Role:     strings.TrimSpace(f.Get("fonction")),
	}
	errs := map[string]string{}
	if !admins.ValidUsername(in.Username) {
		errs["identifiant"] = "Lettres minuscules sans accent, chiffres, point, tiret ou tiret bas, 32 au plus."
	}
	if in.Name == "" {
		errs["nom"] = "Indique le nom affiché."
	}
	if in.Role == "" {
		errs["fonction"] = "Indique la fonction."
	}
	if len(errs) == 0 {
		password, err := s.admins.Create(r.Context(), in.Username, in.Name, in.Role)
		switch {
		case errors.Is(err, admins.ErrTaken):
			errs["identifiant"] = "Cet identifiant est déjà pris."
		case err != nil:
			s.serverError(w, r, err)
			return
		default:
			s.renderTemporary(w, r, "Compte créé pour "+in.Name, in.Username, password)
			return
		}
	}
	s.renderAccounts(w, r, http.StatusUnprocessableEntity, accountsData{New: in, Errors: errs}, nil)
}

// managedAccount is the account of the path, which the owner manages: never
// the owner's own, which only reset-password resets.
func (s *Server) managedAccount(w http.ResponseWriter, r *http.Request) (admins.Account, bool) {
	username := r.PathValue("identifiant")
	a, ok := s.admins.Get(username)
	if !ok || s.isOwner(username) {
		s.notFound(w, r)
		return admins.Account{}, false
	}
	return a, true
}

func (s *Server) managedAccountPage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.managedAccount(w, r)
	if !ok {
		return
	}
	p, err := s.adminPage(r, a.Name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = a
	s.render(w, r, http.StatusOK, "compte_gere", p)
}

// resetAccount gives the account a new temporary password; its sessions end.
func (s *Server) resetAccount(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	a, ok := s.managedAccount(w, r)
	if !ok {
		return
	}
	password, err := s.admins.ResetPassword(r.Context(), a.Username)
	switch {
	case errors.Is(err, admins.ErrNotFound):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.renderTemporary(w, r, "Nouveau mot de passe temporaire pour "+a.Name, a.Username, password)
	}
}

// deleteAccount removes the account: its sessions end and its open requests
// return to « à traiter ».
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	a, ok := s.managedAccount(w, r)
	if !ok {
		return
	}
	if r.PostForm.Get("confirmer") != "oui" {
		s.writeText(w, r, http.StatusBadRequest, "Coche la case pour confirmer la suppression.\n")
		return
	}
	switch err := s.admins.Delete(r.Context(), a.Username); {
	case errors.Is(err, admins.ErrNotFound):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, accountsPath+"?supprime=1", http.StatusSeeOther)
	}
}

// renderTemporary shows a temporary password: the answer to the POST that
// made it, never stored and never cached (securityHeaders sets no-store).
func (s *Server) renderTemporary(w http.ResponseWriter, r *http.Request, heading, username, password string) {
	p, err := s.adminPage(r, "Mot de passe temporaire")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = temporaryData{Heading: heading, Username: username, Password: password, SignInURL: s.cfg.AdminBaseURL.String() + "/connexion"}
	s.render(w, r, http.StatusOK, "mot_de_passe_temporaire", p)
}
