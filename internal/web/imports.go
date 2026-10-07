package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
	"github.com/SkYNewZ/sos-vpdive/internal/xlsx"
)

// Upload limits (spec §7.2): 5 MB file, 50 MB decompressed, 20 000 rows.
const (
	maxUploadBytes = 5 << 20
	multipartSlack = 64 << 10
	maxFieldBytes  = 1 << 10
	maxListedRows  = 20
	dateTimeFormat = "02/01/2006 à 15:04"
	dateFormat     = "02/01/2006"
)

// Import kinds, as the forms name them.
const (
	kindMembers  = "membres"
	kindPayments = "paiements"
	kindMollie   = "mollie"
)

// formKinds maps the type field of the import forms to the journal kind.
var formKinds = map[string]imports.Kind{kindMembers: imports.Members, kindPayments: imports.Payments, kindMollie: imports.Mollie}

// errUnreadable wraps the errors of reading a workbook: the file is refused.
var errUnreadable = errors.New("unreadable workbook")

// importsData is the imports page: one section per export, each with the
// errors of its own forms beside their action (spec §12.1).
type importsData struct {
	Members  membersSection
	Payments paymentsSection
	Mollie   mollieSection
}

// importErrors are shown beside the upload and the confirmation of a section.
type importErrors struct {
	Upload, Confirm string
}

type membersSection struct {
	importErrors

	Link    vpdiveLink
	Last    *imports.Info
	Preview *members.Preview
}

// linesSection is what the payments and Mollie sections share: lines
// attributed at display time, purged after 90 days, some of them to check.
type linesSection struct {
	importErrors

	Link    vpdiveLink
	Last    *imports.Info
	Purged  bool // lines removed after 90 days without an import
	Report  payments.Report
	ToCheck int // lines of « Paiements à vérifier » in place (spec §7.7)
}

type paymentsSection struct {
	linesSection

	Preview *payments.Preview
}

type mollieSection struct {
	linesSection

	Preview *payments.MolliePreview
}

// errs returns the error slots of the section of kind.
func (d *importsData) errs(kind string) *importErrors {
	switch kind {
	case kindPayments:
		return &d.Payments.importErrors
	case kindMollie:
		return &d.Mollie.importErrors
	}
	return &d.Members.importErrors
}

// failed is the page with msg beside the upload form of kind.
func failed(kind, msg string) importsData {
	var d importsData
	d.errs(kind).Upload = msg
	return d
}

func (s *Server) importsPage(w http.ResponseWriter, r *http.Request) {
	var done *notice
	switch r.URL.Query().Get("importe") {
	case kindMembers:
		done = &notice{Kind: noticeSuccess, Text: "Liste des membres importée."}
	case kindPayments:
		done = &notice{Kind: noticeSuccess, Text: "Paiements importés."}
	case kindMollie:
		done = &notice{Kind: noticeSuccess, Text: "Encaissements Mollie importés."}
	}
	s.renderImports(w, r, http.StatusOK, done, importsData{})
}

// renderImports shows the page with what an action left in d, the latest
// imports and the attribution of the payment lines in place.
func (s *Server) renderImports(w http.ResponseWriter, r *http.Request, status int, n *notice, d importsData) {
	p, err := s.adminPage(r, "Imports")
	if err == nil {
		err = s.importsView(r.Context(), &d)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if n != nil {
		p.Notices = append(p.Notices, *n)
	}
	p.Data = d
	s.render(w, r, status, "imports", p)
}

func (s *Server) importsView(ctx context.Context, d *importsData) error {
	d.Members.Link, d.Payments.Link, d.Mollie.Link = s.vpdive["membres"], s.vpdive["paiements"], s.vpdive["vpaydive"]
	last, ok, err := s.members.LastImport(ctx)
	if err != nil {
		return err
	}
	if ok {
		d.Members.Last = &last
	}
	if err := fillLines(ctx, &d.Payments.linesSection, s.payments); err != nil {
		return err
	}
	if err := fillLines(ctx, &d.Mollie.linesSection, s.mollie); err != nil {
		return err
	}
	d.Mollie.ToCheck, d.Payments.ToCheck, err = s.checks.Count(ctx)
	return err
}

// lineStore is what the imports page reads of the payments and Mollie stores.
type lineStore interface {
	LastImport(ctx context.Context) (imports.Info, bool, error)
	Report(ctx context.Context) (payments.Report, error)
	Expired(info imports.Info) bool
}

// fillLines fills the latest import of a lines section and the attribution
// of the lines in place: none after an import older than 90 days means the
// purge, while a recent import may hold no line at all.
func fillLines(ctx context.Context, sec *linesSection, store lineStore) error {
	last, ok, err := store.LastImport(ctx)
	if err != nil || !ok {
		return err
	}
	sec.Last = &last
	sec.Report, err = store.Report(ctx)
	sec.Purged = sec.Report == payments.Report{} && store.Expired(last)
	return err
}

// uploadImport reads, validates and previews an export. Nothing changes
// before the confirmation.
func (s *Server) uploadImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+multipartSlack)
	kind, data, ok := s.readUpload(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	exp, err := s.readExport(ctx, formKinds[kind], data)
	if err != nil {
		if _, msg, refused := refusal(formKinds[kind], err); refused {
			s.renderImports(w, r, http.StatusUnprocessableEntity, nil, failed(kind, msg))
			return
		}
		s.serverError(w, r, err)
		return
	}
	sess, _ := sessionFrom(ctx)
	d, err := s.preview(ctx, sess.account.Username, exp)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderImports(w, r, http.StatusOK, nil, d)
}

