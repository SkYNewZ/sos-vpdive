package web

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
)

const (
	loginBodyLimit  = 16 << 10
	turnstileAction = "connexion"
)

type loginData struct {
	Username string
	SiteKey  string
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionOf(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, http.StatusOK, "", notice{})
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, username string, n notice) {
	p := s.newPage(r, "Connexion")
	if n.Kind != "" {
		p.Notices = append(p.Notices, n)
	}
	data := loginData{Username: username}
	if s.turnstile != nil {
		data.SiteKey = s.turnstile.SiteKey
	}
	p.Data = data
	s.render(w, r, status, "login", p)
}

// login checks, in order: the anti-bot token, the rate limits, then the
// password. Unknown usernames are verified against a dummy hash so that the
// response time does not reveal which accounts exist.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyLimit)
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, http.StatusBadRequest, "", notice{Kind: noticeError, Text: "Formulaire illisible. Réessaie."})
		return
	}
	ctx := r.Context()
	// Usernames are [a-z0-9._-]: a phone keyboard capitalizing the first
	// letter must not lock anyone out.
	username := strings.ToLower(strings.TrimSpace(r.PostForm.Get("username")))
	ip := s.clientIP(r)

	if s.turnstile != nil {
		err := s.turnstile.Verify(ctx, r.PostForm.Get("cf-turnstile-response"), ip, s.cfg.AdminBaseURL.Hostname(), turnstileAction)
		switch {
		case errors.Is(err, ErrBotCheckUnavailable):
			s.logger.WarnContext(ctx, "turnstile unavailable", "error", err)
			s.renderLogin(w, r, http.StatusServiceUnavailable, username, notice{Kind: noticeError,
				Text: "Le contrôle anti-robot ne répond pas. Réessaie dans un instant, ou écris au club : " + s.cfg.NotifyEmail.Address + "."})
			return
		case err != nil:
			s.renderLogin(w, r, http.StatusForbidden, username, notice{Kind: noticeError, Text: "Le contrôle anti-robot a échoué. Réessaie."})
			return
		}
	}

	// Serialise the check-then-act below: the limits and argon2id memory hold.
	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	wait, err := s.limiter.retryAfter(ctx, ip, username)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if wait > 0 {
		s.renderLogin(w, r, http.StatusTooManyRequests, username, notice{Kind: noticeError,
			Text: "Trop d'essais. Réessaie dans " + humanDuration(wait) + "."})
		return
	}

	account, known := s.admins.Get(username)
	hash := s.dummyHash
	if known {
		hash = account.PasswordHash
	}
	ok, err := admins.VerifyPassword(hash, r.PostForm.Get("password"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !known || !ok {
		if err := s.limiter.fail(ctx, ip, username); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.renderLogin(w, r, http.StatusUnauthorized, username, notice{Kind: noticeError, Text: "Identifiant ou mot de passe incorrect."})
		return
	}
	if err := s.limiter.succeed(ctx, username); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.startSession(ctx, w, account); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// forbidCSRF refuses a form whose anti-CSRF token is missing or invalid.
func (s *Server) forbidCSRF(w http.ResponseWriter, r *http.Request) {
	s.writeText(w, r, http.StatusForbidden, "Requête refusée : jeton de formulaire invalide.\n")
}

// postForm parses a small urlencoded form and checks its CSRF token; it has
// already answered 403 when it returns false.
func (s *Server) postForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyLimit)
	if err := r.ParseForm(); err != nil || !s.csrfValid(r, r.PostForm.Get("csrf")) {
		s.forbidCSRF(w, r)
		return false
	}
	return true
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	s.deleteSession(r.Context(), sess.hash)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/connexion", http.StatusSeeOther)
}

// home is the committee's start page. Lot 2 turns it into the requests board.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	p, err := s.adminPage(r, "Demandes")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin_home", p)
}

// adminPage is newPage plus the committee banners (spec §3.6, §4.1, §7.2).
func (s *Server) adminPage(r *http.Request, title string) (page, error) {
	p := s.newPage(r, title)
	notices, err := s.adminNotices(r.Context())
	if err != nil {
		return page{}, err
	}
	p.Notices = notices
	return p, nil
}

func (s *Server) adminNotices(ctx context.Context) ([]notice, error) {
	var out []notice
	if err := s.admins.Err(); err != nil {
		out = append(out, notice{Kind: noticeError,
			Text: "Le fichier des comptes est invalide : la dernière modification est ignorée et les comptes précédents restent actifs. Erreur : " + err.Error()})
	}
	has, err := s.members.HasList(ctx)
	if err != nil {
		return nil, err
	}
	if !has {
		return append(out, notice{Kind: noticeWarning, Text: "Le formulaire est fermé : aucune liste des membres n'est importée.",
			Link: "/imports", LinkText: "Importer la liste"}), nil
	}
	last, ok, err := s.members.LastImport(ctx)
	if err != nil {
		return nil, err
	}
	if ok && s.now().Sub(last.ImportedAt) > s.cfg.MembersMaxAge {
		out = append(out, notice{Kind: noticeWarning,
			Text: fmt.Sprintf("La liste des membres date du %s. Pense à refaire l'import.", s.formatDate(last.ImportedAt)),
			Link: "/imports", LinkText: "Refaire l'import"})
	}
	return out, nil
}

// serverError logs an internal error and answers 500 without detail.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.ErrorContext(r.Context(), "internal error", "error", err)
	s.writeText(w, r, http.StatusInternalServerError, "Erreur interne. Réessaie dans un instant.\n")
}

func humanDuration(d time.Duration) string {
	m := int(math.Ceil(d.Minutes()))
	if m <= 1 {
		return "1 minute"
	}
	return strconv.Itoa(m) + " minutes"
}
