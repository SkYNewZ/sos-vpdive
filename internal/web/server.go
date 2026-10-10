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
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/carnets"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/kb"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/push"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/suggest"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// Deps are the server's collaborators.
type Deps struct {
	Config   *config.Config
	DB       *sql.DB
	Keys     *secure.Keys
	Members  *members.Store
	Payments *payments.Store
	Mollie   *payments.MollieStore
	Calendar *calendar.Store // the pushed activity calendar (lot 8)
	Carnets  *carnets.Store  // the carnet cards pushed by the script (design 2026-10-09)
	Admins   *admins.Registry
	Tickets  *tickets.Store
	Outbox   *mail.Outbox
	Push     *push.Store                                         // committee devices subscribed to Web Push
	PushTest func(ctx context.Context, sessionHash []byte) error // test notification to a session's device; nil without VAPID keys
	Broker   *Broker                                             // shared with tickets.Deps.OnChange
	KB       *kb.Base
	Content  fs.FS
	Logger   *slog.Logger
	Now      func() time.Time

	Turnstile *Turnstile // nil in development without keys
}

// Server routes requests to the members or the committee site by host.
type Server struct {
	cfg       *config.Config
	db        *sql.DB
	keys      *secure.Keys
	members   *members.Store
	payments  *payments.Store
	mollie    *payments.MollieStore
	calendar  *calendar.Store
	carnets   *carnets.Store
	checks    *payments.CheckStore
	admins    *admins.Registry
	tickets   *tickets.Store
	outbox    *mail.Outbox
	push      *push.Store
	pushTest  func(ctx context.Context, sessionHash []byte) error
	broker    *Broker
	kb        *kb.Base
	suggest   *suggest.Client // nil without LLM_API_KEY: no screen 2
	fiches    []suggest.Fiche // what the model reads of the knowledge base
	keepAlive time.Duration   // event stream keepalive and session check, shortened by tests
	logger    *slog.Logger
	now       func() time.Time
	paris     *time.Location
	tracer    trace.Tracer

	assistant       *assistant.Client // nil when the assistant is off
	convs           *assistant.Store
	assistantPrompt string

	turnstile *Turnstile
	limiter   *limiter
	dummyHash string

	// ponytail: one login at a time (single instance, a few logins a day); per-key locks if volume grows.
	loginMu sync.Mutex

	robots  robotsPolicy
	vpdive  vpdiveLinks
	labels  calendarLabels // config/calendar.yaml
	assets  *assets
	apps    map[bool]installable // by committee host
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
	labels, err := loadCalendarLabels(d.Content)
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
		cfg: d.Config, db: d.DB, keys: d.Keys, members: d.Members, payments: d.Payments, mollie: d.Mollie, calendar: d.Calendar, carnets: d.Carnets,
		checks: payments.NewCheckStore(d.DB, d.Keys, d.Members, d.Now), admins: d.Admins,
		tickets: d.Tickets, outbox: d.Outbox, push: d.Push, pushTest: d.PushTest, broker: d.Broker, kb: d.KB, suggest: suggest.New(d.Config.LLM),
		keepAlive: keepAliveInterval,
		logger:    d.Logger, now: d.Now, paris: paris, tracer: otel.Tracer(tracerName),
		turnstile: d.Turnstile,
		limiter:   &limiter{db: d.DB, keys: d.Keys, now: d.Now},
		robots:    robots, vpdive: links, labels: labels, assets: static,
	}
	if s.dummyHash, err = dummyHash(); err != nil {
		return nil, err
	}
	if s.apps, err = newInstallables(static); err != nil {
		return nil, err
	}
	for _, f := range d.KB.Fiches {
		s.fiches = append(s.fiches, suggest.Fiche{ID: f.ID, Title: f.Title, Answer: f.AnswerText})
	}
	if a := d.Config.Assistant; a != nil {
		s.assistant = assistant.NewClient(a)
		s.convs = assistant.NewStore(d.Now)
		s.assistantPrompt = assistantSystem(d.KB)
	}
	funcs := template.FuncMap{
		"static": s.assets.URL, "formatTime": s.formatTime, "formatDate": s.formatDate, "shortPeriod": payments.ShortPeriod, "author": s.tickets.AccountName,
		"age": s.age, "accountOf": s.accountOf, "actor": s.actorName, "isoDate": isoDate,
		"fieldName": tickets.FieldName, "categoryLabel": func(id string) string { return s.tickets.Catalog.CategoryLabel(id) }, "describe": s.tickets.Describe,
		"formField": newFormField, "themeColor": func() string { return themeColor }, "methodLabel": methodLabel,
		"category":    s.labels.category,
		"activity":    func(key string) string { return label(s.labels.Activities, key) },
		"environment": func(key string) string { return label(s.labels.Environments, key) },
		"roles":       s.labels.roles, "join": func(v []string) string { return strings.Join(v, ", ") },
		"when":      func(ev calendar.Event) string { return eventWhen(ev, s.paris) },
		"hours":     func(ev calendar.Event) string { return eventHours(ev, s.paris) },
		"cellHours": func(ev calendar.Event, day time.Time) string { return cellHours(ev, day, s.paris) },
		"shortDay":  func(t time.Time) string { return frShortDay(t.In(s.paris)) },
		"longDay":   func(t time.Time) string { return frLongDay(t.In(s.paris)) },
		"cart":      cartText,
		"dollars":   dollars,
		"plural":    plural,
	}
	if s.pages, err = parsePages(funcs); err != nil {
		return nil, err
	}
	s.public = s.requireOrigin(d.Config.BaseURL, s.publicRoutes())
	s.admin = s.apiRoutes(s.requireOrigin(d.Config.AdminBaseURL, s.adminRoutes()))
	csp := contentSecurityPolicy(d.Config.Umami)
	s.handler = s.recoverPanics(securityHeaders(csp, s.refuseAIRobots(http.HandlerFunc(s.route))))
	return s, nil
}

