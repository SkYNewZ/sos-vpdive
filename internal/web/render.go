package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
)

// noticeKind selects the daisyUI alert color in templates/layout.html.
type noticeKind string

// Notice kinds, mapped to daisyUI alert colors in templates/layout.html;
// any other kind renders as information.
const (
	noticeSuccess noticeKind = "success"
	noticeWarning noticeKind = "warning"
	noticeError   noticeKind = "error"
)

// notice is a banner shown above the page content.
type notice struct {
	Kind     noticeKind
	Text     string
	Link     string
	LinkText string
}

// page is the data every template receives.
type page struct {
	Title     string
	Admin     bool            // committee domain
	Account   *admins.Account // signed-in committee member
	CSRF      string
	Notices   []notice
	ClubEmail string
	Data      any
}

func parsePages(funcs template.FuncMap) (map[string]*template.Template, error) {
	names, err := fs.Glob(embedded, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	pages := map[string]*template.Template{}
	for _, n := range names {
		base := path.Base(n)
		if base == "layout.html" {
			continue
		}
		t, err := template.New(base).Funcs(funcs).ParseFS(embedded, "templates/layout.html", n)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", base, err)
		}
		pages[strings.TrimSuffix(base, ".html")] = t
	}
	return pages, nil
}

// newPage fills the fields common to every page, including the signed-in
// account and its anti-CSRF token.
func (s *Server) newPage(r *http.Request, title string) page {
	p := page{
		Title:     title,
		Admin:     matchHost(r.Host, s.cfg.AdminBaseURL),
		ClubEmail: s.cfg.NotifyEmail.Address,
	}
	if sess, ok := sessionFrom(r.Context()); ok {
		a := sess.account
		p.Account = &a
		p.CSRF = s.keys.CSRFToken(sess.hash)
	}
	return p
}

// render executes a page into a buffer first, so a template error never
// sends half a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, p page) {
	t, ok := s.pages[name]
	if !ok {
		s.logger.ErrorContext(r.Context(), "unknown page", "page", name)
		http.Error(w, "Erreur interne.", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		s.logger.ErrorContext(r.Context(), "render page", "page", name, "error", err)
		http.Error(w, "Erreur interne.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		s.logger.DebugContext(r.Context(), "write response", "error", err)
	}
}

// writeText sends a short plain-text response.
func (s *Server) writeText(w http.ResponseWriter, r *http.Request, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, text); err != nil {
		s.logger.DebugContext(r.Context(), "write response", "error", err)
	}
}
