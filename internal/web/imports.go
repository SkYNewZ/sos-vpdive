package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/members"
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

// importsData embeds the message: Upload and Confirm errors sit next to their
// form's action (spec §12.1).
type importsData struct {
	importMessage

	MembersLink vpdiveLink
	Last        *members.ImportInfo
	Preview     *members.Preview
}

// importMessage is what a page render reports: a page-level notice, or an
// error shown beside the upload or confirmation action.
type importMessage struct {
	Notice          *notice
	Upload, Confirm string
}

func (s *Server) importsPage(w http.ResponseWriter, r *http.Request) {
	var done *notice
	if r.URL.Query().Get("importe") == "1" {
		done = &notice{Kind: noticeSuccess, Text: "Liste des membres importée."}
	}
	s.renderImports(w, r, http.StatusOK, nil, importMessage{Notice: done})
}

func (s *Server) renderImports(w http.ResponseWriter, r *http.Request, status int, preview *members.Preview, m importMessage) {
	p, err := s.adminPage(r, "Imports")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if m.Notice != nil {
		p.Notices = append(p.Notices, *m.Notice)
	}
	data := importsData{importMessage: m, MembersLink: s.vpdive["membres"], Preview: preview}
	last, ok, err := s.members.LastImport(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if ok {
		data.Last = &last
	}
	p.Data = data
	s.render(w, r, status, "imports", p)
}

// uploadImport reads, validates and previews a members export. Nothing
// changes before the confirmation.
func (s *Server) uploadImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+multipartSlack)
	data, ok := s.readUpload(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var rows []xlsx.Row
	if err := telemetry.Trace(ctx, s.tracer, "import.read", func(context.Context) error {
		var err error
		rows, err = xlsx.ReadFirstSheet(data, members.ImportLimits())
		return err
	}); err != nil {
		s.renderImports(w, r, http.StatusUnprocessableEntity, nil, importMessage{Upload: workbookMessage(err)})
		return
	}
	sess, _ := sessionFrom(ctx)
	var preview *members.Preview
	var parseErr *members.ParseError
	err := telemetry.Trace(ctx, s.tracer, "import.validate", func(ctx context.Context) error {
		exp, err := members.Parse(rows, s.paris)
		if err != nil {
			return err
		}
		preview, err = s.members.NewPreview(ctx, sess.account.Username, exp)
		return err
	})
	switch {
	case errors.As(err, &parseErr):
		s.renderImports(w, r, http.StatusUnprocessableEntity, nil, importMessage{Upload: parseMessage(parseErr)})
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.renderImports(w, r, http.StatusOK, preview, importMessage{})
	}
}

// readUpload reads the multipart body in memory: the CSRF field first, then
// the file. ParseMultipartForm is not used because it spills large files to
// disk, and the export must never touch the disk (spec §7.2).
func (s *Server) readUpload(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	mr, err := r.MultipartReader()
	if err != nil {
		s.writeText(w, r, http.StatusBadRequest, "Requête refusée : envoi de fichier attendu.\n")
		return nil, false
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "csrf" {
		s.forbidCSRF(w, r)
		return nil, false
	}
	token, err := io.ReadAll(io.LimitReader(part, maxFieldBytes))
	if err != nil || !s.csrfValid(r, string(token)) {
		s.forbidCSRF(w, r)
		return nil, false
	}
	missing := importMessage{Upload: "Choisis le fichier exporté depuis VPDive avant d'envoyer."}
	part, err = mr.NextPart()
	if err != nil || part.FormName() != "file" {
		s.renderImports(w, r, http.StatusUnprocessableEntity, nil, missing)
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(part, maxUploadBytes+1))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig) || len(data) > maxUploadBytes:
		s.renderImports(w, r, http.StatusRequestEntityTooLarge, nil,
			importMessage{Upload: "Fichier trop volumineux : 5 Mo au plus."})
		return nil, false
	case err != nil:
		s.writeText(w, r, http.StatusBadRequest, "Envoi interrompu. Réessaie.\n")
		return nil, false
	case len(data) == 0:
		s.renderImports(w, r, http.StatusUnprocessableEntity, nil, missing)
		return nil, false
	}
	return data, true
}

func (s *Server) confirmImport(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	username := sess.account.Username
	id := r.PostForm.Get("apercu")
	err := telemetry.Trace(r.Context(), s.tracer, "import.replace", func(ctx context.Context) error {
		return s.members.Confirm(ctx, id, username, r.PostForm.Get("confirmer_moitie") == "oui")
	})
	switch {
	case err == nil:
		http.Redirect(w, r, "/imports?importe=1", http.StatusSeeOther)
	case errors.Is(err, members.ErrSecondConfirmRequired):
		preview, perr := s.members.Preview(id, username)
		if perr != nil {
			s.renderImports(w, r, http.StatusConflict, nil, importMessage{Notice: expiredNotice()})
			return
		}
		s.renderImports(w, r, http.StatusUnprocessableEntity, preview, importMessage{
			Confirm: "Coche la seconde confirmation : ce fichier contient moins de la moitié des comptes de la liste actuelle."})
	case errors.Is(err, members.ErrPreviewNotFound):
		s.renderImports(w, r, http.StatusConflict, nil, importMessage{Notice: expiredNotice()})
	case errors.Is(err, members.ErrStale):
		s.renderImports(w, r, http.StatusConflict, nil, importMessage{Notice: &notice{Kind: noticeError,
			Text: "Un autre import est passé entre-temps. Dépose de nouveau le fichier pour voir un aperçu à jour."}})
	default:
		s.serverError(w, r, err)
	}
}

func expiredNotice() *notice {
	return &notice{Kind: noticeWarning, Text: "Cet aperçu a expiré ou a déjà servi. Dépose de nouveau le fichier si besoin."}
}

func workbookMessage(err error) string {
	switch {
	case errors.Is(err, xlsx.ErrTooLarge):
		return "Fichier trop volumineux une fois décompressé : 50 Mo au plus."
	case errors.Is(err, xlsx.ErrTooManyRows), errors.Is(err, xlsx.ErrTooManyCells):
		return "Fichier trop long : 20 000 lignes au plus."
	default:
		return "Ce fichier n'est pas un classeur Excel (.xlsx) lisible. Dépose l'export « Télécharger » de la liste des membres."
	}
}

func parseMessage(pe *members.ParseError) string {
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
	return t.In(s.paris).Format(dateFormat)
}
