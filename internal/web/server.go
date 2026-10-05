// Package web serves both domains from one binary (spec §9.6): the members
// site and the committee site, chosen by the request's host name.
package web

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// Deps are the server's collaborators.
type Deps struct {
	Config  *config.Config
	DB      *sql.DB
	Keys    *secure.Keys
	Members *members.Store
	Admins  *admins.Registry
	Content fs.FS
	Logger  *slog.Logger
	Now     func() time.Time

	Turnstile *Turnstile // nil in development without keys
}

// Server routes requests to the members or the committee site by host.
type Server struct {
	cfg     *config.Config
	db      *sql.DB
	keys    *secure.Keys
	members *members.Store
	admins  *admins.Registry
	logger  *slog.Logger
	now     func() time.Time
	paris   *time.Location

	turnstile *Turnstile
	limiter   *limiter
	dummyHash string

	robots  robotsPolicy
	vpdive  vpdiveLinks
	assets  *assets
	pages   map[string]*template.Template
	public  http.Handler
	admin   http.Handler
	handler http.Handler
}

// New builds the server and validates the embedded content.
func New(d Deps) (*Server, error) {
	robots, err := loadRobots(d.Content)
	if err != nil {
		return nil, err
	}
	links, err := loadVPDiveLinks(d.Content, d.Config.VPDiveBaseURL)
	if err != nil {
		return nil, err
	}
	static, err := newAssets()
	if err != nil {
		return nil, err
	}
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		return nil, fmt.Errorf("time zone: %w", err)
	}
	s := &Server{
		cfg: d.Config, db: d.DB, keys: d.Keys, members: d.Members, admins: d.Admins,
		logger: d.Logger, now: d.Now, paris: paris,
		robots: robots, vpdive: links, assets: static,
	}
	s.turnstile = d.Turnstile
	s.limiter = &limiter{db: d.DB, keys: d.Keys, now: d.Now}
	if s.dummyHash, err = admins.HashPassword("dummy password for unknown usernames"); err != nil {
		return nil, err
	}
	if s.pages, err = parsePages(s.templateFuncs()); err != nil {
		return nil, err
	}
	s.public = s.requireOrigin(d.Config.BaseURL, s.publicRoutes())
	s.admin = s.requireOrigin(d.Config.AdminBaseURL, s.adminRoutes())
	s.handler = s.recoverPanics(s.withClientIP(securityHeaders(s.refuseAIRobots(http.HandlerFunc(s.route)))))
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// route picks the site by host name. An unknown host gets a 404 (spec §9.6).
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case matchHost(r.Host, s.cfg.BaseURL):
		s.public.ServeHTTP(w, r)
	case matchHost(r.Host, s.cfg.AdminBaseURL):
		s.admin.ServeHTTP(w, r)
	default:
		s.writeText(w, r, http.StatusNotFound, "Page introuvable.\n")
	}
}

func (s *Server) publicRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	s.commonRoutes(mux)
	s.handle(mux, "GET /{$}", s.publicHome)
	return mux
}

// adminRoutes serves the committee site.
func (s *Server) adminRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	s.commonRoutes(mux)
	s.handle(mux, "GET /connexion", s.loginForm)
	s.handle(mux, "POST /connexion", s.login)
	s.handle(mux, "POST /deconnexion", s.signedIn(s.logout))
	s.handle(mux, "GET /{$}", s.signedIn(s.home))
	return mux
}

// commonRoutes are served on both domains. Health checks and static files
// are neither traced nor logged (spec §9.9).
func (s *Server) commonRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /static/", s.assets.handler())
	s.handle(mux, "GET /robots.txt", s.robotsTxt)
	s.handle(mux, "/", s.notFound)
}

// handle registers an instrumented route. The catch-all is named
// "unmatched" so that no raw path reaches a span or a log line.
func (s *Server) handle(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	route := strings.Replace(pattern, "{$}", "", 1)
	if pattern == "/" {
		route = "unmatched"
	}
	mux.Handle(pattern, s.instrument(route, h))
}

func (s *Server) templateFuncs() template.FuncMap {
	return template.FuncMap{"static": s.assets.URL}
}

func (s *Server) publicHome(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "public_home", s.newPage(r, "Demande d'aide"))
}

func (s *Server) robotsTxt(w http.ResponseWriter, r *http.Request) {
	s.writeText(w, r, http.StatusOK, s.robots.txt())
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.writeText(w, r, http.StatusNotFound, "Page introuvable.\n")
}

// healthz answers 200 when the database answers (spec §9.2).
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		s.writeText(w, r, http.StatusServiceUnavailable, "unavailable\n")
		return
	}
	s.writeText(w, r, http.StatusOK, "ok\n")
}
