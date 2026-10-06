package web

import (
	"context"
	"errors"
	"net/http"
	netmail "net/mail"
	"net/url"
	"regexp"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// Member form (spec §3.1–3.3, §11.3). A valid request is stored as a draft;
// when the model matches it with fiches, screen 2 shows them, otherwise the
// request is confirmed at once.
const (
	turnstileRequest = "demande"
	honeypotField    = "site_web"
	formEmailLimit   = 5
	unknownAddress   = "Cette adresse n'est pas celle d'un compte VPDive du club. Utilise l'adresse de ton compte, ou écris au club."
)

var refPattern = regexp.MustCompile(`^CPP-\d{4,}$`)

// formData is the member form: an empty one, or the one just sent with its
// values and its problems, keyed by input name.
type formData struct {
	Open         bool // a members list is imported (spec §3.6)
	SiteKey      string
	FormKey      string
	Categories   []tickets.Category
	Category     string
	Values       url.Values
	Errors       map[string]string
	CapturesLost bool // screenshots were sent: a browser never refills a file input
	Suggestions  bool // the description goes to the model (spec §11.7)
	FAQ          vpdiveLink
	Tarifs       vpdiveLink
}

// formField is one dedicated field as form.html renders it.
type formField struct {
	tickets.Field

	Name  string
	Value string
	Error string
}

func newFormField(categoryID string, f tickets.Field, d formData) formField {
	name := tickets.FieldName(categoryID, f.ID)
	return formField{Field: f, Name: name, Value: d.Values.Get(name), Error: d.Errors[name]}
}

func (s *Server) formPage(w http.ResponseWriter, r *http.Request) {
	open, err := s.members.HasList(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	key, err := secure.NewToken()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderForm(w, r, http.StatusOK, formData{Open: open, FormKey: key}, nil)
}

// renderForm shows the form; n, when set, says why it came back.
func (s *Server) renderForm(w http.ResponseWriter, r *http.Request, status int, d formData, n *notice) {
	p := s.newPage(r, "Demande d'aide")
	d.Categories = s.tickets.Catalog.Public()
	d.FAQ, d.Tarifs = s.vpdive["faq"], s.vpdive["tarifs"]
	d.Suggestions = s.suggest != nil
	if s.turnstile != nil {
		d.SiteKey = s.turnstile.SiteKey
	}
	if n == nil && len(d.Errors) > 0 {
		n = &notice{Kind: noticeError, Text: "Ta demande n'est pas partie : corrige les champs signalés."}
	}
	if n != nil {
		p.Notices = append(p.Notices, *n)
	}
	p.Data = d
	s.render(w, r, status, "form", p)
}

// submit files a request in the order of spec §3.2: anti-robot check, rate
// limits, whitelist. An unknown address never stores anything.
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	f, err := readMultipart(w, r, captureField)
	if err != nil {
		s.readError(w, r, err)
		return
	}
	ctx := r.Context()
	d := formData{
		Open: true, FormKey: f.values.Get("cle"), Category: f.values.Get("categorie"),
		Values: f.values, Errors: map[string]string{}, CapturesLost: len(f.files) > 0,
	}
	if !secure.IsToken(d.FormKey) {
		s.writeText(w, r, http.StatusBadRequest, "Formulaire expiré : recharge la page.\n")
		return
	}
	open, err := s.members.HasList(ctx)
	switch {
	case err != nil:
		s.serverError(w, r, err)
		return
	case !open:
		d.Open = false
		s.renderForm(w, r, http.StatusServiceUnavailable, d, nil)
		return
	case f.values.Get(honeypotField) != "":
		s.renderForm(w, r, http.StatusBadRequest, d, &notice{Kind: noticeError, Text: "Envoi refusé."})
		return
	}
	if status, n := s.checkBot(r, f.values.Get(turnstileField), s.cfg.BaseURL.Hostname(), turnstileRequest); n != nil {
		s.renderForm(w, r, status, d, n)
		return
	}
	// A resend of the same form (lost response, double tap) finds the request
	// already filed or its screen 2, whatever its new anti-robot token.
	out, found, err := s.tickets.Resubmitted(ctx, d.FormKey)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if found {
		s.filed(w, r, out)
		return
	}
	sub := s.readSubmission(&d)
	allowed, err := s.submitAllowed(ctx, s.clientIP(r).String(), sub.Email)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !allowed {
		// A duplicate POST can land while the first is still being filed and
		// count against the limit: the request exists, so confirm it.
		if out, found, err := s.tickets.Resubmitted(ctx, d.FormKey); err == nil && found {
			s.filed(w, r, out)
			return
		}
		s.renderForm(w, r, http.StatusTooManyRequests, d, &notice{Kind: noticeError,
			Text: "Trop d'envois en peu de temps. Réessaie dans une heure, ou écris au club : " + s.cfg.NotifyEmail.Address + "."})
		return
	}
	if len(d.Errors) == 0 {
		if err := s.checkMember(ctx, &d, sub.Email); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if len(d.Errors) == 0 {
		var msg string
		if sub.Captures, msg = sanitizeCaptures(f.files); msg != "" {
			d.Errors[captureField] = msg
		}
	}
	if len(d.Errors) > 0 {
		s.renderForm(w, r, http.StatusUnprocessableEntity, d, nil)
		return
	}
	out, err = s.tickets.Submit(ctx, sub, s.chooser(sub))
	switch {
	case errors.Is(err, tickets.ErrStorage):
		s.renderForm(w, r, http.StatusServiceUnavailable, d, &notice{Kind: noticeError,
			Text: "Tes captures n'ont pas pu être enregistrées, et ta demande n'est pas partie. Réessaie, avec ou sans captures."})
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.filed(w, r, out)
	}
}

// filed answers a sent form: the confirmation, or screen 2 behind its draft
// token. Screen 2 is the response itself: the token never goes into a URL.
func (s *Server) filed(w http.ResponseWriter, r *http.Request, out tickets.Outcome) {
	if out.Token == "" {
		redirectSent(w, r, out.Ref)
		return
	}
	s.renderBefore(w, r, http.StatusOK, out.Token, nil)
}

// readSubmission checks the typed fields. Problems land in d.Errors, keyed
// by input name, beside their field.
func (s *Server) readSubmission(d *formData) tickets.Submission {
	v := d.Values
	sub := tickets.Submission{FormKey: d.FormKey}
	var ok bool
	if sub.FirstName, ok = tickets.CleanText(v.Get("prenom"), 1, tickets.NameMax); !ok {
		d.Errors["prenom"] = "Indique ton prénom, 100 caractères au plus."
	}
	if sub.LastName, ok = tickets.CleanText(v.Get("nom"), 1, tickets.NameMax); !ok {
		d.Errors["nom"] = "Indique ton nom, 100 caractères au plus."
	}
	if email, err := secure.NormalizeEmail(v.Get("email")); err == nil && validAddress(email) {
		sub.Email = email
	} else {
		d.Errors["email"] = "Indique une adresse mail valide : celle de ton compte VPDive."
	}
	if sub.Description, ok = tickets.CleanText(v.Get("description"), tickets.DescriptionMin, tickets.DescriptionMax); !ok {
		d.Errors["description"] = "Décris ta demande en 20 à 4 000 caractères."
	}
	category, found := s.tickets.Catalog.Category(d.Category)
	if !found || category.CommitteeOnly {
		d.Errors["categorie"] = "Choisis une catégorie dans la liste."
		return sub
	}
	var problems map[string]string
	sub.Fields, problems = s.tickets.Catalog.ReadFields(category.ID, v.Get)
	for id, msg := range problems {
		d.Errors[tickets.FieldName(category.ID, id)] = msg
	}
	return sub
}

// submitAllowed applies the form limits of spec §11.3: FORM_RATE_LIMIT per
// address and per hour, then 5 per email and per hour.
func (s *Server) submitAllowed(ctx context.Context, ip, email string) (bool, error) {
	rules := []rule{{"form-ip:" + ip, s.cfg.FormRateLimit, time.Hour}}
	if email != "" {
		rules = append(rules, rule{"form-email:" + email, formEmailLimit, time.Hour})
	}
	return s.limiter.allowAll(ctx, rules...)
}

// checkMember blocks an address absent from the members list (spec §3.6),
// before anything is stored.
func (s *Server) checkMember(ctx context.Context, d *formData, email string) error {
	known, err := s.members.Lookup(ctx, email)
	if err != nil {
		return err
	}
	if !known {
		d.Errors["email"] = unknownAddress
	}
	return nil
}

// validAddress accepts a normalized address that net/mail reads back unchanged.
func validAddress(email string) bool {
	a, err := netmail.ParseAddress(email)
	return err == nil && a.Address == email
}

// readError answers a member form that could not be read.
func (s *Server) readError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errFormTooLarge) {
		s.writeText(w, r, http.StatusRequestEntityTooLarge,
			"Envoi trop volumineux : 3 captures de 5 Mo au plus. Reviens en arrière et retire des captures.\n")
		return
	}
	s.writeText(w, r, http.StatusBadRequest, "Envoi interrompu. Réessaie.\n")
}

// redirectSent shows the confirmation (spec §3.3). Only the reference is in
// the URL, never the tracking link.
func redirectSent(w http.ResponseWriter, r *http.Request, ref string) {
	http.Redirect(w, r, "/demandes/envoyee?ref="+url.QueryEscape(ref), http.StatusSeeOther)
}

func (s *Server) sentPage(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if !refPattern.MatchString(ref) {
		s.notFound(w, r)
		return
	}
	p := s.newPage(r, "Demande envoyée")
	p.Data = ref
	s.render(w, r, http.StatusOK, "sent", p)
}
