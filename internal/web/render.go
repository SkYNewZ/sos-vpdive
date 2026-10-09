package web

import (
	"bytes"
	"fmt"
	"html/template"
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
	Section   string          // committee sidebar entry of this page (nav.go)
	Tab       string          // committee phone tab of this page (nav.go)
	Account   *admins.Account // signed-in committee member
	Nav       []navItem       // committee navigation of the signed-in account
	Bare      bool            // the plain shell, even when signed in
	CSRF      string
	Notices   []notice
	ClubEmail string
	Umami     *umamiPage // nil: the page view is not counted
	Data      any
}

func parsePages(funcs template.FuncMap) (map[string]*template.Template, error) {
	names, err := fs.Glob(embedded, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	partials, err := fs.Glob(embedded, "templates/_*.html")
	if err != nil {
		return nil, fmt.Errorf("list partials: %w", err)
	}
	pages := map[string]*template.Template{}
	for _, n := range names {
		base := path.Base(n)
		if base == "layout.html" || strings.HasPrefix(base, "_") {
			continue
		}
		files := append([]string{"templates/layout.html"}, partials...)
		t, err := template.New(base).Funcs(funcs).ParseFS(embedded, append(files, n)...)
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
		Admin:     hostKey(r.Host, s.cfg.AdminBaseURL.Scheme) == s.cfg.AdminBaseURL.Host,
		ClubEmail: s.cfg.NotifyEmail.Address,
	}
	p.Umami = s.umamiPage(r.Pattern, p.Admin)
	if p.Admin {
		p.Section, p.Tab = navSection(r.URL.Path)
	}
	if sess, ok := sessionFrom(r.Context()); ok {
		a := sess.account
		p.Account = &a
		p.Nav = navFor(s.isOwner(a.Username), s.assistant != nil)
		p.CSRF = s.keys.CSRFToken(sess.hash)
	}
	return p
}

// render executes a page and sends it.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, p page) {
	if body, ok := s.execute(w, r, name, "layout", p); ok {
		s.write(w, r, status, "text/html; charset=utf-8", body)
	}
}

// renderFragment sends the template name of the page file name.html alone:
// a piece of page that a script fetches.
func (s *Server) renderFragment(w http.ResponseWriter, r *http.Request, name string, data any) {
	if body, ok := s.execute(w, r, name, name, data); ok {
		s.write(w, r, http.StatusOK, "text/html; charset=utf-8", body)
	}
}

// execute runs template tmpl of the page file name.html into a buffer, so a
// template error never sends half a page; on failure it answers 500.
func (s *Server) execute(w http.ResponseWriter, r *http.Request, name, tmpl string, data any) ([]byte, bool) {
	t, ok := s.pages[name]
	if !ok {
		s.logger.ErrorContext(r.Context(), "unknown page", "page", name)
		http.Error(w, "Erreur interne.", http.StatusInternalServerError)
		return nil, false
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, tmpl, data); err != nil {
		s.logger.ErrorContext(r.Context(), "render page", "page", name, "template", tmpl, "error", err)
		http.Error(w, "Erreur interne.", http.StatusInternalServerError)
		return nil, false
	}
	return buf.Bytes(), true
}

// writeText sends a short plain-text response.
func (s *Server) writeText(w http.ResponseWriter, r *http.Request, status int, text string) {
	s.write(w, r, status, "text/plain; charset=utf-8", []byte(text))
}

// write sends a response body built in advance.
func (s *Server) write(w http.ResponseWriter, r *http.Request, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		s.logger.DebugContext(r.Context(), "write response", "error", err)
	}
}
