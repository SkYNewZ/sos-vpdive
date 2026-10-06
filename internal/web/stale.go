package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// AlertStaleImports mails the committee once about each import past its
// maximum age, on top of the banner (spec §7.6): with a script, a list that
// ages means the script is broken. meta remembers the import alerted, so a
// new import re-arms the alert. The daily purge job runs it.
func (s *Server) AlertStaleImports(ctx context.Context) error {
	sent := false
	for _, k := range []struct {
		kind   imports.Kind
		maxAge time.Duration
	}{
		{imports.Members, s.cfg.MembersMaxAge},
		{imports.Payments, s.cfg.PaymentsMaxAge},
		{imports.Mollie, s.cfg.VPayDiveMaxAge},
	} {
		last, ok, err := imports.Last(ctx, s.db, k.kind)
		if err != nil {
			return err
		}
		if !ok || s.now().Sub(last.ImportedAt) <= k.maxAge {
			continue
		}
		alerted, err := s.alertStale(ctx, last)
		if err != nil {
			return err
		}
		sent = sent || alerted
	}
	if sent {
		s.outbox.Wake()
	}
	return nil
}

// alertStale queues the alert about last unless it went already, and
// records it in the same transaction.
func (s *Server) alertStale(ctx context.Context, last imports.Info) (bool, error) {
	key, id := "stale_alert:"+string(last.Kind), strconv.FormatInt(last.ID, 10)
	alerted := false
	err := store.Tx(ctx, s.db, "import.stale_alert", func(ctx context.Context, tx *sql.Tx) error {
		var done []byte
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&done)
		switch {
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("read %s: %w", key, err)
		case string(done) == id:
			return nil
		}
		text := fmt.Sprintf("Le dernier import (%s) date du %s.\n\n"+
			"Si un script dépose les exports, il est peut-être en panne. Refais l'import : %s\n\n"+
			"Ne réponds pas à ce mail.\n", exportNames[last.Kind], s.formatDate(last.ImportedAt), s.cfg.AdminBaseURL.JoinPath("imports"))
		if err := s.outbox.Enqueue(ctx, tx, mail.Mail{Event: mail.EventImportStale, To: s.cfg.NotifyEmail.Address,
			Subject: "Import ancien : " + exportNames[last.Kind], Text: text}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, []byte(id)); err != nil {
			return fmt.Errorf("record %s: %w", key, err)
		}
		alerted = true
		return nil
	})
	return alerted, err
}
