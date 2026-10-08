package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/kb"
)

// toolEnv is a server with every import in place and a toolbox on a fresh
// conversation.
func toolEnv(t *testing.T) (*testEnv, *toolbox) {
	t.Helper()
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	e.importPayments(t)
	e.importMollie(t)
	e.importCalendar(t, "calendar_view.json")
	return e, &toolbox{s: e.srv, c: &assistant.Conversation{}}
}

// call runs a tool and decodes its JSON.
func (tb *toolbox) call(t *testing.T, name, input string) (map[string]any, string) {
	t.Helper()
	out, step, err := tb.run(context.Background(), name, json.RawMessage(input))
	require.NoError(t, err)
	assert.NotContains(t, out, "@", "no address ever reaches the model")
	assert.NotContains(t, out, string(rune(0xa0)), "amounts and carts come with plain spaces")
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &v), out)
	return v, step
}

func TestToolFindMember(t *testing.T) {
	_, tb := toolEnv(t)
	got, step := tb.call(t, "find_member", `{"query":"Léa Martin"}`)
	assert.Equal(t, "exacte", got["correspondance"])
	cands := got["candidats"].([]any)
	require.Len(t, cands, 2)
	assert.Equal(t, true, cands[0].(map[string]any)["homonyme"])
	assert.Equal(t, "Recherche « Léa Martin » : 2 candidats", step)

	got, _ = tb.call(t, "find_member", `{"query":"hugo"}`)
	hugo := got["candidats"].([]any)[0].(map[string]any)
	assert.Equal(t, "m3", hugo["ref"])
	assert.Equal(t, "Hugo Bernard", hugo["nom"])

	tb.c.Emails = []string{"hugo.bernard@example.org"}
	got, _ = tb.call(t, "find_member", `{"query":"[email 1]"}`)
	assert.Equal(t, "m3", got["candidats"].([]any)[0].(map[string]any)["ref"], "one ref per person")

	got, _ = tb.call(t, "find_member", `{"query":"Zoé Inconnue"}`)
	assert.Equal(t, "aucune", got["correspondance"])
	assert.Empty(t, got["candidats"])
	got, _ = tb.call(t, "find_member", `{}`)
	assert.Contains(t, got["erreur"], "query")
	assert.Equal(t, "Liste des membres", tb.sources[0].Label)
}

func TestToolMemberPayments(t *testing.T) {
	_, tb := toolEnv(t)
	tb.call(t, "find_member", `{"query":"Hugo Bernard"}`)
	got, step := tb.call(t, "member_payments", `{"ref":"m1"}`)
	assert.Equal(t, "Paiements de Hugo Bernard lus", step, "the resolver sees whose data was read")
	assert.Equal(t, "ok", got["etat"])
	assert.NotEmpty(t, got["soldes"])
	assert.Contains(t, mustJSON(t, got["soldes"]), "-180,00 €")
	assert.Contains(t, mustJSON(t, got["lignes"]), "05/01/2026", "card purchases always come")
	assert.Contains(t, mustJSON(t, got["mollie"]), "Sortie Porquerolles")

	got, _ = tb.call(t, "member_payments", `{"ref":"m1","du":"2026-06-01","au":"2026-06-05"}`)
	assert.Contains(t, mustJSON(t, got["lignes"]), "03/06/2026 à 19:00", "Plongée Porquerolles, to pay: not a card line, created in the period")
	assert.NotContains(t, mustJSON(t, got["lignes"]), "06/06/2026 à 09:00", "the Porquerolles refund, created the 6th")
	assert.NotContains(t, mustJSON(t, got["lignes"]), "Sortie Sec de la Croix", "created the 9th")

	tb.call(t, "find_member", `{"query":"Léa Martin"}`)
	got, _ = tb.call(t, "member_payments", `{"ref":"m2"}`)
	assert.Equal(t, "ambigu", got["etat"])
	got, _ = tb.call(t, "member_payments", `{"ref":"m9"}`)
	assert.Contains(t, got["erreur"], "find_member")
	got, _ = tb.call(t, "member_payments", `{"ref":"m1","du":"hier"}`)
	assert.Contains(t, got["erreur"], "AAAA-MM-JJ")
}

