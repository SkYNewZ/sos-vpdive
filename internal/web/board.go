package web

import (
	"net/http"
	"net/url"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// Board filter values (spec §4.2). The default view shows the requests to
// handle and in progress.
const (
	filterAllOpen = "ouvertes"
	filterMine    = "moi"
)

// option is one choice of a select.
type option struct {
	Value string
	Label string
}

type boardQuery struct {
	Status   string
	Assignee string
	Category string
}

type boardData struct {
	Rows       []tickets.Row
	Query      boardQuery
	Statuses   []option
	Accounts   []admins.Account
	Categories []option
	Nobody     string // filter value of « personne »
}

// boardStatuses are the choices of the status filter, default first.
var boardStatuses = []option{
	{"", "À traiter et en cours"},
	{string(tickets.StatusTodo), tickets.StatusTodo.Label()},
	{string(tickets.StatusInProgress), tickets.StatusInProgress.Label()},
	{string(tickets.StatusWaiting), tickets.StatusWaiting.Label()},
	{filterAllOpen, "Toutes les demandes ouvertes"},
	{string(tickets.StatusDone), tickets.StatusDone.Label()},
}

// boardFilter reads the filter form; an unknown status falls back to the
// default view.
func boardFilter(q url.Values, me string) (tickets.Filter, boardQuery) {
	bq := boardQuery{Status: q.Get("statut"), Assignee: q.Get("resolveur"), Category: q.Get("categorie")}
	f := tickets.Filter{Assignee: bq.Assignee, Category: bq.Category}
	switch bq.Status {
	case filterAllOpen:
		f.Statuses = []tickets.Status{tickets.StatusTodo, tickets.StatusInProgress, tickets.StatusWaiting}
	case string(tickets.StatusTodo), string(tickets.StatusInProgress), string(tickets.StatusWaiting), string(tickets.StatusDone):
		f.Statuses = []tickets.Status{tickets.Status(bq.Status)}
	default:
		bq.Status = ""
	}
	if bq.Assignee == filterMine {
		f.Assignee = me
	}
	return f, bq
}

// board is the committee's start page: the requests, oldest first.
func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	q := r.URL.Query()
	f, bq := boardFilter(q, sess.account.Username)
	rows, err := s.tickets.Board(r.Context(), f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p, err := s.adminPage(r, "Demandes")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if q.Get("supprimee") == "1" {
		p.Notices = append(p.Notices, notice{Kind: noticeSuccess, Text: "Demande supprimée."})
	}
	p.Data = boardData{
		Rows: rows, Query: bq, Statuses: boardStatuses, Accounts: s.admins.Accounts(),
		Categories: s.categoryOptions(bq.Category), Nobody: tickets.AssigneeNobody,
	}
	s.render(w, r, http.StatusOK, "admin_home", p)
}

// categoryOptions lists every category, committee-only ones included. A
// current value that left the catalog stays selectable, labelled « (retiré) ».
func (s *Server) categoryOptions(current string) []option {
	opts := make([]option, 0, len(s.catalog.Categories)+1)
	found := current == ""
	for _, c := range s.catalog.Categories {
		opts = append(opts, option{c.ID, c.Label})
		found = found || c.ID == current
	}
	if !found {
		opts = append(opts, option{current, s.catalog.CategoryLabel(current)})
	}
	return opts
}