// dummyHash is computed once per process: unknown usernames are verified
// against it so that login time does not reveal which accounts exist.
var dummyHash = sync.OnceValues(func() (string, error) {
	return admins.HashPassword("dummy password for unknown usernames")
})

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// route picks the site by host name. An unknown host gets a 404 (spec §9.6).
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case hostKey(r.Host, s.cfg.BaseURL.Scheme) == s.cfg.BaseURL.Host:
		s.public.ServeHTTP(w, r)
	case hostKey(r.Host, s.cfg.AdminBaseURL.Scheme) == s.cfg.AdminBaseURL.Host:
		s.admin.ServeHTTP(w, r)
	default:
		s.notFound(w, r)
	}
}

func (s *Server) publicRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	s.commonRoutes(mux, false)
	s.handle(mux, "GET /{$}", s.formPage)
	s.handle(mux, "POST /demandes", s.submit)
	s.handle(mux, "POST /demandes/confirmer", s.confirmDraft)
	s.handle(mux, "POST /demandes/abandonner", s.abandonDraft)
	s.handle(mux, "POST /demandes/lien", s.resendDraftLink)
	s.handle(mux, "GET /demandes/abandonnee", s.solvedPage)
	s.handle(mux, "GET /demandes/envoyee", s.sentPage)
	s.handle(mux, "GET /suivi/{jeton}", s.trackingPage)
	s.handle(mux, "POST /suivi/{jeton}/reponse", s.memberReply)
	s.handle(mux, "POST /suivi/{jeton}/cloture", s.memberClose)
	s.handle(mux, "GET /suivi/{jeton}/captures/{id}", s.memberCapture)
	s.handle(mux, "GET /retrouver", s.linksPage)
	s.handle(mux, "POST /retrouver", s.requestLinks)
	return mux
}