// export is a parsed export: the field of its kind is set.
type export struct {
	members  *members.Export
	payments *payments.Export
	mollie   *payments.MollieExport
}

// readExport reads and validates an export of kind, whatever its source:
// the upload form and the pushed route share it (spec §7.6). The export
// carries the hash of the file, which the journal keeps.
func (s *Server) readExport(ctx context.Context, kind imports.Kind, data []byte) (export, error) {
	var (
		rows    []xlsx.Row
		created time.Time // the members export dates itself in row 2
	)
	if err := telemetry.Trace(ctx, s.tracer, "import.read", func(context.Context) error {
		var err error
		rows, err = xlsx.ReadFirstSheet(data, imports.Limits())
		if err == nil && kind != imports.Members {
			created, _ = xlsx.Created(data, imports.Limits())
		}
		return err
	}); err != nil {
		return export{}, fmt.Errorf("%w: %w", errUnreadable, err)
	}
	hash := s.keys.Hash(string(data))
	var exp export
	err := telemetry.Trace(ctx, s.tracer, "import.validate", func(context.Context) error {
		var err error
		switch kind {
		case imports.Members:
			if exp.members, err = members.Parse(rows, s.paris); err == nil {
				exp.members.FileHash = hash
			}
		case imports.Payments:
			if exp.payments, err = payments.Parse(rows, created, s.paris); err == nil {
				exp.payments.FileHash = hash
			}
		case imports.Mollie:
			if exp.mollie, err = payments.ParseMollie(rows, created, s.paris); err == nil {
				exp.mollie.FileHash = hash
			}
		}
		return err
	})
	return exp, err
}

// preview keeps the preview of exp for username.
func (s *Server) preview(ctx context.Context, username string, exp export) (importsData, error) {
	var (
		d   importsData
		err error
	)
	switch {
	case exp.members != nil:
		d.Members.Preview, err = s.members.NewPreview(ctx, username, exp.members)
	case exp.payments != nil:
		d.Payments.Preview, err = s.payments.NewPreview(ctx, username, exp.payments)
	default:
		d.Mollie.Preview, err = s.mollie.NewPreview(ctx, username, exp.mollie)
	}
	return d, err
}

// readUpload reads the multipart body in memory: the CSRF field first, then
// the kind of export, then the file. ParseMultipartForm is not used because
// it spills large files to disk, and the export must never touch the disk
// (spec §7.2).
func (s *Server) readUpload(w http.ResponseWriter, r *http.Request) (string, []byte, bool) {
	mr, err := r.MultipartReader()
	if err != nil {
		s.writeText(w, r, http.StatusBadRequest, "Requête refusée : envoi de fichier attendu.\n")
		return "", nil, false
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "csrf" {
		s.forbidCSRF(w, r)
		return "", nil, false
	}
	token, err := io.ReadAll(io.LimitReader(part, maxFieldBytes))
	if err != nil || !s.csrfValid(r, string(token)) {
		s.forbidCSRF(w, r)
		return "", nil, false
	}
	part, err = mr.NextPart()
	var kind []byte
	if err == nil && part.FormName() == "type" {
		kind, err = io.ReadAll(io.LimitReader(part, maxFieldBytes))
	}
	if _, known := formKinds[string(kind)]; err != nil || !known {
		s.writeText(w, r, http.StatusBadRequest, "Requête refusée : type d'import inconnu.\n")
		return "", nil, false
	}
	missing := failed(string(kind), "Choisis le fichier exporté depuis VPDive avant d'envoyer.")
	part, err = mr.NextPart()
	if err != nil || part.FormName() != "file" {
		s.renderImports(w, r, http.StatusUnprocessableEntity, nil, missing)
		return "", nil, false
	}
	data, err := io.ReadAll(io.LimitReader(part, maxUploadBytes+1))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig) || len(data) > maxUploadBytes:
		s.renderImports(w, r, http.StatusRequestEntityTooLarge, nil, failed(string(kind), "Fichier trop volumineux : 5 Mo au plus."))
		return "", nil, false
	case err != nil:
		s.writeText(w, r, http.StatusBadRequest, "Envoi interrompu. Réessaie.\n")
		return "", nil, false
	case len(data) == 0:
		s.renderImports(w, r, http.StatusUnprocessableEntity, nil, missing)
		return "", nil, false
	}
	return string(kind), data, true
}

