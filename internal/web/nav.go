package web

import (
	"net/http"
	"strings"
)

// navItem is an entry of the committee navigation: the desktop sidebar
// lists them all but « Plus », the phone tab bar those with Tab set (spec
// §12.2 as amended: icons in the navigation only, with their label).
type navItem struct {
	Section   string // first path segment of its pages
	Path      string
	Label     string
	Icon      string // symbol of the sprite in templates/layout.html
	Tab       bool
	PhoneOnly bool // « Plus » gathers on phones what the sidebar shows
	OwnerOnly bool // « Comptes »: OWNER_USERNAME only
	Assistant bool // shown only when the assistant is on
}

// importsPath is the imports page, linked from the navigation and from the
// banners about stale or missing exports.
const importsPath = "/imports"

var adminNav = []navItem{
	{Section: "demandes", Path: "/", Label: "Demandes", Icon: "inbox", Tab: true},
	{Section: "anomalies", Path: "/anomalies", Label: "À vérifier", Icon: "list-checks", Tab: true},
	{Section: "annulations", Path: "/annulations", Label: "Annulations", Icon: "calendar-x", Tab: true},
	{Section: "calendrier", Path: "/calendrier", Label: "Calendrier", Icon: "calendar-days"},
	{Section: "assistant", Path: "/assistant", Label: "Assistant", Icon: "sparkles", Assistant: true},
	{Section: "fiches", Path: "/fiches", Label: "Fiches", Icon: "book-open", Tab: true},
	{Section: "imports", Path: importsPath, Label: "Imports", Icon: "upload"},
	{Section: "effacement", Path: "/effacement", Label: "Effacement", Icon: "user-x"},
	{Section: "notifications", Path: "/notifications", Label: "Notifications", Icon: "bell"},
	{Section: "comptes", Path: accountsPath, Label: "Comptes", Icon: "users", OwnerOnly: true},
	{Section: "plus", Path: "/plus", Label: "Plus", Icon: "ellipsis", Tab: true, PhoneOnly: true},
}

// navFor is the navigation of a committee page: « Comptes » for the owner
// only, « Assistant » when the assistant is on.
func navFor(owner, assistant bool) []navItem {
	out := make([]navItem, 0, len(adminNav))
	for _, it := range adminNav {
		if (!it.OwnerOnly || owner) && (!it.Assistant || assistant) {
			out = append(out, it)
		}
	}
	return out
}

// navSection tells which sidebar entry and which phone tab a committee path
// belongs to. Pages without a tab of their own, /plus included, light up
// « Plus ».
func navSection(path string) (section, tab string) {
	section, _, _ = strings.Cut(strings.TrimPrefix(path, "/"), "/")
	switch section {
	case "":
		section = "demandes"
	case "push":
		section = "notifications"
	}
	for _, it := range adminNav {
		if it.Section == section && it.Tab {
			return section, section
		}
	}
	return section, "plus"
}

// plusPage is the phone tab bar's « Plus »: the pages without a tab, the
// account and the logout.
func (s *Server) plusPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.adminPage(r, "Plus")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "plus", p)
}