// adminRoutes serves the committee site.
func (s *Server) adminRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	s.commonRoutes(mux, true)
	s.handle(mux, "GET /connexion", s.loginForm)
	s.handle(mux, "POST /connexion", s.login)
	s.handle(mux, "POST /deconnexion", s.signedIn(s.logout))
	s.handle(mux, "GET /imports", s.signedIn(s.importsPage))
	s.handle(mux, "POST /imports", s.signedIn(s.uploadImport))
	s.handle(mux, "POST /imports/confirmer", s.signedIn(s.confirmImport))
	s.handle(mux, "GET /demandes/{id}", s.signedIn(s.ticketPage))
	s.handle(mux, "POST /demandes/{id}/actions", s.signedIn(s.ticketAction))
	s.handle(mux, "GET /demandes/{id}/captures/{cid}", s.signedIn(s.adminCapture))
	s.handle(mux, "GET /demandes/{id}/toast", s.signedIn(s.toast))
	s.handle(mux, "GET /effacement", s.signedIn(s.erasurePage))
	s.handle(mux, "POST /effacement", s.signedIn(s.erase))
	s.handle(mux, "GET /fiches", s.signedIn(s.fichesPage))
	s.handle(mux, "GET /annulations", s.signedIn(s.cancellationsPage))
	s.handle(mux, "GET /calendrier", s.signedIn(s.calendarPage))
	s.handle(mux, "GET /calendrier/{id}", s.signedIn(s.eventPage))
	s.handle(mux, "GET /anomalies", s.signedIn(s.checksPage))
	s.handle(mux, "POST /anomalies/masquer", s.signedIn(s.dismissCheck))
	s.handle(mux, "GET /envois", s.signedIn(s.failedMails))
	s.handle(mux, "GET /notifications", s.signedIn(s.notificationsPage))
	if s.cfg.PushoverToken != "" {
		s.handle(mux, "POST /notifications/pushover", s.signedIn(s.savePushover))
	}
	s.handle(mux, "GET /plus", s.signedIn(s.plusPage))
	s.handle(mux, "GET "+accountPath, s.signedIn(s.accountPage))
	s.handle(mux, "POST "+accountPath, s.signedIn(s.changePassword))
	s.handle(mux, "POST "+accountPath+"/photo", s.signedIn(s.setPhoto))
	s.handle(mux, "GET "+accountsPath, s.ownerOnly(s.accountsPage))
	s.handle(mux, "POST "+accountsPath, s.ownerOnly(s.createAccount))
	s.handle(mux, "GET "+accountsPath+"/{identifiant}", s.ownerOnly(s.managedAccountPage))
	s.handle(mux, "POST "+accountsPath+"/{identifiant}/mot-de-passe", s.ownerOnly(s.resetAccount))
	s.handle(mux, "POST "+accountsPath+"/{identifiant}/suppression", s.ownerOnly(s.deleteAccount))
	if s.cfg.VAPID != nil {
		s.handle(mux, "POST /push/abonnement", s.signedIn(s.subscribePush))
		s.handle(mux, "POST /push/desabonnement", s.signedIn(s.unsubscribePush))
		s.handle(mux, "POST /push/test", s.signedIn(s.testPush))
	}
	s.handle(mux, "POST /envois/{id}/relancer", s.signedIn(s.retryMail))
	// The event stream is neither traced nor logged (spec §9.9).
	mux.HandleFunc("GET /evenements", s.events)
	if s.assistant != nil {
		s.handle(mux, "GET /assistant", s.signedIn(s.assistantPage))
		s.handle(mux, "GET /assistant/journal", s.ownerOnly(s.assistantJournal))
		s.handle(mux, "GET /assistant/{id}", s.signedIn(s.assistantConversation))
		s.handle(mux, "POST /assistant/messages", s.signedIn(s.assistantAsk))
	}
	s.handle(mux, "GET /{$}", s.signedIn(s.board))
	return mux
}

// apiRoutes puts the pushed import before the committee site: it escapes the
// session and the Origin check, a script sends neither and its token protects
// it (spec §11.2). Without IMPORT_TOKEN the route answers 404 (spec §7.6).
func (s *Server) apiRoutes(site http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	if s.cfg.ImportToken != "" {
		s.handle(mux, "POST /api/imports/{type}", s.apiImport)
		s.handle(mux, "POST /api/imports/{type}/failure", s.apiImportFailure)
	} else {
		s.handle(mux, "/api/imports/", s.notFound)
	}
	mux.Handle("/", site)
	return mux
}

// offlinePath is the page the service worker shows when a navigation fails.
const offlinePath = "/hors-ligne"

// commonRoutes are served on both domains, each with its own app (spec
// §9.6). Health checks and static files are neither traced nor logged
// (spec §9.9).
func (s *Server) commonRoutes(mux *http.ServeMux, admin bool) {
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /static/", s.assets.handler())
	s.handle(mux, "GET /kb/{file}", s.kbDiagram)
	s.handle(mux, "GET /robots.txt", s.robotsTxt)
	s.handle(mux, "GET /manifest.webmanifest", s.manifestFile(admin))
	s.handle(mux, "GET /sw.js", s.serviceWorker(admin))
	s.handle(mux, "GET "+offlinePath, s.offlinePage)
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
