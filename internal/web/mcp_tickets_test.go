package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

func TestMCPListTicketsPaginates(t *testing.T) {
	e, base := newMCPEnv(t)
	first := e.submitTicket(t, "lea.martin@example.org")
	e.clock.advance(24 * time.Hour)
	second := e.submitTicket(t, "hugo.bernard@example.org")
	e.clock.advance(24 * time.Hour)
	third := e.submitTicket(t, "hugo.bernard@example.org", func(s *tickets.Submission) { s.FirstName, s.LastName = "Hugo", "Bernard" })
	session := e.session(t, e.connect(t, e.login(t), base+"/client.json").AccessToken)

	refs := func(got map[string]any) []string {
		list := got["demandes"].([]any)
		if len(list) == 0 {
			return nil
		}
		out := make([]string, 0, len(list))
		for _, d := range list {
			out = append(out, d.(map[string]any)["ref"].(string))
		}
		return out
	}
	got, _ := callTool(t, session, "comite_list_tickets", nil)
	assert.Equal(t, []string{third.Ref, second.Ref, first.Ref}, refs(got), "the latest first")
	assert.InDelta(t, 3, got["total"], 0)
	assert.Equal(t, false, got["has_more"])
	row := got["demandes"].([]any)[0].(map[string]any)
	assert.InDelta(t, third.ID, row["id"], 0)
	assert.Equal(t, "Hugo Bernard", row["demandeur"])
	assert.Equal(t, "carnet", row["categorie"])
	assert.Equal(t, "todo", row["statut"])
	assert.Equal(t, "À traiter", row["statut_libelle"])
	assert.Equal(t, "04/09/2026 à 12:00", row["deposee_le"])
	assert.Equal(t, "Mon carnet affiche un montant que je ne comprends pas.", row["extrait"])
	assert.NotContains(t, row, "assigne")
	assert.NotContains(t, row, "close_le")

	got, _ = callTool(t, session, "comite_list_tickets", map[string]any{"limit": 2})
	assert.Equal(t, []string{third.Ref, second.Ref}, refs(got))
	assert.Equal(t, true, got["has_more"])
	got, _ = callTool(t, session, "comite_list_tickets", map[string]any{"limit": 2, "offset": 2})
	assert.Equal(t, []string{first.Ref}, refs(got))
	assert.Equal(t, false, got["has_more"])
	got, _ = callTool(t, session, "comite_list_tickets", map[string]any{"offset": 10})
	assert.Empty(t, got["demandes"])

	for name, c := range map[string]struct {
		args map[string]any
		want []string
	}{
		"by address":       {map[string]any{"email": " Hugo.Bernard@example.org"}, []string{third.Ref, second.Ref}},
		"closed only":      {map[string]any{"statuts": []string{"done"}}, nil},
		"open":             {map[string]any{"statuts": []string{"todo", "in_progress"}}, []string{third.Ref, second.Ref, first.Ref}},
		"a category":       {map[string]any{"categorie": "carnet"}, []string{third.Ref, second.Ref, first.Ref}},
		"another category": {map[string]any{"categorie": "paiement"}, nil},
		"taken by nobody":  {map[string]any{"assigne": "-"}, []string{third.Ref, second.Ref, first.Ref}},
		"taken by alice":   {map[string]any{"assigne": "alice"}, nil},
		"one day":          {map[string]any{"du": "2026-09-03", "au": "2026-09-03"}, []string{second.Ref}},
		"since":            {map[string]any{"du": "2026-09-03"}, []string{third.Ref, second.Ref}},
		"until":            {map[string]any{"au": "2026-09-02", "email": "lea.martin@example.org"}, []string{first.Ref}},
	} {
		got, refusal := callTool(t, session, "comite_list_tickets", c.args)
		require.Empty(t, refusal, name)
		assert.Equal(t, c.want, refs(got), name)
		assert.InDelta(t, len(c.want), got["total"], 0, name)
	}
	for want, args := range map[string]map[string]any{
		"statut inconnu":        {"statuts": []string{"draft"}},
		"catégorie inconnue":    {"categorie": "plongee"},
		"AAAA-MM-JJ":            {"du": "03/09/2026"},
		"du avant au":           {"du": "2026-09-05", "au": "2026-09-01"},
		"entiers positifs":      {"offset": -1},
		"adresse mail invalide": {"email": "deux\nmots@example.org"},
	} {
		_, refusal := callTool(t, session, "comite_list_tickets", args)
		assert.Contains(t, refusal, want)
	}
}

