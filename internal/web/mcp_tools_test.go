package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/ffessm"
)

// withRegister puts a fake federation register in place of the FFESSM
// client.
func withRegister(f *fakeRegister) func(*Deps) {
	return func(d *Deps) { d.Licences = f }
}

// undocumented lists the properties of a JSON schema that carry no
// description, by path.
func undocumented(schema any, path string) []string {
	node, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	if props, ok := node["properties"].(map[string]any); ok {
		for name, prop := range props {
			at := path + "." + name
			if p, ok := prop.(map[string]any); !ok || p["description"] == nil || p["description"] == "" {
				out = append(out, at)
			}
			out = append(out, undocumented(prop, at)...)
		}
	}
	out = append(out, undocumented(node["items"], path+"[]")...)
	if defs, ok := node["$defs"].(map[string]any); ok {
		for name, def := range defs {
			out = append(out, undocumented(def, path+"#"+name)...)
		}
	}
	return out
}

// The client's model has nothing but these texts to understand the app:
// every tool says what it does, and every field what it holds.
func TestMCPEveryToolIsDocumented(t *testing.T) {
	e, tok := mcpEnv(t, withRegister(&fakeRegister{}))
	session := e.session(t, tok.AccessToken)
	assert.Greater(t, utf8.RuneCountInString(session.InitializeResult().Instructions), 800, "the server explains its data model")

	list, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	names := make([]string, 0, len(list.Tools))
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		assert.True(t, strings.HasPrefix(tool.Name, "comite_"), tool.Name)
		assert.LessOrEqual(t, len(tool.Name), 64, tool.Name)
		assert.NotEmpty(t, tool.Title, tool.Name)
		assert.Greater(t, utf8.RuneCountInString(tool.Description), 150, "%s: a description says what comes back, from where and within which limits", tool.Name)
		require.NotNil(t, tool.Annotations, tool.Name)
		assert.True(t, tool.Annotations.ReadOnlyHint, tool.Name)
		require.NotNil(t, tool.Annotations.OpenWorldHint, tool.Name)
		assert.False(t, *tool.Annotations.OpenWorldHint, tool.Name)
		require.NotNil(t, tool.OutputSchema, tool.Name)
		for kind, schema := range map[string]any{"input": tool.InputSchema, "output": tool.OutputSchema} {
			raw, err := json.Marshal(schema)
			require.NoError(t, err)
			var decoded any
			require.NoError(t, json.Unmarshal(raw, &decoded))
			assert.Empty(t, undocumented(decoded, tool.Name+" "+kind), "fields without a description")
		}
	}
	assert.ElementsMatch(t, []string{
		"comite_find_member", "comite_member_payments", "comite_member_outings", "comite_member_membership",
		"comite_member_licence", "comite_search_licences", "comite_find_outings", "comite_get_outing",
		"comite_list_cancellations", "comite_list_memberships", "comite_list_anomalies", "comite_import_status",
		"comite_list_tickets", "comite_get_ticket", "comite_ticket_stats",
	}, names)

	off, tok := mcpEnv(t)
	list, err = off.session(t, tok.AccessToken).ListTools(context.Background(), nil)
	require.NoError(t, err)
	for _, tool := range list.Tools {
		assert.NotContains(t, tool.Name, "licence", "no register tool without the club's FFESSM account")
	}
}

// A connection calls only the tools its scope covers: the ground for a
// write scope, which no tool asks for yet.
func TestMCPToolNeedsItsScope(t *testing.T) {
	e, tok := mcpEnv(t)
	_, err := e.db.ExecContext(context.Background(), `UPDATE mcp_connections SET scope = 'other'`)
	require.NoError(t, err)
	_, refusal := callTool(t, e.session(t, tok.AccessToken), "comite_import_status", nil)
	assert.Contains(t, refusal, "« read »")
}

