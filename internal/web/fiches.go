package web

import (
	"net/http"
	"slices"

	"github.com/SkYNewZ/sos-vpdive/internal/kb"
)

// ficheView is a fiche as the committee pages show it (spec §5.3).
type ficheView struct {
	kb.Fiche

	CategoryLabels []string
	Buttons        []vpdiveLink
	Deflections    int // avoided requests that showed it on screen 2
}

func (s *Server) ficheView(f kb.Fiche) ficheView {
	v := ficheView{Fiche: f}
	for _, c := range f.Categories {
		v.CategoryLabels = append(v.CategoryLabels, s.tickets.Catalog.CategoryLabel(c))
	}
	for _, key := range f.Links {
		v.Buttons = append(v.Buttons, s.vpdive[key])
	}
	return v
}

// fichesPage lists the whole knowledge base for the committee, with how many
// avoided requests showed each fiche.
func (s *Server) fichesPage(w http.ResponseWriter, r *http.Request) {
	counts, err := s.tickets.DeflectionCounts(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	views := make([]ficheView, 0, len(s.kb.Fiches))
	for _, f := range s.kb.Fiches {
		v := s.ficheView(f)
		v.Deflections = counts[f.ID]
		views = append(views, v)
	}
	p, err := s.adminPage(r, "Fiches")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = views
	s.render(w, r, http.StatusOK, "fiches", p)
}

// suggestedFiches returns the fiches chosen at submission, and the ids of
// those removed from kb/ since (spec §9.7).
func (s *Server) suggestedFiches(ids []string) (found []ficheView, removed []string) {
	for _, id := range ids {
		if f, ok := s.kb.Get(id); ok {
			found = append(found, s.ficheView(f))
		} else {
			removed = append(removed, id)
		}
	}
	return found, removed
}

// ticketButtons orders the VPDive buttons of a request: the links its fiches
// declare first (spec §7), then the usual ones, without duplicates.
func ticketButtons(fiches []ficheView) []string {
	var keys []string
	for _, f := range fiches {
		for _, k := range f.Links {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	for _, k := range ticketLinks {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}
