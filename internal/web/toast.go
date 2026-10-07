package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// toastTitles name the changes a desktop toast reports (spec §4.2 as amended
// by the owner). A deleted request has nothing left to show.
var toastTitles = map[tickets.ChangeType]string{
	tickets.ChangeCreated: "Nouvelle demande",
	tickets.ChangeReplied: "Réponse de l'adhérent",
	tickets.ChangeUpdated: "Demande mise à jour",
}

// toastData feeds templates/toast.html.
type toastData struct {
	Title string
	Kind  string // the change type
	Row   tickets.Row
}

// toast renders the notice of a change made by someone else, as an HTML
// fragment fetched under the session: the event stream itself never
// carries a name.
func (s *Server) toast(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	kind := tickets.ChangeType(r.URL.Query().Get("type"))
	title, ok := toastTitles[kind]
	if err != nil || !ok {
		s.notFound(w, r)
		return
	}
	row, err := s.tickets.Row(r.Context(), id)
	if errors.Is(err, tickets.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderFragment(w, r, "toast", toastData{Title: title, Kind: string(kind), Row: row})
}
