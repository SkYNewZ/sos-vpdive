package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"

	sosvpdive "github.com/SkYNewZ/sos-vpdive"
	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/blobs"
	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
	"github.com/SkYNewZ/sos-vpdive/internal/web"
)

const (
	tracerName           = "github.com/SkYNewZ/sos-vpdive/cmd/sos-vpdive"
	accountsPollInterval = 10 * time.Second
	purgeInterval        = 24 * time.Hour
	shutdownTimeout      = 30 * time.Second
)

// app is what serve runs. setup builds it without listening, so that tests
// can check every startup refusal.
type app struct {
	logger  *slog.Logger
	db      *sql.DB
	admins  *admins.Registry
	members *members.Store
	tickets *tickets.Store
	outbox  *mail.Outbox
	broker  *web.Broker
	web     *web.Server
}

// setup opens the database, refuses a SECRET_KEY that does not match it,
// loads the accounts file and the content files, releases the requests of
// accounts removed while the service was stopped, and builds the web server.
func setup(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*app, error) {
	keys, err := secure.NewKeys(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	if err := ensureDataDir(cfg); err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, store.FileName))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*app, error) { return nil, errors.Join(err, db.Close()) }
	if err := store.CheckKey(ctx, db, keys); err != nil {
		return fail(fmt.Errorf("refusing to start: %w", err))
	}
	registry, err := admins.Load(cfg.AdminsFile, logger)
	if err != nil {
		return fail(err)
	}
	catalog, err := tickets.LoadCatalog(sosvpdive.Content)
	if err != nil {
		return fail(err)
	}
	captures, err := blobs.New(cfg)
	if err != nil {
		return fail(err)
	}
	memberStore := members.NewStore(db, keys, time.Now)
	outbox := mail.NewOutbox(db, keys, time.Now)
	broker := web.NewBroker()
	ticketStore := tickets.NewStore(tickets.Deps{
		DB: db, Keys: keys, Catalog: catalog, Members: memberStore, Outbox: outbox, Blobs: captures,
		Account: registry.Get, BaseURL: cfg.BaseURL, AdminBaseURL: cfg.AdminBaseURL,
		ClubEmail: cfg.NotifyEmail.Address, RetentionDays: cfg.RetentionDays,
		Now: time.Now, Logger: logger, OnChange: broker.Publish,
	})
	a := &app{logger: logger, db: db, admins: registry, members: memberStore, tickets: ticketStore, outbox: outbox, broker: broker}
	if err := ticketStore.ReleaseMissing(ctx, a.knownAccount); err != nil {
		return fail(fmt.Errorf("release requests of removed accounts: %w", err))
	}
	var turnstile *web.Turnstile
	if cfg.TurnstileEnabled() {
		turnstile = web.NewTurnstile(cfg.TurnstileSiteKey, cfg.TurnstileSecretKey, "")
	}
	a.web, err = web.New(web.Deps{
		Config: cfg, DB: db, Keys: keys, Members: memberStore, Admins: registry,
		Content: sosvpdive.Content, Logger: logger, Now: time.Now, Turnstile: turnstile,
		Tickets: ticketStore, Catalog: catalog, Outbox: outbox, Broker: broker,
	})
	if err != nil {
		return fail(err)
	}
	return a, nil
}

func (a *app) knownAccount(username string) bool {
	_, ok := a.admins.Get(username)
	return ok
}

// onAccountsChange revokes the sessions of removed or changed accounts and
// returns the open requests of removed accounts to "à traiter" (spec §4.1).
func (a *app) onAccountsChange(ctx context.Context, users []string) {
	if err := a.web.RevokeSessions(ctx, users); err != nil {
		a.logger.ErrorContext(ctx, "revoke sessions", "error", err)
	}
	if err := a.tickets.ReleaseMissing(ctx, a.knownAccount); err != nil {
		a.logger.ErrorContext(ctx, "release requests of removed accounts", "error", err)
	}
}

// serve runs until SIGTERM, SIGINT or ctx ends, then finishes the requests in
// flight (spec §9.2).
func serve(ctx context.Context, getenv func(string) string, stdout io.Writer) (err error) {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	logger := telemetry.NewLogger(stdout, cfg.LogLevel)
	shutdownTraces, err := telemetry.Setup(ctx, logger)
	if err != nil {
		return err
	}
	a, err := setup(ctx, cfg, logger)
	if err != nil {
		return errors.Join(err, shutdownTraces(context.WithoutCancel(ctx)))
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	var jobs sync.WaitGroup
	jobs.Go(func() { a.admins.Watch(ctx, accountsPollInterval, a.onAccountsChange) })
	jobs.Go(func() { a.runPurges(ctx) })
	jobs.Go(func() { a.outbox.Run(ctx, mail.NewSMTP(cfg.SMTP, cfg.MailFrom), logger) })

	httpServer := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           a.web,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	// Shutdown waits for idle connections: live board streams never are.
	httpServer.RegisterOnShutdown(a.broker.Close)
	listenErr := make(chan error, 1)
	go func() { listenErr <- httpServer.ListenAndServe() }()
	logger.InfoContext(ctx, "listening", "port", cfg.Port, "env", string(cfg.Env))

	select {
	case err := <-listenErr:
		stop()
		jobs.Wait()
		return errors.Join(fmt.Errorf("http server: %w", err), a.db.Close(), shutdownTraces(context.WithoutCancel(ctx)))
	case <-ctx.Done():
	}
	detached := context.WithoutCancel(ctx) // ctx is done: shutdown needs a live one
	logger.InfoContext(detached, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(detached, shutdownTimeout)
	defer cancel()
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	jobs.Wait()
	return errors.Join(shutdownErr, shutdownTraces(shutdownCtx), a.db.Close())
}

// runPurges applies the retention rules at startup, then daily (spec §8.3).
func (a *app) runPurges(ctx context.Context) {
	ticker := time.NewTicker(purgeInterval)
	defer ticker.Stop()
	for {
		a.purge(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// purge applies the retention rules of spec §8.3 and removes orphan screenshots.
func (a *app) purge(ctx context.Context) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "job.purge")
	defer span.End()
	steps := []struct {
		code string
		run  func(context.Context) error
	}{
		{"purge_sessions", a.web.Purge},
		{"purge_members", a.members.Purge},
		{"purge_tickets", a.tickets.Purge},
		{"purge_outbox", a.outbox.Purge},
		{"sweep_captures", a.tickets.SweepOrphans},
	}
	for _, step := range steps {
		if err := step.run(ctx); err != nil && ctx.Err() == nil {
			telemetry.Fail(span, step.code)
			a.logger.ErrorContext(ctx, "purge", "step", step.code, "error", err)
		}
	}
}
