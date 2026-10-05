package web

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

const (
	loginBodyLimit = 16 << 10
	// postFormLimit fits a 4 000-rune reply once URL-encoded.
	postFormLimit   = 64 << 10
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

	if status, n := s.checkBot(r, r.PostForm.Get(turnstileField), s.cfg.AdminBaseURL.Hostname(), turnstileAction); n != nil {
		s.renderLogin(w, r, status, username, *n)
		return
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
	r.Body = http.MaxBytesReader(w, r.Body, postFormLimit)
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
	s.broker.disconnect(func(sub *subscriber) bool { return sub.session == string(sess.hash) })
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/connexion", http.StatusSeeOther)
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

// adminNotices are the committee banners: accounts file, members list
// (spec §3.6, §7.2), failed mails (§6) and open requests idle for a year
// (§8.3).
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
	last, imported, err := s.members.LastImport(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case !has:
		out = append(out, notice{Kind: noticeWarning, Text: "Le formulaire est fermé : aucune liste des membres n'est importée.",
			Link: "/imports", LinkText: "Importer la liste"})
	case imported && s.now().Sub(last.ImportedAt) > s.cfg.MembersMaxAge:
		out = append(out, notice{Kind: noticeWarning,
			Text: fmt.Sprintf("La liste des membres date du %s. Pense à refaire l'import.", s.formatDate(last.ImportedAt)),
			Link: "/imports", LinkText: "Refaire l'import"})
	}
	failed, err := s.outbox.FailedCount(ctx)
	if err != nil {
		return nil, err
	}
	if failed > 0 {
		out = append(out, notice{Kind: noticeError, Text: failedMailsText(failed), Link: "/envois", LinkText: "Voir les envois en échec"})
	}
	idle, err := s.tickets.Idle(ctx)
	if err != nil {
		return nil, err
	}
	if len(idle) > 0 {
		out = append(out, notice{Kind: noticeWarning, Text: idleText(idle), Link: "/?statut=" + filterAllOpen, LinkText: "Voir les demandes ouvertes"})
	}
	return out, nil
}

func failedMailsText(n int) string {
	if n == 1 {
		return "1 mail n'a pas pu partir."
	}
	return strconv.Itoa(n) + " mails n'ont pas pu partir."
}

// idleText names the open requests without activity for 12 months: they are
// flagged for review, never closed automatically (spec §8.3).
func idleText(rows []tickets.Row) string {
	refs := make([]string, len(rows))
	for i, row := range rows {
		refs[i] = row.Ref
	}
	if len(rows) == 1 {
		return "1 demande ouverte est sans activité depuis 12 mois, à revoir : " + refs[0] + "."
	}
	return strconv.Itoa(len(rows)) + " demandes ouvertes sont sans activité depuis 12 mois, à revoir : " + strings.Join(refs, ", ") + "."
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
