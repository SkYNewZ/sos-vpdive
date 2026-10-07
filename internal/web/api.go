package web

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/mail"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

// apiImportLimit is the number of pushed imports allowed per hour and per
// client address (spec §7.6).
const apiImportLimit = 10

// exportNames name each export in the mails to the club; their keys are the
// {type} values of the pushed-import route.
var exportNames = map[imports.Kind]string{
	imports.Members: "liste des membres", imports.Payments: "export des paiements", imports.Mollie: "export VPayDive",
	imports.Calendar: "calendrier",
}

// pushed is the answer to an accepted pushed import (spec §7.6): what the
// file held.
type pushed struct {
	Result  string `json:"result"` // imported or unchanged
	Read    int    `json:"read"`
	Kept    int    `json:"kept"`
	Skipped int    `json:"skipped"`
	ToCheck int    `json:"to_check"`
}

// pushRefused is the answer to a refused pushed import: a stable code and,
// for a refused file, the message the imports page would show.
type pushRefused struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// apiImport takes an export pushed by the external script (spec §7.6): no
// session and no Origin, a token; the same reading and validation as an
// upload, then a replacement without preview.
func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ok, err := s.limiter.allow(ctx, "import-api:"+s.clientIP(r).String(), apiImportLimit, time.Hour)
	switch {
	case err != nil:
		s.logger.ErrorContext(ctx, "internal error", "error", err)
		s.writeJSON(w, r, http.StatusInternalServerError, pushRefused{Error: "internal"})
		return
	case !ok:
		w.Header().Set("Retry-After", "3600")
		s.writeJSON(w, r, http.StatusTooManyRequests, pushRefused{Error: "rate_limited"})
		return
	}
	if !s.importTokenValid(r.Header.Get("Authorization")) {
		s.logger.WarnContext(ctx, "import token refused")
		s.writeJSON(w, r, http.StatusUnauthorized, pushRefused{Error: "unauthorized"})
		return
	}
	kind := imports.Kind(r.PathValue("type"))
	if _, known := exportNames[kind]; !known {
		s.writeJSON(w, r, http.StatusNotFound, pushRefused{Error: "unknown_type"})
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUploadBytes))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		s.refusePushed(w, r, kind, http.StatusRequestEntityTooLarge, pushRefused{Error: "too_large", Message: "Fichier trop volumineux : 5 Mo au plus."})
		return
	case err != nil:
		s.writeJSON(w, r, http.StatusBadRequest, pushRefused{Error: "interrupted"})
		return
	}
	exp, err := s.readExport(ctx, kind, data)
	if err == nil {
		err = telemetry.Trace(ctx, s.tracer, "import.replace", func(ctx context.Context) error { return s.importPushed(ctx, exp) })
	}
	answer := exp.counts()
	switch {
	case err == nil:
		answer.Result = "imported"
	case errors.Is(err, imports.ErrUnchanged):
		answer.Result = "unchanged"
	case errors.Is(err, imports.ErrTooFew):
		s.refusePushed(w, r, kind, http.StatusUnprocessableEntity, pushRefused{Error: "too_few", Message: tooFewMessage(kind)})
		return
	default:
		if code, msg, refused := refusal(kind, err); refused {
			s.refusePushed(w, r, kind, http.StatusUnprocessableEntity, pushRefused{Error: code, Message: msg})
			return
		}
		s.logger.ErrorContext(ctx, "internal error", "error", err)
		s.writeJSON(w, r, http.StatusInternalServerError, pushRefused{Error: "internal"})
		return
	}
	s.logger.InfoContext(ctx, "export pushed", "kind", string(kind), "result", answer.Result, "kept", answer.Kept)
	s.writeJSON(w, r, http.StatusOK, answer)
}

// tooFewMessage explains a pushed file under half of the data in place. The
// calendar has no manual upload to get past it (owner decision, lot 8).
func tooFewMessage(kind imports.Kind) string {
	if kind == imports.Calendar {
		return "Ce calendrier contient moins de la moitié des événements en place sur sa période. Vérifie le calendrier dans VPDive."
	}
	return "Ce fichier contient moins de la moitié des données en place. S'il est juste, dépose-le à la main sur la page des imports."
}

// importTokenValid compares the bearer token with IMPORT_TOKEN in constant
// time; hashing both first hides the length too. The scheme is
// case-insensitive (RFC 7235).
func (s *Server) importTokenValid(header string) bool {
	scheme, token, _ := strings.Cut(header, " ")
	return strings.EqualFold(scheme, "Bearer") &&
		subtle.ConstantTimeCompare(secure.TokenHash(token), secure.TokenHash(s.cfg.ImportToken)) == 1
}

// importPushed replaces the data of exp's kind without preview.
func (s *Server) importPushed(ctx context.Context, exp export) error {
	switch {
	case exp.members != nil:
		return s.members.Import(ctx, exp.members)
	case exp.payments != nil:
		return s.payments.Import(ctx, exp.payments)
	case exp.calendar != nil:
		return s.calendar.Import(ctx, exp.calendar)
	default:
		return s.mollie.Import(ctx, exp.mollie)
	}
}

// counts is what a pushed file held: rows read, kept, skipped (without a
// name, or without an email for members) and lines to check.
func (e export) counts() pushed {
	var p pushed
	switch {
	case e.members != nil:
		p.Kept, p.Skipped = len(e.members.Members), e.members.Skipped
	case e.payments != nil:
		p.Kept, p.Skipped, p.ToCheck = len(e.payments.Lines), e.payments.Skipped, e.payments.ToCheck()
	case e.mollie != nil:
		p.Kept, p.Skipped, p.ToCheck = len(e.mollie.Lines), e.mollie.Skipped, e.mollie.ToCheck()
	case e.calendar != nil:
		p.Kept, p.Skipped = len(e.calendar.Events), e.calendar.Skipped
	}
	p.Read = p.Kept + p.Skipped
	return p
}

// refusePushed answers a refused file and mails the committee (spec §7.6):
// a script nobody watches failed. The data in place did not change.
func (s *Server) refusePushed(w http.ResponseWriter, r *http.Request, kind imports.Kind, status int, answer pushRefused) {
	ctx := r.Context()
	s.logger.InfoContext(ctx, "pushed export refused", "kind", string(kind), "code", answer.Error)
	next := "Vérifie l'export dans VPDive, puis dépose-le à la main si besoin"
	if kind == imports.Calendar { // no manual upload for the calendar
		next = "Vérifie le calendrier dans VPDive. Le script d'import est peut-être en panne"
	}
	text := fmt.Sprintf("Le script d'import a déposé un fichier que l'outil a refusé : %s.\n\nRaison : %s\n\n"+
		"Les données en place n'ont pas changé. %s", exportNames[kind], answer.Message, next)
	if err := store.Tx(ctx, s.db, "import.refused", func(ctx context.Context, tx *sql.Tx) error {
		return s.queueImportsMail(ctx, tx, mail.EventImportRefused, "Import automatique refusé : "+exportNames[kind], text)
	}); err != nil {
		s.logger.ErrorContext(ctx, "queue refusal mail", "error", err)
	} else {
		s.outbox.Wake()
	}
	s.writeJSON(w, r, status, answer)
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.write(w, r, status, "application/json", append(body, '\n'))
}
