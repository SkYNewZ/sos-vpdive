package web

import (
	"context"
	"errors"
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
)

// importsData is the imports page: one section per export, each with the
// errors of its own forms beside their action (spec §12.1).
type importsData struct {
	Members  membersSection
	Payments paymentsSection
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

type paymentsSection struct {
	importErrors

	Link    vpdiveLink
	Last    *imports.Info
	Purged  bool // lines removed after 90 days without an import
	Report  payments.Report
	Preview *payments.Preview
}

// errs returns the error slots of the section of kind.
func (d *importsData) errs(kind string) *importErrors {
	if kind == kindPayments {
		return &d.Payments.importErrors
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
	d.Members.Link, d.Payments.Link = s.vpdive["membres"], s.vpdive["paiements"]
	last, ok, err := s.members.LastImport(ctx)
	if err != nil {
		return err
	}
	if ok {
		d.Members.Last = &last
	}
	paid, ok, err := s.payments.LastImport(ctx)
	if err != nil || !ok {
		return err
	}
	d.Payments.Last = &paid
	inPlace, err := s.payments.HasLines(ctx)
	if err != nil {
		return err
	}
	d.Payments.Purged = !inPlace
	d.Payments.Report, err = s.payments.Report(ctx)
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
	var (
		rows    []xlsx.Row
		created time.Time // payments only: the members export dates itself in row 2
	)
	if err := telemetry.Trace(ctx, s.tracer, "import.read", func(context.Context) error {
		var err error
		rows, err = xlsx.ReadFirstSheet(data, imports.Limits())
		if err == nil && kind == kindPayments {
			created, _ = xlsx.Created(data, imports.Limits())
		}
		return err
	}); err != nil {
		s.renderImports(w, r, http.StatusUnprocessableEntity, nil, failed(kind, workbookMessage(kind, err)))
		return
	}
	sess, _ := sessionFrom(ctx)
	var d importsData
	err := telemetry.Trace(ctx, s.tracer, "import.validate", func(ctx context.Context) error {
		var err error
		d, err = s.preview(ctx, kind, sess.account.Username, rows, created)
		return err
	})
	var (
		membersErr  *members.ParseError
		paymentsErr *payments.ParseError
	)
	switch {
	case errors.As(err, &membersErr):
		s.renderImports(w, r, http.StatusUnprocessableEntity, nil, failed(kind, membersMessage(membersErr)))
	case errors.As(err, &paymentsErr):
		s.renderImports(w, r, http.StatusUnprocessableEntity, nil, failed(kind, paymentsMessage(paymentsErr)))
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.renderImports(w, r, http.StatusOK, nil, d)
	}
}

// preview parses an export of kind and keeps its preview for username.
func (s *Server) preview(ctx context.Context, kind, username string, rows []xlsx.Row, created time.Time) (importsData, error) {
	var d importsData
	if kind == kindPayments {
		exp, err := payments.Parse(rows, created, s.paris)
		if err != nil {
			return d, err
		}
		d.Payments.Preview, err = s.payments.NewPreview(ctx, username, exp)
		return d, err
	}
	exp, err := members.Parse(rows, s.paris)
	if err != nil {
		return d, err
	}
	d.Members.Preview, err = s.members.NewPreview(ctx, username, exp)
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
	if err != nil || (string(kind) != kindMembers && string(kind) != kindPayments) {
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
	default:
		s.writeText(w, r, http.StatusBadRequest, "Requête refusée : type d'import inconnu.\n")
		return
	}
	err := telemetry.Trace(r.Context(), s.tracer, "import.replace", func(ctx context.Context) error {
		return confirm(ctx, id, username, r.PostForm.Get("confirmer_moitie") == "oui")
	})
	switch {
	case err == nil:
		http.Redirect(w, r, "/imports?importe="+kind, http.StatusSeeOther)
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
	what := "des comptes de la liste actuelle"
	if kind == kindPayments {
		d.Payments.Preview, err = s.payments.Preview(id, username)
		what = "des lignes en place"
	} else {
		d.Members.Preview, err = s.members.Preview(id, username)
	}
	d.errs(kind).Confirm = "Coche la seconde confirmation : ce fichier contient moins de la moitié " + what + "."
	return d, err
}

func expiredNotice() *notice {
	return &notice{Kind: noticeWarning, Text: "Cet aperçu a expiré ou a déjà servi. Dépose de nouveau le fichier si besoin."}
}

func workbookMessage(kind string, err error) string {
	switch {
	case errors.Is(err, xlsx.ErrTooLarge):
		return "Fichier trop volumineux une fois décompressé : 50 Mo au plus."
	case errors.Is(err, xlsx.ErrTooManyRows), errors.Is(err, xlsx.ErrTooManyCells):
		return "Fichier trop long : 20 000 lignes au plus."
	case kind == kindPayments:
		return "Ce fichier n'est pas un classeur Excel (.xlsx) lisible. Dépose l'export « Télécharger Excel » de la page des paiements."
	default:
		return "Ce fichier n'est pas un classeur Excel (.xlsx) lisible. Dépose l'export « Télécharger » de la liste des membres."
	}
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
		return "Fichier refusé."
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
		return "Fichier refusé."
	}
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

func (s *Server) formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(s.paris).Format(dateTimeFormat)
}

func (s *Server) formatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(s.paris).Format(dateFormat)
}