func TestToolMemberOutings(t *testing.T) {
	_, tb := toolEnv(t)
	tb.call(t, "find_member", `{"query":"Hugo Bernard"}`)
	got, _ := tb.call(t, "member_outings", `{"ref":"m1"}`)
	assert.Equal(t, "ok", got["etat"])
	for _, key := range []string{"import_calendrier", "import_vpdive", "import_mollie"} {
		require.Contains(t, got, key, "the lines and signals say which imports they come from")
		assert.NotEmpty(t, got[key].(map[string]any)["recu"], key)
	}
	labels := make([]string, 0, len(tb.sources))
	for _, src := range tb.sources {
		labels = append(labels, src.Label)
	}
	assert.ElementsMatch(t, []string{"Liste des membres", "Calendrier", "Paiements VPDive", "Encaissements Mollie"}, labels)
	all := mustJSON(t, got["sorties"])
	assert.Contains(t, all, "evt-porquerolles")
	assert.Contains(t, all, "partiel, 40,00 € sur 60,00 €")
	assert.Contains(t, all, `"personnes":2`, "Séjour Corse, upcoming")
	assert.NotContains(t, all, "evt-levant", "older than 90 days by default")

	got, _ = tb.call(t, "member_outings", `{"ref":"m1","du":"2026-05-01"}`)
	assert.Contains(t, mustJSON(t, got["sorties"]), signalCarnet)
}

func TestToolMemberRequests(t *testing.T) {
	e, tb := toolEnv(t)
	first := e.submitTicket(t, "hugo.bernard@example.org")
	e.submitTicket(t, "hugo.bernard@example.org")
	tb.call(t, "find_member", `{"query":"Hugo Bernard"}`)
	got, _ := tb.call(t, "member_requests", `{"ref":"m1"}`)
	reqs := got["demandes"].([]any)
	require.Len(t, reqs, 2)
	assert.Contains(t, mustJSON(t, reqs), first.Ref)
	assert.Contains(t, got["note"], "SMS")
	assert.NotContains(t, got, "tronque")
	assert.Contains(t, tb.sources[len(tb.sources)-1].Link, "/demandes/")
}

func TestToolOutings(t *testing.T) {
	_, tb := toolEnv(t)
	got, _ := tb.call(t, "find_outings", `{"du":"2026-09-01","au":"2026-09-30"}`)
	assert.Len(t, got["sorties"], 4)
	got, _ = tb.call(t, "find_outings", `{"du":"2026-09-01","au":"2026-09-30","texte":"corse"}`)
	assert.Len(t, got["sorties"], 1)
	got, _ = tb.call(t, "find_outings", `{"du":"2026-01-01","au":"2026-09-30"}`)
	assert.Contains(t, got["erreur"], "62 jours")

	got, step := tb.call(t, "outing", `{"id":"evt-porquerolles"}`)
	assert.Equal(t, "Sortie « Sortie Porquerolles » lue", step)
	people := mustJSON(t, got["participants"])
	assert.Contains(t, people, "BERNARD Hugo")
	assert.Contains(t, people, "partiel, 40,00 € sur 60,00 €")
	assert.Contains(t, people, `"participation":"non_inscrit"`, "Famille Roux")
	got, _ = tb.call(t, "outing", `{"id":"evt-inconnu"}`)
	assert.Contains(t, got["erreur"], "inconnue")
}

// A fresh server: a one-event push over calendar_view.json's window would
// be refused as « under half » (CLAUDE.md, lot 8).
func TestToolOutingUnregistrations(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	tb := &toolbox{s: e.srv, c: &assistant.Conversation{}}
	body := []byte(`{"from": "2026-06-04", "to": "2027-09-02", "events": [
		{"id": "evt-garonne", "title": "Sortie Cap Garonne",
		 "starts_at": "2026-06-07T09:00:00+02:00", "ends_at": "2026-06-07T12:00:00+02:00",
		 "participants": [{"vpdive_id": 101, "name": "MARTIN Léa", "last_name": "Martin", "first_name": "Léa", "registered": true, "people": 1}],
		 "unregistrations": [{"last_name": "BERNARD", "first_name": "Hugo", "at": "2026-06-05T16:42:00Z", "by": "Alice ORGANISATRICE"}]}]}`)
	exp, err := calendar.Parse(body, e.srv.paris, e.clock.now())
	require.NoError(t, err)
	require.NoError(t, e.deps.Calendar.Import(context.Background(), exp))
	got, _ := tb.call(t, "outing", `{"id":"evt-garonne"}`)
	left := mustJSON(t, got["desinscriptions"])
	assert.Contains(t, left, "Hugo BERNARD")
	assert.Contains(t, left, "05/06/2026 à 18:42")
	assert.Contains(t, left, "Alice ORGANISATRICE")
}