// Every tool answers on a database that holds nothing: empty lists, never a
// null the output schema would refuse, never a failure.
func TestMCPToolsAnswerOnAnEmptyDatabase(t *testing.T) {
	e, base := newMCPEnv(t, withRegister(&fakeRegister{}))
	session := e.session(t, e.connect(t, e.login(t), base+"/client.json").AccessToken)
	for name, args := range map[string]map[string]any{
		"comite_find_member":        {"query": "Hugo"},
		"comite_find_outings":       {"du": "2026-09-01", "au": "2026-09-30"},
		"comite_list_cancellations": nil,
		"comite_list_memberships":   nil,
		"comite_list_anomalies":     nil,
		"comite_import_status":      nil,
		"comite_list_tickets":       nil,
		"comite_ticket_stats":       nil,
		"comite_search_licences":    {"query": "Bernard Hugo"},
		"comite_member_licence":     {"numero": "A-13-000001"},
	} {
		got, refusal := callTool(t, session, name, args)
		assert.Empty(t, refusal, name)
		assert.NotNil(t, got, name)
	}
	for name, args := range map[string]map[string]any{
		"comite_member_payments":   {"adherent": "hugo.bernard@example.org"},
		"comite_member_outings":    {"adherent": "hugo.bernard@example.org"},
		"comite_member_membership": {"adherent": "hugo.bernard@example.org"},
		"comite_get_outing":        {"id": "inconnue"},
		"comite_get_ticket":        {"demande": "CPP-0001"},
	} {
		_, refusal := callTool(t, session, name, args)
		assert.NotEmpty(t, refusal, name)
		assert.NotEqual(t, errMCPInternal.Error(), refusal, name)
	}
	assert.NotContains(t, e.logs.String(), "mcp tool failed")
	assert.Empty(t, e.sentryEvents(t))
}

func TestMCPToolsRoundTripThroughSDKClient(t *testing.T) {
	e, tok := mcpEnv(t)
	session := e.session(t, tok.AccessToken)

	got, _ := callTool(t, session, "comite_find_member", map[string]any{"query": "hugo"})
	hugo := got["candidats"].([]any)[0].(map[string]any)
	assert.Equal(t, "Hugo Bernard", hugo["nom"])
	assert.Equal(t, "hugo.bernard@example.org", hugo["ref"], "a member's handle is their address")
	assert.Equal(t, "Moniteurs actifs, Membres actifs", hugo["organisation"])

	got, _ = callTool(t, session, "comite_member_payments", map[string]any{"adherent": hugo["ref"]})
	assert.Equal(t, "ok", got["etat"])
	assert.Contains(t, mustJSON(t, got["soldes"]), "-180,00 €")
	assert.Contains(t, mustJSON(t, got["mollie"]), "Sortie Porquerolles")
	assert.Equal(t, map[string]any{"du": "05/05/2026", "au": "sans fin"}, got["periode_lue"], "the last 120 days by default")
	got, _ = callTool(t, session, "comite_member_payments", map[string]any{"adherent": " Hugo.Bernard@Example.org ", "du": "2026-06-01", "au": "2026-06-05"})
	assert.Equal(t, map[string]any{"du": "01/06/2026", "au": "05/06/2026"}, got["periode_lue"])
	_, refusal := callTool(t, session, "comite_member_payments", map[string]any{"adherent": hugo["ref"], "du": "hier"})
	assert.Contains(t, refusal, "AAAA-MM-JJ")

	got, _ = callTool(t, session, "comite_member_outings", map[string]any{"adherent": hugo["ref"], "du": "2026-01-01"})
	assert.Equal(t, "ok", got["etat"])
	outings := got["sorties"].([]any)
	require.NotEmpty(t, outings)
	id := outings[0].(map[string]any)["id"]

	got, _ = callTool(t, session, "comite_get_outing", map[string]any{"id": id})
	assert.Equal(t, outings[0].(map[string]any)["titre"], got["sortie"].(map[string]any)["titre"])
	assert.NotEmpty(t, got["participants"])
	_, refusal = callTool(t, session, "comite_get_outing", map[string]any{"id": "inconnue"})
	assert.Contains(t, refusal, "comite_find_outings")

	got, _ = callTool(t, session, "comite_find_outings", map[string]any{"du": "2026-08-15", "au": "2026-10-10"})
	assert.NotEmpty(t, got["sorties"])
	_, refusal = callTool(t, session, "comite_find_outings", map[string]any{"du": "2026-01-01", "au": "2026-12-31"})
	assert.Contains(t, refusal, "62 jours")

	got, _ = callTool(t, session, "comite_list_cancellations", nil)
	assert.Contains(t, got, "sorties")
	got, _ = callTool(t, session, "comite_list_anomalies", nil)
	assert.Contains(t, got, "a_verifier")
	assert.Contains(t, got, "masquees")

	got, _ = callTool(t, session, "comite_import_status", nil)
	states := got["imports"].([]any)
	require.Len(t, states, 6)
	byType := map[string]map[string]any{}
	for _, st := range states {
		byType[st.(map[string]any)["type"].(string)] = st.(map[string]any)
	}
	assert.Equal(t, "Liste des membres", byType["members"]["libelle"])
	assert.Equal(t, false, byType["members"]["aucun_import"])
	assert.InDelta(t, 336, byType["members"]["age_max_heures"], 0)
	assert.Equal(t, true, byType["campaign"]["aucun_import"])
	assert.Empty(t, byType["campaign"]["passages"])
}

