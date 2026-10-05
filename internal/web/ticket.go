package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// ticketLinks are the VPDive buttons of a request page, in this order
// (spec §7). The faq and tarifs links belong to the member form.
var ticketLinks = []string{"paiements", "membres", "messagerie", "support"}

// replyActions send a reply to the member, alone or with a status change.
var replyActions = []tickets.Action{tickets.ActionReply, tickets.ActionReplyWait, tickets.ActionReplyClose}

// ticketMessage is what an action leaves on a page shown again: a notice,
// and the text typed in the reply or note field with its problem.
type ticketMessage struct {
	Notice                *notice
	Reply, Note           string
	ReplyError, NoteError string
	stale                 tickets.Action // set when the action met ErrStale
}

type ticketData struct {
	ticketMessage

	Ticket     *tickets.Detail
	Assignee   *admins.Account
	Accounts   []admins.Account
	Profile    *profileView // nil when the address left the members list
	Others     []tickets.Row
	Fields     []tickets.FieldValue
	Categories []option
	Links      []vpdiveLink
	Filter     paymentsFilter
}

// profileView is the requester as the last members import knows them.
type profileView struct {
	Seasons string
	Licence string
}

// paymentsFilter is what to type in VPDive's payments filter (spec §7): the
// filter is a POST form there, it cannot be prefilled from another site.
type paymentsFilter struct {
	Name    string
	Product string
	From    string
	To      string
}

func (s *Server) ticketPage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	s.renderTicket(w, r, http.StatusOK, id, ticketMessage{})
}

// renderTicket shows a request as it stands now, with what an action left.
func (s *Server) renderTicket(w http.ResponseWriter, r *http.Request, status int, id int64, m ticketMessage) {
	ctx := r.Context()
	t, err := s.tickets.Detail(ctx, id)
	if errors.Is(err, tickets.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data, err := s.ticketView(ctx, t)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if m.stale != "" {
		m.Notice = s.staleNotice(m.stale, t, m)
	}
	data.ticketMessage = m
	p, err := s.adminPage(r, "Demande "+t.Ref)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if m.Notice != nil {
		p.Notices = append(p.Notices, *m.Notice)
	}
	p.Data = data
	s.render(w, r, status, "ticket", p)
}

func (s *Server) ticketView(ctx context.Context, t *tickets.Detail) (ticketData, error) {
	others, err := s.tickets.Others(ctx, t.ID)
	if err != nil {
		return ticketData{}, err
	}
	profile, found, err := s.members.Find(ctx, t.Email)
	if err != nil {
		return ticketData{}, err
	}
	d := ticketData{
		Ticket: t, Assignee: s.accountOf(t.Assignee), Accounts: s.admins.Accounts(), Others: others,
		Fields: s.catalog.Display(t.Fields), Categories: s.categoryOptions(t.Category),
	}
	for _, key := range ticketLinks {
		if l, ok := s.vpdive[key]; ok {
			d.Links = append(d.Links, l)
		}
	}
	name := t.FirstName + " " + t.LastName
	if found {
		d.Profile = newProfileView(profile)
		name = profile.FirstName + " " + profile.LastName
	}
	today := s.now().In(s.paris)
	d.Filter = paymentsFilter{
		Name: name, Product: s.catalog.Product(t.Fields),
		From: time.Date(today.Year(), time.January, 1, 0, 0, 0, 0, s.paris).Format(dateFormat),
		To:   today.Format(dateFormat),
	}
	return d, nil
}

func newProfileView(p members.Profile) *profileView {
	v := &profileView{Seasons: "non fournies par l'export", Licence: "non renseignée"}
	if p.Seasons != nil {
		v.Seasons = *p.Seasons
		if v.Seasons == "" {
			v.Seasons = "aucune"
		}
	}
	if p.LicenceExpires != "" {
		v.Licence = isoDate(p.LicenceExpires)
	}
	return v
}

// staleNotice explains why an action from an outdated page was refused.
func (s *Server) staleNotice(action tickets.Action, t *tickets.Detail, m ticketMessage) *notice {
	if action == tickets.ActionTake && t.Assignee != "" {
		return &notice{Kind: noticeWarning, Text: s.tickets.AccountName(t.Assignee) + " a pris cette demande entre-temps. Ton action n'a pas été appliquée."}
	}
	text := "Cette demande a changé entre-temps : voici son état actuel. Ton action n'a pas été appliquée."
	switch {
	case m.Reply == "" && m.Note == "":
	case t.Status.Open() && t.Status != tickets.StatusTodo:
		text += " Ton texte est conservé."
	default:
		text += " Ton texte n'a pas été envoyé : copie-le ci-dessous."
	}
	return &notice{Kind: noticeWarning, Text: text}
}

// ticketAction runs one committee action (spec §8.1). The page's version
// travels with it: an action from an outdated page is refused whole.
func (s *Server) ticketAction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	if !s.postForm(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	cmd, m := readCommand(id, sess.account.Username, r.PostForm)
	if m.ReplyError != "" || m.NoteError != "" {
		s.renderTicket(w, r, http.StatusUnprocessableEntity, id, m)
		return
	}
	err := s.tickets.Apply(r.Context(), cmd)
	switch {
	case err == nil && cmd.Action == tickets.ActionDelete:
		http.Redirect(w, r, "/?supprimee=1", http.StatusSeeOther)
	case err == nil:
		http.Redirect(w, r, "/demandes/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
	case errors.Is(err, tickets.ErrStale):
		m.stale = cmd.Action
		s.renderTicket(w, r, http.StatusConflict, id, m)
	case errors.Is(err, tickets.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, tickets.ErrNotAllowed), errors.Is(err, tickets.ErrInvalid):
		m.Notice = &notice{Kind: noticeError, Text: "Cette action n'est pas possible dans l'état actuel de la demande."}
		s.renderTicket(w, r, http.StatusUnprocessableEntity, id, m)
	default:
		s.serverError(w, r, err)
	}
}

// readCommand turns the posted form into a command. A reply or a note that
// is empty or too long comes back with its problem instead.
func readCommand(id int64, actor string, v url.Values) (tickets.Command, ticketMessage) {
	cmd := tickets.Command{
		Action: tickets.Action(v.Get("action")), TicketID: id, Version: formInt(v, "version"), Actor: actor,
		Assignee: v.Get("assignee"), Category: v.Get("categorie"),
		AttachmentID: formInt(v, "capture"), MessageID: formInt(v, "message_id"),
	}
	m := ticketMessage{Reply: v.Get("message"), Note: v.Get("note")}
	var ok bool
	switch {
	case slices.Contains(replyActions, cmd.Action):
		if cmd.Body, ok = tickets.CleanText(m.Reply, 1, tickets.MessageMax); !ok {
			m.ReplyError = "Écris ta réponse : 4 000 caractères au plus."
		}
	case cmd.Action == tickets.ActionNote:
		if cmd.Body, ok = tickets.CleanText(m.Note, 1, tickets.MessageMax); !ok {
			m.NoteError = "Écris ta note : 4 000 caractères au plus."
		}
	}
	return cmd, m
}

func (s *Server) adminCapture(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(r, "id")
	capture, okCapture := pathID(r, "cid")
	if !okID || !okCapture {
		s.notFound(w, r)
		return
	}
	data, mime, err := s.tickets.Capture(r.Context(), id, capture)
	s.writeCapture(w, r, data, mime, err)
}