func (s *Server) confirmImport(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	username := sess.account.Username
	kind, id := r.PostForm.Get("type"), r.PostForm.Get("apercu")
	var confirm func(ctx context.Context, id, username string, secondConfirm bool) error
	switch kind {
	case kindMembers:
		confirm = s.members.Confirm
	case kindPayments:
		confirm = s.payments.Confirm
	case kindMollie:
		confirm = s.mollie.Confirm
	default:
		s.writeText(w, r, http.StatusBadRequest, "Requête refusée : type d'import inconnu.\n")
		return
	}
	err := telemetry.Trace(r.Context(), s.tracer, "import.replace", func(ctx context.Context) error {
		return confirm(ctx, id, username, r.PostForm.Get("confirmer_moitie") == "oui")
	})
	switch {
	case err == nil:
		http.Redirect(w, r, importsPath+"?importe="+kind, http.StatusSeeOther)
	case errors.Is(err, imports.ErrSecondConfirmRequired):
		d, perr := s.livePreview(kind, id, username)
		if perr != nil {
			s.renderImports(w, r, http.StatusConflict, expiredNotice(), importsData{})
			return
		}
		s.renderImports(w, r, http.StatusUnprocessableEntity, nil, d)
	case errors.Is(err, imports.ErrPreviewNotFound):
		s.renderImports(w, r, http.StatusConflict, expiredNotice(), importsData{})
	case errors.Is(err, imports.ErrStale):
		s.renderImports(w, r, http.StatusConflict, &notice{Kind: noticeError,
			Text: "Un autre import est passé entre-temps. Dépose de nouveau le fichier pour voir un aperçu à jour."}, importsData{})
	default:
		s.serverError(w, r, err)
	}
}

// livePreview shows a preview of kind again, asking for the second
// confirmation it lacked.
func (s *Server) livePreview(kind, id, username string) (importsData, error) {
	var (
		d   importsData
		err error
	)
	what := "des lignes en place"
	switch kind {
	case kindPayments:
		d.Payments.Preview, err = s.payments.Preview(id, username)
	case kindMollie:
		d.Mollie.Preview, err = s.mollie.Preview(id, username)
	default:
		d.Members.Preview, err = s.members.Preview(id, username)
		what = "des comptes de la liste actuelle"
	}
	d.errs(kind).Confirm = "Coche la seconde confirmation : ce fichier contient moins de la moitié " + what + "."
	return d, err
}

func expiredNotice() *notice {
	return &notice{Kind: noticeWarning, Text: "Cet aperçu a expiré ou a déjà servi. Dépose de nouveau le fichier si besoin."}
}

// fileRefused is the message of a refusal no rule explains.
const fileRefused = "Fichier refusé."

// refusal explains why an export of kind is refused: a stable code for the
// pushed route and the message of the imports page. refused is false for an
// internal error.
func refusal(kind imports.Kind, err error) (code, message string, refused bool) {
	var (
		membersErr  *members.ParseError
		paymentsErr *payments.ParseError
	)
	switch {
	case errors.Is(err, xlsx.ErrTooLarge):
		return "too_large", "Fichier trop volumineux une fois décompressé : 50 Mo au plus.", true
	case errors.Is(err, xlsx.ErrTooManyRows), errors.Is(err, xlsx.ErrTooManyCells):
		return "too_many_rows", "Fichier trop long : 20 000 lignes au plus.", true
	case errors.Is(err, errUnreadable):
		return "invalid_workbook", unreadableMessage(kind), true
	case errors.As(err, &membersErr):
		return string(membersErr.Kind), membersMessage(membersErr), true
	case errors.As(err, &paymentsErr) && kind == imports.Mollie:
		return string(paymentsErr.Kind), mollieMessage(paymentsErr), true
	case errors.As(err, &paymentsErr):
		return string(paymentsErr.Kind), paymentsMessage(paymentsErr), true
	}
	return "", "", false
}

