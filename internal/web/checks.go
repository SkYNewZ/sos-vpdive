package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
)

// checksData is the « Paiements à vérifier » page (spec §7.7): the treasurer's
// work list, oldest first.
type checksData struct {
	Open, Masked checkTable
	Summary      string        // counts of the open lines by signal
	Payments     *imports.Info // latest imports, nil when none
	Mollie       *imports.Info
	Links        []vpdiveLink
}

// checkTable is one table of the page; open lines carry the masking form.
type checkTable struct {
	Rows []checkView
	CSRF string
}

// checkView is a line to check with its age, counted from its date.
type checkView struct {
	payments.Check

	Age ageView
}

func (s *Server) checksPage(w http.ResponseWriter, r *http.Request) {
	var n *notice
	if r.URL.Query().Get("masquee") == "1" {
		n = &notice{Kind: noticeSuccess, Text: "Ligne masquée."}
	}
	s.renderChecks(w, r, http.StatusOK, n)
}

func (s *Server) renderChecks(w http.ResponseWriter, r *http.Request, status int, n *notice) {
	ctx := r.Context()
	open, masked, err := s.checks.List(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := checksData{Links: []vpdiveLink{s.vpdive["paiements"], s.vpdive["vpaydive"]}, Summary: checksSummary(payments.Tally(open))}
	for _, c := range open {
		d.Open.Rows = append(d.Open.Rows, checkView{Check: c, Age: s.age(c.Date, time.Time{})})
	}
	for _, c := range masked {
		d.Masked.Rows = append(d.Masked.Rows, checkView{Check: c, Age: s.age(c.Date, time.Time{})})
	}
	paid, ok, err := s.payments.LastImport(ctx)
	if err == nil && ok {
		d.Payments = &paid
	}
	collected, ok, cerr := s.mollie.LastImport(ctx)
	if err = errors.Join(err, cerr); err != nil {
		s.serverError(w, r, err)
		return
	}
	if ok {
		d.Mollie = &collected
	}
	p, err := s.adminPage(r, "Paiements à vérifier")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if n != nil {
		p.Notices = append(p.Notices, *n)
	}
	d.Open.CSRF = p.CSRF
	p.Data = d
	s.render(w, r, status, "anomalies", p)
}

// dismissCheck masks a line once checked (spec §7.7). The tool corrects
// nothing: the masking is all it records, with who and when.
func (s *Server) dismissCheck(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	err := s.checks.Dismiss(r.Context(), r.PostForm.Get("empreinte"), sess.account.Username)
	switch {
	case errors.Is(err, payments.ErrCheckNotFound):
		s.renderChecks(w, r, http.StatusNotFound, &notice{Kind: noticeWarning,
			Text: "Cette ligne n'est plus dans les exports importés : rien à masquer."})
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.logger.InfoContext(r.Context(), "check dismissed", "actor", sess.account.Username)
		http.Redirect(w, r, "/anomalies?masquee=1", http.StatusSeeOther)
	}
}

// checksSummary counts the open lines by signal, in a sentence; "" when
// there is none.
func checksSummary(unsettled, partial int) string {
	var parts []string
	if unsettled > 0 {
		parts = append(parts, plural(unsettled, "encaissée par Mollie et non soldée dans VPDive", "encaissées par Mollie et non soldées dans VPDive"))
	}
	if partial > 0 {
		parts = append(parts, plural(partial, "paiement partiel", "paiements partiels"))
	}
	if len(parts) == 0 {
		return ""
	}
	return plural(unsettled+partial, "ligne", "lignes") + " à vérifier : " + strings.Join(parts, ", ") + "."
}

// plural writes n with the singular or plural form: « 1 ligne », « 2 lignes ».
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