// Imported free texts (titles, authors, products) reach the model masked
// like the resolver's text.
func TestToolResultsAreMasked(t *testing.T) {
	e := newTestEnv(t)
	e.importMembers(t, "members_valid.xlsx")
	body := []byte(`{"from": "2026-06-04", "to": "2027-09-02", "events": [
		{"id": "evt-contact", "title": "Sortie, contact jean@example.org ou 06 12 34 56 78",
		 "starts_at": "2026-09-10T09:00:00+02:00", "ends_at": "2026-09-10T12:00:00+02:00",
		 "participants": [],
		 "unregistrations": [{"last_name": "Bernard", "first_name": "Hugo", "at": "2026-09-01T10:00:00Z", "by": "Paul +33 6 12 34 56 78"}]}]}`)
	exp, err := calendar.Parse(body, e.srv.paris, e.clock.now())
	require.NoError(t, err)
	require.NoError(t, e.deps.Calendar.Import(context.Background(), exp))
	tb := &toolbox{s: e.srv, c: &assistant.Conversation{}}
	out, _, err := tb.run(context.Background(), "outing", json.RawMessage(`{"id":"evt-contact"}`))
	require.NoError(t, err)
	assert.Contains(t, out, "contact [email 1] ou [téléphone]")
	assert.Contains(t, out, "Paul [téléphone]")
	assert.NotContains(t, out, "jean@")
	assert.True(t, json.Valid([]byte(out)), "masking keeps the JSON valid")
	assert.Equal(t, []string{"jean@example.org"}, tb.c.Emails, "the address is known to find_member")
}

// A planted message cannot have the model read everyone.
func TestToolPeopleCap(t *testing.T) {
	_, tb := toolEnv(t)
	tb.call(t, "find_member", `{"query":"Martin Bernard Petit Durand"}`) // partial matches: m1… m5
	for _, ref := range []string{"m1", "m2", "m3"} {
		got, _ := tb.call(t, "member_requests", `{"ref":"`+ref+`"}`)
		assert.NotContains(t, got, "erreur", ref)
	}
	got, _ := tb.call(t, "member_payments", `{"ref":"m4"}`)
	assert.Contains(t, got["erreur"], "trois personnes")
	got, _ = tb.call(t, "member_payments", `{"ref":"m1"}`)
	assert.NotContains(t, got, "erreur", "a person already read stays readable")
}

func TestToolCancellationsAndFiche(t *testing.T) {
	_, tb := toolEnv(t)
	got, _ := tb.call(t, "cancellations", `{}`)
	assert.Contains(t, mustJSON(t, got["sorties"]), "SORTIE ANNULÉE")
	got, _ = tb.call(t, "read_fiche", `{"id":"carnet-solde-negatif"}`)
	assert.NotEmpty(t, got["procedure"])
	assert.NotEmpty(t, got["reponse_adherent"])
	got, _ = tb.call(t, "read_fiche", `{"id":"nope"}`)
	assert.Contains(t, got["erreur"], "inconnue")
	got, _ = tb.call(t, "nope", `{}`)
	assert.Contains(t, got["erreur"], "inconnu")
}

// The head count of an outing is the same in the list and on its page, the
// waiting list apart.
func TestToolOutingHeadCount(t *testing.T) {
	_, tb := toolEnv(t)
	got, _ := tb.call(t, "find_outings", `{"du":"2026-09-01","au":"2026-09-30","texte":"corse"}`)
	corse := got["sorties"].([]any)[0].(map[string]any)
	assert.InDelta(t, 1, corse["inscrits"], 0, "Noé is in; the two places of Hugo wait")
	assert.InDelta(t, 2, corse["liste_attente"], 0)
	got, _ = tb.call(t, "outing", `{"id":"evt-corse"}`)
	assert.Equal(t, corse, got["sortie"], "one description of an event")
}

// toolSpans lists the assistant.tool spans recorded, as "name/outcome".
func toolSpans(spans *tracetest.SpanRecorder) []string {
	ended := spans.Ended()
	out := make([]string, 0, len(ended))
	for _, span := range ended {
		attrs := map[string]string{}
		for _, a := range span.Attributes() {
			attrs[string(a.Key)] = a.Value.AsString()
		}
		out = append(out, attrs["assistant.tool.name"]+"/"+attrs["assistant.tool.outcome"])
	}
	return out
}