func unreadableMessage(kind imports.Kind) string {
	const unreadable = "Ce fichier n'est pas un classeur Excel (.xlsx) lisible."
	switch kind {
	case imports.Members:
		return unreadable + " Dépose l'export « Télécharger » de la liste des membres."
	case imports.Payments:
		return unreadable + " Dépose l'export « Télécharger Excel » de la page des paiements."
	case imports.Mollie:
		return unreadable + " Dépose l'export « Exporter (Excel) » de la page VPayDive."
	}
	return unreadable
}

func membersMessage(pe *members.ParseError) string {
	switch pe.Kind {
	case members.ProblemNoHeader:
		return "Colonne « Email » introuvable dans les dix premières lignes. Vérifie que le fichier est bien l'export de la liste des membres."
	case members.ProblemMissingColumn:
		return "Colonne obligatoire absente : « " + pe.Column + " »."
	case members.ProblemDuplicateEmail:
		return "Une même adresse figure sur plusieurs " + rowList(pe.Rows) + ". Corrige les comptes dans VPDive, puis refais l'export."
	case members.ProblemInvalidEmail:
		return "Adresse contenant un espace, " + rowList(pe.Rows) + ". Corrige le compte dans VPDive, puis refais l'export."
	default:
		return fileRefused
	}
}

func paymentsMessage(pe *payments.ParseError) string {
	switch pe.Kind {
	case payments.ProblemNoHeader:
		return "Colonne « Créé le » introuvable dans les dix premières lignes. Vérifie que le fichier est bien l'export des paiements."
	case payments.ProblemMissingColumn:
		return "Colonne obligatoire absente : « " + pe.Column + " »."
	case payments.ProblemInvalidNumber:
		return "Montant ou quantité illisible, " + rowList(pe.Rows) + ". Vérifie ces lignes dans VPDive, puis refais l'export."
	case payments.ProblemInvalidDate:
		return "Date « Créé le » illisible, " + rowList(pe.Rows) + ". Refais l'export sans retoucher le fichier."
	case payments.ProblemEmptyProduct:
		return "La colonne « Produit/Événement » est vide sur plus de 5 % des lignes : c'est un bug connu de l'export VPDive. Change les filtres de la page des paiements, puis refais l'export."
	default:
		return fileRefused
	}
}

func mollieMessage(pe *payments.ParseError) string {
	switch pe.Kind {
	case payments.ProblemNoHeader:
		return "Colonne « Montant Panier » introuvable dans les dix premières lignes. Vérifie que le fichier est bien l'export VPayDive des encaissements Mollie."
	case payments.ProblemMissingColumn:
		return "Colonne obligatoire absente : « " + pe.Column + " »."
	case payments.ProblemInvalidNumber:
		return "Montant illisible, " + rowList(pe.Rows) + ". Vérifie ces lignes dans VPDive, puis refais l'export."
	case payments.ProblemInvalidDate:
		return "Date de paiement illisible, " + rowList(pe.Rows) + ". Refais l'export sans retoucher le fichier."
	case payments.ProblemEmptyProduct: // the VPayDive export has no such check
	}
	return fileRefused
}

// rowList formats file row numbers: "ligne 7", "lignes 12, 40", at most 20.
func rowList(rows []int) string {
	shown := rows[:min(len(rows), maxListedRows)]
	parts := make([]string, len(shown))
	for i, n := range shown {
		parts[i] = strconv.Itoa(n)
	}
	text := strings.Join(parts, ", ")
	if len(rows) > len(shown) {
		text += "…"
	}
	if len(rows) == 1 {
		return "ligne " + text
	}
	return "lignes " + text
}

// formatTime shows midnight as a date: an export cell without a time reads
// as midnight, and « à 00:00 » would invent one.
func (s *Server) formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.In(s.paris)
	if h, m, sec := t.Clock(); h == 0 && m == 0 && sec == 0 {
		return t.Format(dateFormat)
	}
	return t.Format(dateTimeFormat)
}

func (s *Server) formatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(s.paris).Format(dateFormat)
}
