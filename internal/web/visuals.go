package web

import (
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/campaign"
	"github.com/SkYNewZ/sos-vpdive/internal/carnets"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/visual"
)

// ticketVisuals only projects blocks that already passed their identity gates.
// Detailed records remain below each summary; no new member lookup is needed.
type ticketVisuals struct {
	Outings, Payments, Mollie, Campaign template.HTML
	Cards                               []cardVisual
}

type cardVisual struct {
	Summary template.HTML
	Card    carnets.View
}

func visualHTML(kind string, b visual.Block) template.HTML {
	h, _ := visual.Render(kind, b)
	return h
}

func (s *Server) visualSource(label string, info imports.Info) string {
	text := label + " · reçu le " + s.formatTime(info.ImportedAt)
	if s.stateOf(info, true).Perime {
		text += " · périmé"
	}
	return text
}

func (s *Server) ticketVisuals(d ticketData) ticketVisuals {
	v := ticketVisuals{Outings: s.outingsVisual(d.Outings)}
	if b := d.Payments; b.State == payments.BlockLines {
		partial := 0
		for _, l := range b.ToSettle {
			if l.Partial() && l.Dismissal == nil {
				partial++
			}
		}
		summary := visual.Block{
			Title: "Situation des paiements", Source: s.visualSource("VPDive", b.Import),
			Note: "Les avoirs ne sont pas des dettes. Une ligne à régler n'est pas nécessairement une anomalie.",
			Metrics: []visual.Metric{
				{Label: "Avoirs et soldes", Value: strconv.Itoa(len(b.Balances))},
				{Label: "Lignes à régler", Value: strconv.Itoa(len(b.ToSettle))},
				{Label: "Paiements partiels à vérifier", Value: strconv.Itoa(partial), State: countState(partial)},
				{Label: "Annulations payées", Value: strconv.Itoa(len(b.Cancelled)), State: countState(len(b.Cancelled))},
			},
		}
		for _, balance := range b.Balances[:min(4, len(b.Balances))] {
			summary.Metrics = append(summary.Metrics, visual.Metric{Label: "Avoir · " + balance.Product, Value: balance.UnitPrice.Abs().Euros()})
		}
		if len(b.Balances) > 4 {
			summary.Note += " Quatre avoirs affichés ici ; les suivants sont dans le détail."
		}
		v.Payments = visualHTML("paiements", summary)
	}
	if b := d.Mollie; b.State == payments.BlockLines {
		unchecked := 0
		for _, p := range b.Payments {
			for _, l := range p.Lines {
				if l.Unsettled() && l.Dismissal == nil {
					unchecked++
				}
			}
		}
		v.Mollie = visualHTML("paiements", visual.Block{
			Title: "Rapprochement Mollie / VPDive", Source: s.visualSource("VPayDive", b.Import),
			Note: "Encaissements présents dans l'export uniquement. Les tentatives échouées ou expirées n'y figurent pas.",
			Metrics: []visual.Metric{
				{Label: "Encaissements", Value: strconv.Itoa(len(b.Payments))},
				{Label: "Lignes non soldées à vérifier", Value: strconv.Itoa(unchecked), State: countState(unchecked)},
			},
		})
	}
	if b := d.Carnets; b.State == payments.BlockLines {
		for _, c := range b.Cards {
			label, value := "Avoir restant", c.Amount.Abs().Euros()
			state := ""
			if c.Amount > 0 {
				label, value, state = "Reste à payer", c.Amount.Euros(), string(noticeWarning)
			}
			net := c.Net().Euros()
			if c.Partial() {
				net = "Non calculé"
			}
			b := visual.Block{Title: c.Title, Source: s.visualSource("Cartes VPDive", b.Import), Partial: c.Partial(),
				Metrics: []visual.Metric{
					{Label: label, Value: value, State: state},
					{Label: "Débits lus", Value: strconv.Itoa(c.Debits)},
					{Label: "Recrédits lus", Value: strconv.Itoa(c.Recredits)},
					{Label: "Débit net lu", Value: net},
				},
			}
			if c.Unusual > 0 {
				b.Note = "Montant inhabituel : à vérifier dans l'historique, sans en déduire la cause."
			}
			v.Cards = append(v.Cards, cardVisual{Summary: visualHTML("cartes", b), Card: c})
		}
	}
	if b := d.Campaign; b.State == payments.BlockLines && len(b.People) == 1 {
		p := b.People[0]
		note := "Le statut de la campagne et la validité de la licence sont deux informations distinctes."
		if b.Typed {
			note = "Rapproché par le nom tapé, à confirmer. " + note
		}
		v.Campaign = visualHTML("adhesion", visual.Block{
			Title: "Situation de l'adhésion", Source: s.visualSource("Campagne", b.Import), Note: note,
			Metrics: []visual.Metric{
				{Label: "Dossier", Value: p.Status},
				campaignMetric("Adhésion", p.Membership), campaignMetric("Licence", p.Licence),
			},
		})
	}
	return v
}

