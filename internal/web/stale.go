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

// importAge is how an import ages (spec §7.2, §7.3, §7.5): past its maximum
// age, the committee sees a banner and gets one mail.
type importAge struct {
	kind   imports.Kind
	maxAge time.Duration
	banner string // takes the import date
	link   string
}

func (s *Server) importAges() []importAge {
	return []importAge{
		{imports.Members, s.cfg.MembersMaxAge, "La liste des membres date du %s. Pense à refaire l'import.", importsPath},
		{imports.Payments, s.cfg.PaymentsMaxAge, "Les paiements datent du %s. Pense à refaire l'import.", "/imports#paiements-titre"},
		{imports.Mollie, s.cfg.VPayDiveMaxAge, "Les encaissements Mollie datent du %s. Pense à refaire l'import.", "/imports#encaissements-titre"},
	}
}

// staleImport is the latest import of a kind, past its maximum age.
type staleImport struct {
	importAge

	last imports.Info
}

// staleImports returns the latest imports past their maximum age.
func (s *Server) staleImports(ctx context.Context) ([]staleImport, error) {
	var out []staleImport
	for _, a := range s.importAges() {
		last, ok, err := imports.Last(ctx, s.db, a.kind)
		if err != nil {
			return nil, err
		}
		if ok && s.now().Sub(last.ImportedAt) > a.maxAge {
			out = append(out, staleImport{importAge: a, last: last})
		}
	}
	return out, nil
}

// AlertStaleImports mails the committee once about each import past its
// maximum age, on top of the banner (spec §7.6): with a script, a list that
// ages means the script is broken. meta remembers the import alerted, so a
// new import re-arms the alert. The daily purge job runs it.
func (s *Server) AlertStaleImports(ctx context.Context) error {
	stale, err := s.staleImports(ctx)
	if err != nil {
		return err
	}
	sent := false
	for _, st := range stale {
		alerted, err := s.alertStale(ctx, st.last)
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
		text := fmt.Sprintf("Le dernier import (%s) date du %s.\n\nSi un script dépose les exports, il est peut-être en panne. Refais l'import",
			exportNames[last.Kind], s.formatDate(last.ImportedAt))
		if err := s.queueImportsMail(ctx, tx, mail.EventImportStale, "Import ancien : "+exportNames[last.Kind], text); err != nil {
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

// queueImportsMail queues a mail about imports to the club inbox inside tx.
// text ends where the link to the imports page follows.
func (s *Server) queueImportsMail(ctx context.Context, tx *sql.Tx, ev mail.Event, subject, text string) error {
	return s.outbox.Enqueue(ctx, tx, mail.Mail{Event: ev, To: s.cfg.NotifyEmail.Address, Subject: subject,
		Text: text + " : " + s.cfg.AdminBaseURL.JoinPath("imports").String() + "\n\nNe réponds pas à ce mail.\n"})
}