// The model chooses a tool name: telemetry records the eight known ones and
// "unknown" for anything else, each with its outcome (ok, refused, or the
// code of a failure), and the answer to a made-up name is masked.
func TestToolSpansCarryNameAndOutcome(t *testing.T) {
	_, tb := toolEnv(t)
	spans := tracetest.NewSpanRecorder()
	tb.s.tracer = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
	tb.call(t, "find_member", `{"query":"hugo"}`)
	got, step := tb.call(t, "write to jean@example.org", `{}`)
	assert.Contains(t, got["erreur"], "inconnu")
	assert.Contains(t, got["erreur"], "[email 1]")
	assert.Equal(t, "Outil inconnu demandé", step)
	tb.call(t, "member_payments", `{"ref":"m9"}`)

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := tb.run(gone, "cancellations", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Equal(t, []string{"find_member/ok", "unknown/refused", "member_payments/refused", "cancellations/canceled"}, toolSpans(spans))
}

// Lists are capped at maxLines, with a flag.
func TestToolListsAreCapped(t *testing.T) {
	e, tb := toolEnv(t)
	for range maxLines + 1 {
		_, err := e.deps.Tickets.Submit(context.Background(), newSubmission(t, "hugo.bernard@example.org"), nil)
		require.NoError(t, err)
	}
	tb.call(t, "find_member", `{"query":"Hugo Bernard"}`)
	got, _ := tb.call(t, "member_requests", `{"ref":"m1"}`)
	assert.Len(t, got["demandes"], maxLines)
	assert.Equal(t, true, got["tronque"])
}

// A result says when its import was received, and whether it is stale.
func TestToolImportState(t *testing.T) {
	e := newTestEnv(t)
	tb := &toolbox{s: e.srv, c: &assistant.Conversation{}}
	st, err := e.srv.importState(context.Background(), imports.Members)
	require.NoError(t, err)
	assert.Equal(t, "aucun import", st.describe())

	e.importMembers(t, "members_valid.xlsx")
	st, err = e.srv.importState(context.Background(), imports.Members)
	require.NoError(t, err)
	assert.False(t, st.Perime)
	assert.Contains(t, st.describe(), "reçu le 02/09/2026")

	e.clock.advance(15 * 24 * time.Hour) // the members list may be 14 days old
	got, _ := tb.call(t, "find_member", `{"query":"hugo"}`)
	assert.Equal(t, true, got["import_membres"].(map[string]any)["perime"])
	require.Len(t, tb.sources, 1)
	assert.True(t, tb.sources[0].Stale)
	assert.Equal(t, "02/09/2026", tb.sources[0].Date)
	st, err = e.srv.importState(context.Background(), imports.Members)
	require.NoError(t, err)
	assert.Contains(t, st.describe(), "périmé")
}

func TestBlocksText(t *testing.T) {
	text := blocksText([]kb.Block{
		{Kind: kb.Paragraph, Items: []string{"Vérifie le solde."}},
		{Kind: kb.Numbers, Items: []string{"Ouvre la fiche.", "Note le montant."}},
		{Kind: kb.Bullets, Items: []string{"carnet", "formation"}},
	})
	assert.Equal(t, "Vérifie le solde.\n\n1. Ouvre la fiche.\n2. Note le montant.\n\n- carnet\n- formation", text)
}

// Every definition has a valid schema and is served by run.
func TestToolDefsAreServed(t *testing.T) {
	_, tb := toolEnv(t)
	tools := tb.tools()
	names := make([]string, 0, len(tools.Defs))
	for _, def := range tools.Defs {
		names = append(names, def.Name)
		var schema struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		require.NoError(t, json.Unmarshal(def.InputSchema, &schema), def.Name)
		assert.Equal(t, "object", schema.Type, def.Name)
		for _, name := range schema.Required {
			assert.Contains(t, schema.Properties, name, def.Name)
		}
		assert.NotEmpty(t, def.Description, def.Name)
		out, _, err := tools.Run(context.Background(), def.Name, json.RawMessage(`{}`))
		require.NoError(t, err, def.Name)
		assert.NotContains(t, out, "outil inconnu", def.Name)
	}
	assert.ElementsMatch(t, []string{"find_member", "member_payments", "member_outings", "member_requests",
		"find_outings", "outing", "cancellations", "read_fiche"}, names)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
