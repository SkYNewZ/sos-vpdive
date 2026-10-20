package web

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/SkYNewZ/sos-vpdive/internal/campaign"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// membershipPrice is the club's membership; any other amount gets a badge.
const membershipPrice payments.Amount = 5000

// membershipsData is the page « Suivi des adhésions » (design 2026-10-20).
type membershipsData struct {
	Imported, Purged bool
	Import           imports.Info
	Link             vpdiveLink
	Sections         [3]peopleSection // to validate, validated, incomplete
	Invited          int              // neither membership nor licence
}

// peopleSection is one list of the page, sorted by last then first name.
type peopleSection struct {
	ID, Title, Intro string
	People           []campaignRow
}

// campaignRow is one person of the campaign as the page shows them.
type campaignRow struct {
	Name                string
	Membership, Licence []campaignEntry
	Validation          string // validated: when and by whom; incomplete: what is missing
	Homonym             bool   // ambiguous: never attributed to a member
	Unpaid              bool   // validated with the membership or the licence unpaid
}

// campaignEntry is one entry of an « Adhésion » or « License » cell.
type campaignEntry struct {
	campaign.Entry

	StateText string // « Réglé », « Au panier », « état inconnu : … »
	PaidOn    string
	OddAmount bool // a membership other than membershipPrice
}

func (s *Server) membershipsPage(w http.ResponseWriter, r *http.Request) {
	o, err := s.campaign.Overview(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := membershipsData{Imported: o.Imported, Purged: o.Purged, Import: o.Import, Link: s.vpdive["campagne"],
		Sections: [3]peopleSection{
			{ID: "a-valider", Title: "À valider", Intro: "Adhésion et licence réglées, dossier pas encore validé dans VPDive."},
			{ID: "valides", Title: "Validés"},
			{ID: "incomplets", Title: "Incomplets", Intro: "Une seule des deux réglée, ou au panier sans paiement."},
		}}
	people := slices.Clone(o.People)
	slices.SortStableFunc(people, func(a, b payments.Marked[campaign.Person]) int {
		return cmp.Or(
			cmp.Compare(secure.NormalizeName(a.Line.LastName), secure.NormalizeName(b.Line.LastName)),
			cmp.Compare(secure.NormalizeName(a.Line.FirstName), secure.NormalizeName(b.Line.FirstName)))
	})
	for _, m := range people {
		p, v := m.Line, s.campaignRow(m)
		membership, licence := paid(p.Membership), paid(p.Licence)
		var i int
		switch {
		case p.Status == campaign.StatusValidated:
			i, v.Validation, v.Unpaid = 1, s.validation(p), !membership || !licence
		case membership && licence && p.Status == campaign.StatusPending:
			i, v.Validation = 0, "En attente de validation"
		case len(p.Membership)+len(p.Licence) > 0:
			i, v.Validation = 2, missing(membership, licence, p.Status)
		default:
			d.Invited++
			continue
		}
		d.Sections[i].People = append(d.Sections[i].People, v)
	}
	page, err := s.adminPage(r, "Suivi des adhésions")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	page.Data = d
	s.render(w, r, http.StatusOK, "adhesions", page)
}

func (s *Server) campaignRow(m payments.Marked[campaign.Person]) campaignRow {
	p := m.Line
	v := campaignRow{Name: strings.TrimSpace(p.LastName + " " + p.FirstName), Homonym: m.Ambiguous}
	view := func(entries []campaign.Entry, isMembership bool) []campaignEntry {
		out := make([]campaignEntry, len(entries))
		for i, e := range entries {
			out[i] = campaignEntry{Entry: e, PaidOn: s.formatDate(e.PaidOn), StateText: stateText(e.State),
				OddAmount: isMembership && !e.Unreadable() && e.Amount != membershipPrice}
		}
		return out
	}
	v.Membership, v.Licence = view(p.Membership, true), view(p.Licence, false)
	return v
}

// validation is « Validé le 14/10/2026 à 18:32 par Prénom Nom », as much
// of it as the export gave.
func (s *Server) validation(p campaign.Person) string {
	switch {
	case p.ValidationUnreadable:
		return "Validé, validation illisible"
	case p.ValidatedAt.IsZero():
		return "Validé"
	case p.ValidatedBy == "": // blanked by an erasure
		return "Validé le " + s.formatTime(p.ValidatedAt)
	}
	return "Validé le " + s.formatTime(p.ValidatedAt) + " par " + p.ValidatedBy
}

// paid tells whether one of the entries is paid.
func paid(entries []campaign.Entry) bool {
	return slices.ContainsFunc(entries, func(e campaign.Entry) bool { return e.State == campaign.StatePaid })
}

// missing says what keeps an incomplete person off the list to validate.
func missing(membership, licence bool, status string) string {
	switch {
	case !membership && !licence:
		return "Il manque l'adhésion et la licence."
	case !membership:
		return "Il manque l'adhésion."
	case !licence:
		return "Il manque la licence."
	}
	return "Statut VPDive inattendu : « " + status + " »."
}

func stateText(state string) string {
	switch state {
	case campaign.StatePaid, "":
		return state
	case campaign.StateInCart:
		return "Au panier"
	}
	return "état inconnu : " + state
}

// campaignBlock is the « Campagne d'adhésion » block of a request page
// (design 2026-10-20). A requester outside the members list is looked up by
// the name typed on the form, committee side only: never for the mail, the
// member or the payments (a departure from §7.3).
type campaignBlock struct {
	State  payments.BlockState
	Import imports.Info
	Typed  bool // looked up by the typed name: a line found is to confirm
	People []campaignPerson
}

// campaignPerson is a campaign line as the block shows it.
type campaignPerson struct {
	campaignRow

	Status, Settlement string
}

// campaignBlock finds the campaign lines of the requester: through the
// members list (nameHash), else through the typed name in either order.
func (s *Server) campaignBlock(ctx context.Context, nameHash []byte, typedLast, typedFirst string) (campaignBlock, error) {
	var (
		b      campaignBlock
		people []campaign.Person
		err    error
	)
	b.State, b.Import, people, err = s.campaign.NameLines(ctx, nameHash)
	if err == nil && b.State == payments.BlockNoMember {
		b.Typed, b.State = true, payments.BlockEmpty
		for _, key := range typedKeys(typedLast, typedFirst) {
			if b.State, b.Import, people, err = s.campaign.NameLines(ctx, s.keys.Hash(key)); err != nil || b.State != payments.BlockEmpty {
				break
			}
		}
	}
	if err != nil {
		return campaignBlock{}, err
	}
	for _, p := range people {
		v := campaignPerson{campaignRow: s.campaignRow(payments.Marked[campaign.Person]{Line: p}), Status: p.Status, Settlement: p.Settlement}
		if p.Status == campaign.StatusValidated {
			v.Validation = s.validation(p)
		}
		b.People = append(b.People, v)
	}
	return b, nil
}

// typedKeys are the name keys of a typed name in both orders; none when a
// part is empty: never on half a name.
func typedKeys(last, first string) []string {
	if secure.NormalizeName(last) == "" || secure.NormalizeName(first) == "" {
		return nil
	}
	keys := []string{secure.NameKey(last, first)}
	if swapped := secure.NameKey(first, last); swapped != keys[0] {
		keys = append(keys, swapped)
	}
	return keys
}