func countState(n int) string {
	if n > 0 {
		return string(noticeWarning)
	}
	return ""
}

func campaignMetric(label string, entries []campaignEntry) visual.Metric {
	m := visual.Metric{Label: label, Value: "Aucune ligne"}
	if len(entries) == 0 {
		return m
	}
	values := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Unreadable() {
			m.Value, m.State = "À vérifier", string(noticeWarning)
			return m
		}
		values = append(values, e.StateText+" · "+e.Amount.Euros())
		if e.State != campaign.StatePaid || e.OddAmount {
			m.State = string(noticeWarning)
		}
	}
	m.Value = strings.Join(values, " ; ")
	return m
}

func (s *Server) outingsVisual(b outingsBlock) template.HTML {
	if b.State != outingsList {
		return ""
	}
	now := s.now().In(s.paris)
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, s.paris).AddDate(0, -11, 0)
	months := make([]visual.Month, 12)
	for i := range months {
		d := first.AddDate(0, i, 0)
		months[i].Label = frMonthsShort[d.Month()-1] + " " + d.Format("06")
	}
	past, upcoming, cancelled, left := 0, 0, 0, 0
	registrations := map[string]bool{}
	cancellations := map[string]bool{}
	unregistrations := map[string]bool{}
	var next *outing
	for _, group := range [][]outing{b.Recent, b.Older} {
		for _, o := range group {
			p := o.Participant
			if o.Event.Start.Before(first) {
				continue
			}
			if len(o.Unregistrations) > 0 && !unregistrations[o.Event.ID] {
				left++
				unregistrations[o.Event.ID] = true
			}
			if o.Event.Cancelled() {
				if !cancellations[o.Event.ID] {
					cancelled++
					cancellations[o.Event.ID] = true
				}
				continue
			}
			if p == nil || !p.Registered || p.WaitingList || registrations[o.Event.ID] {
				continue
			}
			registrations[o.Event.ID] = true
			if o.Event.Start.After(now) {
				upcoming++
				if next == nil || o.Event.Start.Before(next.Event.Start) {
					next = &o
				}
				continue
			}
			past++
			d := o.Event.Start.In(s.paris)
			i := (d.Year()-first.Year())*12 + int(d.Month()-first.Month())
			months[i].Count++
		}
	}
	v := visual.Block{Title: "Inscriptions au fil des mois", Source: "Calendrier · reçu le " + s.formatTime(b.Received),
		Period: "Du " + s.formatDate(first) + " au " + s.formatDate(now), Months: months,
		Note: "Barres : inscriptions passées connues, pas des plongées réalisées. Zéro = aucune inscription connue. Annulations et listes d'attente exclues ; les compteurs à venir couvrent toutes les dates connues.",
		Metrics: []visual.Metric{
			{Label: "Inscriptions passées", Value: strconv.Itoa(past)},
			{Label: "Inscriptions à venir connues", Value: strconv.Itoa(upcoming)},
			{Label: "Sorties annulées connues", Value: strconv.Itoa(cancelled)},
			{Label: "Sorties avec désinscription", Value: strconv.Itoa(left)},
		},
	}
	if next != nil {
		v.Note += " Prochaine inscription : " + eventWhen(next.Event, s.paris) + " · " + next.Event.Title + "."
	}
	v.Headers = []string{"Date", "Sortie", "Participation", "Panier"}
	for _, group := range [][]outing{b.Recent, b.Older} {
		for _, o := range group {
			if len(v.Rows) == 5 {
				break
			}
			state, cart := "Non inscrit", "—"
			if p := o.Participant; p != nil {
				cart = cartText(p.Payment)
				if p.Registered {
					state = "Inscrit"
					if p.WaitingList {
						state = "Liste d'attente"
					}
				}
			}
			if state == "Non inscrit" && len(o.Unregistrations) > 0 {
				state = "Désinscrit"
			}
			if o.Event.Cancelled() {
				state = "Annulée"
			}
			v.Rows = append(v.Rows, []string{eventWhen(o.Event, s.paris), o.Event.Title, state, cart})
		}
	}
	v.Note += " Tableau : aperçu de cinq sorties au plus, toutes dates. Participations, invités, rôles et paiements complets ci-dessous."
	return visualHTML("sorties", v)
}