// The homonym rule holds without a conversation: a name two members bear
// reads nothing, whoever's address names them.
func TestMCPHomonymExposesNothing(t *testing.T) {
	e, tok := mcpEnv(t)
	session := e.session(t, tok.AccessToken)
	got, _ := callTool(t, session, "comite_find_member", map[string]any{"query": "Léa Martin"})
	cands := got["candidats"].([]any)
	require.Len(t, cands, 2)
	for _, c := range cands {
		lea := c.(map[string]any)
		assert.Equal(t, true, lea["homonyme"])
		got, _ := callTool(t, session, "comite_member_payments", map[string]any{"adherent": lea["ref"]})
		assert.Equal(t, "ambigu", got["etat"])
		assert.Empty(t, got["lignes"])
		assert.Empty(t, got["soldes"])
		assert.Empty(t, got["cartes"])
		got, _ = callTool(t, session, "comite_member_outings", map[string]any{"adherent": lea["ref"]})
		assert.Equal(t, "ambigu", got["etat"])
		assert.Empty(t, got["sorties"])
	}
}

// Someone the campaign names and the members list does not has no address:
// their handle is their name's keyed hash, which no name gives away.
func TestMCPMemberHandleByEmailAndCampaign(t *testing.T) {
	e, tok := mcpEnv(t)
	e.importCampaign(t)
	session := e.session(t, tok.AccessToken)

	got, _ := callTool(t, session, "comite_list_memberships", nil)
	assert.Equal(t, "ok", got["etat"])
	people := got["personnes"].([]any)
	require.NotEmpty(t, people)
	assert.InDelta(t, len(people), got["total"], 0)
	assert.Equal(t, false, got["has_more"])

	var handle string
	for _, p := range people {
		found, _ := callTool(t, session, "comite_find_member", map[string]any{"query": p.(map[string]any)["nom"]})
		for _, c := range found["candidats"].([]any) {
			if c := c.(map[string]any); c["source"] == sourceCampaign && c["homonyme"] != true {
				handle = c["ref"].(string)
			}
		}
	}
	require.True(t, strings.HasPrefix(handle, campaignHandle), "campaign_valid.xlsx names someone the members list does not")

	got, _ = callTool(t, session, "comite_member_membership", map[string]any{"adherent": handle})
	assert.Equal(t, "ok", got["etat"])
	assert.NotEmpty(t, got["lignes"])
	got, _ = callTool(t, session, "comite_member_payments", map[string]any{"adherent": handle})
	assert.Equal(t, "vide", got["etat"], "no payment line bears a name the members list does not hold")
	assert.Empty(t, got["lignes"])

	for _, forged := range []string{campaignHandle + "bm9uZQ", campaignHandle + "pas du base64 !", campaignHandle, "campagne:" + strings.Repeat("A", 43)} {
		_, refusal := callTool(t, session, "comite_member_membership", map[string]any{"adherent": forged})
		assert.Contains(t, refusal, "référence inconnue", forged)
	}
	_, refusal := callTool(t, session, "comite_member_membership", map[string]any{"adherent": "personne@example.org"})
	assert.Contains(t, refusal, "comite_find_member")
	_, refusal = callTool(t, session, "comite_member_membership", map[string]any{"adherent": "deux mots@example.org"})
	assert.Contains(t, refusal, "adresse mail invalide")
	_, refusal = callTool(t, session, "comite_member_membership", map[string]any{"adherent": " "})
	assert.Contains(t, refusal, "adherent attendu")
}

func TestMCPListMembershipsPaginates(t *testing.T) {
	e, tok := mcpEnv(t)
	e.importCampaign(t)
	session := e.session(t, tok.AccessToken)
	all, _ := callTool(t, session, "comite_list_memberships", nil)
	people := all["personnes"].([]any)
	require.Greater(t, len(people), 2)

	first, _ := callTool(t, session, "comite_list_memberships", map[string]any{"limit": 2})
	assert.Equal(t, people[:2], first["personnes"])
	assert.Equal(t, true, first["has_more"])
	assert.Equal(t, all["total"], first["total"])
	last, _ := callTool(t, session, "comite_list_memberships", map[string]any{"limit": 2, "offset": len(people) - 1})
	assert.Equal(t, people[len(people)-1:], last["personnes"])
	assert.Equal(t, false, last["has_more"])
	past, _ := callTool(t, session, "comite_list_memberships", map[string]any{"offset": 1000})
	assert.Empty(t, past["personnes"])

	total := 0
	for _, section := range []string{"a-valider", "valides", "incomplets"} {
		got, _ := callTool(t, session, "comite_list_memberships", map[string]any{"section": section})
		for _, p := range got["personnes"].([]any) {
			assert.Equal(t, section, p.(map[string]any)["section"])
		}
		total += len(got["personnes"].([]any))
	}
	assert.Equal(t, len(people), total)
	_, refusal := callTool(t, session, "comite_list_memberships", map[string]any{"section": "tous"})
	assert.Contains(t, refusal, "section inconnue")
	_, refusal = callTool(t, session, "comite_list_memberships", map[string]any{"limit": -1})
	assert.Contains(t, refusal, "entiers positifs")
}