func TestMCPTicketByRef(t *testing.T) {
	e, tok := mcpEnv(t)
	session := e.session(t, tok.AccessToken)
	e.submitTicket(t, "lea.martin@example.org")
	tk := e.submitTicket(t, "hugo.bernard@example.org")

	for _, demande := range []string{tk.Ref, strings.ToLower(tk.Ref), " " + tk.Ref + " ", itoa(tk.ID)} {
		got, refusal := callTool(t, session, "comite_get_ticket", map[string]any{"demande": demande})
		require.Empty(t, refusal, demande)
		assert.Equal(t, tk.Ref, got["demande"].(map[string]any)["ref"], demande)
		assert.Equal(t, "hugo.bernard@example.org", got["email"], demande)
	}
	for _, demande := range []string{"CPP-9999", "9999", "0"} {
		_, refusal := callTool(t, session, "comite_get_ticket", map[string]any{"demande": demande})
		assert.Contains(t, refusal, "aucune demande", demande)
	}
	for _, demande := range []string{"", "Hugo Bernard", "hugo.bernard@example.org"} {
		_, refusal := callTool(t, session, "comite_get_ticket", map[string]any{"demande": demande})
		assert.Contains(t, refusal, "CPP-0042", demande)
	}
}

// The committee's notes come with the thread, flagged; the screenshots stay
// behind, down to their existence.
func TestMCPTicketIncludesInternalNotesAndNoCaptures(t *testing.T) {
	e, tok := mcpEnv(t)
	session := e.session(t, tok.AccessToken)
	cookie := e.login(t)
	other := e.submitTicket(t, "hugo.bernard@example.org")
	tk := e.submitTicket(t, "hugo.bernard@example.org", withCapture(t), func(s *tickets.Submission) {
		s.FirstName, s.LastName = "Hugo", "Bernard"
		s.Description = "Rappelle-moi au 06 12 34 56 78, ou écris à hugo.perso@example.org. RIB FR76 3000 6000 0112 3456 7890 189."
	})
	act := func(v url.Values) {
		require.Equal(t, http.StatusSeeOther, e.act(t, cookie, tk.ID, e.openTicket(t, cookie, tk.ID), v).Code)
	}
	act(url.Values{"action": {"take"}})
	act(url.Values{"action": {"note"}, "note": {"Note secrète du comité"}})
	act(url.Values{"action": {"reply_wait"}, "message": {"Peux-tu me montrer ton panier ?"}})
	require.Equal(t, http.StatusSeeOther, e.reply(t, tk.Token, "Voici ce que je vois.", pngBytes(t)).Code)

	got, refusal := callTool(t, session, "comite_get_ticket", map[string]any{"demande": tk.Ref})
	require.Empty(t, refusal)
	demande := got["demande"].(map[string]any)
	assert.Equal(t, "in_progress", demande["statut"], "the member's reply brought it back")
	assert.Equal(t, "alice", demande["assigne"])
	assert.Equal(t, true, demande["adherent_a_repondu_en_dernier"])
	assert.Equal(t, "Rappelle-moi au [téléphone], ou écris à hugo.perso@example.org. RIB [iban].", got["description"])

	messages := got["messages"].([]any)
	require.Len(t, messages, 3)
	note, reply, member := messages[0].(map[string]any), messages[1].(map[string]any), messages[2].(map[string]any)
	assert.Equal(t, "Note secrète du comité", note["texte"])
	assert.Equal(t, true, note["interne"])
	assert.Equal(t, "Alice (Présidente)", note["auteur"])
	assert.Equal(t, false, reply["interne"])
	assert.Equal(t, false, reply["de_l_adherent"])
	assert.Equal(t, "Voici ce que je vois.", member["texte"])
	assert.Equal(t, true, member["de_l_adherent"])
	assert.Equal(t, "adhérent", member["auteur"])

	journal := mustJSON(t, got["journal"])
	for _, want := range []string{"Demande envoyée", "Prise en charge", "Alice (Présidente)", "l'adhérent"} {
		assert.Contains(t, journal, want)
	}
	others := got["autres_demandes"].([]any)
	require.Len(t, others, 1)
	assert.Equal(t, other.Ref, others[0].(map[string]any)["ref"])
	adherent := got["adherent"].(map[string]any)
	assert.Equal(t, "hugo.bernard@example.org", adherent["ref"], "the handle of the member tools")
	assert.Equal(t, "Hugo Bernard", adherent["nom"])

	all := mustJSON(t, got)
	for _, absent := range []string{"capture", "png", "image/", "attachment", "object_key"} {
		assert.NotContains(t, strings.ToLower(all), absent)
	}
	assert.Equal(t, 2, e.count(t, "attachments"), "both screenshots are in place, and none is mentioned")

	stranger := e.submitTicket(t, "inconnue@example.org")
	got, _ = callTool(t, session, "comite_get_ticket", map[string]any{"demande": stranger.Ref})
	assert.NotContains(t, got, "adherent", "an address the members list does not hold names nobody")
}