// Addresses go to the client, which the member allowed; phone numbers and
// bank details never do.
func TestMCPMasksPhonesAndIBANsKeepsEmails(t *testing.T) {
	type sample struct {
		Ref   string   `json:"ref"`
		Note  string   `json:"note"`
		Liste []string `json:"liste"`
		N     int      `json:"n"`
	}
	got, err := maskNumbers(sample{Ref: "lea@example.org", N: 612345678,
		Note:  "Écris à lea@example.org ou appelle le 06 12 34 56 78.",
		Liste: []string{"IBAN FR76 3000 6000 0112 3456 7890 189", "jean.dupont2@example.org"}})
	require.NoError(t, err)
	assert.Equal(t, sample{Ref: "lea@example.org", N: 612345678,
		Note:  "Écris à lea@example.org ou appelle le [téléphone].",
		Liste: []string{"IBAN [iban]", "jean.dupont2@example.org"}}, got)
}

func TestMCPLicenceTools(t *testing.T) {
	register := &fakeRegister{rows: map[string][]ffessm.Licence{
		"hugo.bernard@example.org": {goodLicence},
		"a-13-000002":              {oldLicence},
		"bernard hugo":             {goodLicence, oldLicence},
	}}
	e, tok := mcpEnv(t, withRegister(register))
	session := e.session(t, tok.AccessToken)

	got, _ := callTool(t, session, "comite_member_licence", map[string]any{"adherent": "hugo.bernard@example.org"})
	assert.Equal(t, map[string]any{"etat": "valide", "valable_jusqu_au": "31/12/2099"}, got)
	got, _ = callTool(t, session, "comite_member_licence", map[string]any{"numero": "A-13-000002"})
	assert.Equal(t, "non valide", got["etat"])
	for _, args := range []map[string]any{{}, {"adherent": "hugo.bernard@example.org", "numero": "A-13-000002"}} {
		_, refusal := callTool(t, session, "comite_member_licence", args)
		assert.Contains(t, refusal, "un seul des deux")
	}
	_, refusal := callTool(t, session, "comite_member_licence", map[string]any{"numero": "Bernard Hugo"})
	assert.Contains(t, refusal, "A-13-634166", "a name would find homonyms")

	got, _ = callTool(t, session, "comite_search_licences", map[string]any{"query": "Bernard Hugo"})
	found := got["resultats"].([]any)
	require.Len(t, found, 2)
	assert.Equal(t, map[string]any{"numero": "A-13-000001", "nom": "BERNARD Hugo", "club": "CLUB DU PRADET", "valable_jusqu_au": "31/12/2099", "valide": true}, found[0])
	assert.NotContains(t, mustJSON(t, got), "1990", "no birth date leaves the register")
	_, refusal = callTool(t, session, "comite_search_licences", map[string]any{"query": "ab"})
	assert.Contains(t, refusal, "3 caractères")

	register.mu.Lock()
	register.err = assert.AnError
	register.mu.Unlock()
	_, refusal = callTool(t, session, "comite_search_licences", map[string]any{"query": "Martin Léa"})
	assert.Contains(t, refusal, "ne répond pas")
	assert.Empty(t, e.sentryEvents(t), "an unreachable register is expected")
}

// The register is the federation's, read with the club's own account: a
// connection looks up 30 times an hour at most.
func TestMCPLicenceToolIsRateLimited(t *testing.T) {
	e, tok := mcpEnv(t, withRegister(&fakeRegister{}))
	session := e.session(t, tok.AccessToken)
	for range mcpLicenceLimit {
		_, refusal := callTool(t, session, "comite_search_licences", map[string]any{"query": "Bernard Hugo"})
		require.Empty(t, refusal)
	}
	_, refusal := callTool(t, session, "comite_search_licences", map[string]any{"query": "Bernard Hugo"})
	assert.Contains(t, refusal, "30 recherches par heure")
	_, refusal = callTool(t, session, "comite_member_licence", map[string]any{"numero": "A-13-000001"})
	assert.Contains(t, refusal, "30 recherches par heure", "both tools share the limit")
	var res *mcp.CallToolResult
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "comite_import_status"})
	require.NoError(t, err)
	assert.False(t, res.IsError, "the other tools go on")
}