// What the app's own models wrote about a request stays in the app: the
// summary, the category advice and the fiches chosen.
func TestMCPExcludesStoredModelOutput(t *testing.T) {
	e, tok := mcpEnv(t)
	session := e.session(t, tok.AccessToken)
	const summary = "Résumé écrit par le modèle intégré."
	out, err := e.deps.Tickets.Submit(context.Background(), newSubmission(t, "hugo.bernard@example.org"),
		func(context.Context) (tickets.Suggestion, bool) {
			return tickets.Suggestion{Summary: summary, SuggestedCategory: "paiement", CategorySource: "carnet"}, true
		})
	require.NoError(t, err)
	id, err := e.deps.Tickets.IDByRef(context.Background(), out.Ref)
	require.NoError(t, err)
	require.Equal(t, summary, e.detail(t, id).Summary, "the summary is stored")

	list, _ := callTool(t, session, "comite_list_tickets", nil)
	one, _ := callTool(t, session, "comite_get_ticket", map[string]any{"demande": out.Ref})
	for name, got := range map[string]map[string]any{"list": list, "detail": one} {
		text := mustJSON(t, got)
		assert.NotContains(t, text, "Résumé écrit", name)
		assert.NotContains(t, text, "paiement", name)
		assert.Contains(t, text, "Mon carnet affiche un montant", name+": the member's own words come instead")
	}
}

// Spec §8.3: a closed request that was deleted still counts as closed in
// its month.
func TestMCPTicketStatsAddsArchivedMonths(t *testing.T) {
	e, base := newMCPEnv(t)
	cookie := e.login(t)
	act := func(id int64, action string) {
		require.Equal(t, http.StatusSeeOther, e.act(t, cookie, id, e.openTicket(t, cookie, id), url.Values{"action": {action}}).Code)
	}
	deleted := e.submitTicket(t, "hugo.bernard@example.org")
	act(deleted.ID, "take")
	e.clock.advance(5 * time.Hour)
	act(deleted.ID, "close")
	act(deleted.ID, "delete")
	closed := e.submitTicket(t, "hugo.bernard@example.org")
	act(closed.ID, "take")
	e.clock.advance(2 * time.Hour)
	act(closed.ID, "close")
	e.clock.advance(40 * 24 * time.Hour) // October
	e.submitTicket(t, "lea.martin@example.org")
	session := e.session(t, e.connect(t, e.login(t), base+"/client.json").AccessToken)

	got, refusal := callTool(t, session, "comite_ticket_stats", nil)
	require.Empty(t, refusal)
	assert.Equal(t, map[string]any{"du": "2025-11", "au": "2026-10"}, got["periode"], "twelve months by default")
	assert.Equal(t, []any{
		map[string]any{"statut": "todo", "statut_libelle": "À traiter", "nombre": 1.0},
		map[string]any{"statut": "in_progress", "statut_libelle": "En cours", "nombre": 0.0},
		map[string]any{"statut": "waiting", "statut_libelle": "En attente de l'adhérent", "nombre": 0.0},
		map[string]any{"statut": "done", "statut_libelle": "Fait", "nombre": 1.0},
	}, got["demandes_en_place"])
	assert.Equal(t, []any{
		map[string]any{"mois": "2026-09", "categorie": "carnet", "categorie_libelle": e.deps.Tickets.Catalog.CategoryLabel("carnet"),
			"deposees": 1.0, "closes": 2.0, "heures_moyennes_de_traitement": 3.5, "evitees": 0.0},
		map[string]any{"mois": "2026-10", "categorie": "carnet", "categorie_libelle": e.deps.Tickets.Catalog.CategoryLabel("carnet"),
			"deposees": 1.0, "closes": 0.0, "heures_moyennes_de_traitement": 0.0, "evitees": 0.0},
	}, got["par_mois"])
	assert.Contains(t, got["note"], "365 jours")

	got, _ = callTool(t, session, "comite_ticket_stats", map[string]any{"du": "2026-10", "au": "2026-10"})
	assert.Len(t, got["par_mois"], 1)
	for _, args := range []map[string]any{{"du": "2026-10-01"}, {"au": "octobre"}, {"du": "2026-10", "au": "2026-09"}} {
		_, refusal := callTool(t, session, "comite_ticket_stats", args)
		assert.Contains(t, refusal, "AAAA-MM")
	}
}
