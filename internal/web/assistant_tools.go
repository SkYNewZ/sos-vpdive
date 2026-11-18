package web

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/campaign"
	"github.com/SkYNewZ/sos-vpdive/internal/carnets"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

// Tool limits (design: Tools).
const (
	maxCandidates   = 10
	maxLines        = 100
	maxOutings      = 50
	paymentsBack    = 120 // days of lines by default
	outingsBack     = 90  // days of outings by default
	maxOutingsRange = 62  // days find_outings covers at most
	// maxPeople caps the people whose payments, outings or requests one
	// answer reads: a planted message cannot have the model harvest the
	// members list. Three covers a couple and a third person.
	maxPeople = 3
)

// schema is a JSON Schema object of string properties, required ones first.
func schema(required []string, optional ...string) json.RawMessage {
	props := make([]string, 0, len(required)+len(optional))
	for _, p := range slices.Concat(required, optional) {
		props = append(props, strconv.Quote(p)+`:{"type":"string"}`)
	}
	req := make([]string, len(required))
	for i, p := range required {
		req[i] = strconv.Quote(p)
	}
	return json.RawMessage(`{"type":"object","properties":{` + strings.Join(props, ",") + `},"required":[` + strings.Join(req, ",") + `]}`)
}

// toolDefs are the read-only tools of the assistant (design: Tools). Dates
// are YYYY-MM-DD, Paris.
var toolDefs = []assistant.Tool{
	{Name: "find_member", InputSchema: schema([]string{"query"}),
		Description: "Cherche des adhérents dans la liste des membres par nom, prénom ou les deux (ordre libre, accents et casse ignorés), ou par « [email N] » tel qu'il apparaît dans la saisie. Renvoie au plus 10 candidats, chacun avec une référence m1, m2… à passer aux autres outils, et ses groupes VPDive (organisation)."},
	{Name: "member_payments", InputSchema: schema([]string{refArg}, "du", "au"),
		Description: "Paiements VPDive et encaissements Mollie d'un adhérent (référence m1…) : soldes de carnet et de formation, achats de carte, puis les lignes de la période, par défaut les 120 derniers jours. Dates du et au : AAAA-MM-JJ. Donne aussi chaque carte de plongées de l'adhérent avec tout son historique : plongées débitées ou recréditées, changements de prix, commentaires."},
	{Name: "member_outings", InputSchema: schema([]string{refArg}, "du", "au"),
		Description: "Sorties VPDive d'un adhérent : inscription, panier, lignes de paiement rattachées, signaux, désinscriptions. Par défaut, des 90 derniers jours à toutes celles à venir. Dates du et au : AAAA-MM-JJ."},
	{Name: "member_membership", InputSchema: schema([]string{refArg}),
		Description: "Campagne d'adhésion VPDive d'une personne (référence m1…). Statut « En cours » : dossier pas encore validé par le club, ce qui bloque le paiement de la saison ; « Validé » : dossier validé, avec la date et qui l'a validé. Chaque entrée d'adhésion et de licence a un état : « Réglé » est payé, « Au panier » est ajouté au panier mais pas encore payé ; moyen et date seulement pour un paiement saisi à la main ou par VPayDive. La date de l'export (export_du) fait foi : un paiement ou une validation plus récents n'y sont pas encore. Un nom que portent plusieurs personnes répond homonyme, sans détail."},
	{Name: "member_requests", InputSchema: schema([]string{refArg}),
		Description: "Demandes déposées dans cet outil par l'adresse de l'adhérent : référence, catégorie, statut, date, résumé."},
	{Name: "find_outings", InputSchema: schema([]string{"du", "au"}, "texte"),
		Description: "Sorties du calendrier entre deux dates (62 jours au plus), filtrées par un mot du titre si texte est donné : identifiant, titre, date, catégorie, nombre d'inscrits."},
	{Name: "outing", InputSchema: schema([]string{"id"}),
		Description: "Une sortie du calendrier par son identifiant : chaque participant avec son inscription et l'état de son panier, puis les désinscriptions."},
	{Name: "cancellations", InputSchema: schema(nil),
		Description: "Sorties annulées dont des lignes payées attendent la suppression dans VPDive : nombre de payeurs, total à recréditer sur les carnets, total payé en argent."},
	{Name: licenceTool, InputSchema: schema(nil, refArg, "numero"),
		Description: "Statut de la licence FFESSM d'une personne dans le registre de la fédération, pour savoir si elle peut plonger : donne une référence m1… (recherche par son adresse mail) ou un numéro de licence. Le registre compte beaucoup d'homonymes : ne cherche jamais par nom, et si l'adresse ne donne rien, dis-le sans conclure que la personne n'a pas de licence. Renvoie seulement valide, non valide, introuvable ou plusieurs, avec la date de fin. À ne pas confondre avec la ligne « Licence » de member_membership, qui dit si la licence a été payée dans VPDive."},
	{Name: "read_fiche", InputSchema: schema([]string{"id"}),
		Description: "Une fiche d'aide du club par son identifiant : réponse pour l'adhérent, procédure pour le résolveur, liens VPDive."},
}

// licenceTool is the register tool, offered only with the licence search on.
const licenceTool = "licence_status"

// licenceRefused is the step of a licence_status call the model got wrong.
const licenceRefused = "Licence FFESSM refusée"

// refArg is the argument that names a person in the member tools.
const refArg = "ref"

// findOutingsRefused is the step of a find_outings call the model got wrong.
const findOutingsRefused = "Recherche de sorties refusée"

// importLabel names an import for the model and the source chips.
var importLabel = map[imports.Kind]string{
	imports.Members:  "Liste des membres",
	imports.Payments: "Paiements VPDive",
	imports.Mollie:   "Encaissements Mollie",
	imports.Calendar: "Calendrier",
	imports.Carnets:  "Cartes VPDive",
	imports.Campaign: "Campagne d'adhésion",
}

// toolbox runs the tools of one answer on the conversation c and gathers
// the sources the answer read.
type toolbox struct {
	s       *Server
	c       *assistant.Conversation
	sources []assistant.Source
	read    []string // refs whose data this answer read, maxPeople at most
}

// tools is what the model may call during one answer.
func (t *toolbox) tools() assistant.Tools {
	defs := toolDefs
	if t.s.licences == nil {
		defs = slices.DeleteFunc(slices.Clone(toolDefs), func(d assistant.Tool) bool { return d.Name == licenceTool })
	}
	return assistant.Tools{Defs: defs, Run: t.run}
}

// toolProblem is a call the model got wrong: it reads why and may try again.
type toolProblem struct {
	Erreur string `json:"erreur"`
}

// run executes one call and returns the JSON the model reads, masked, and
// the step the resolver sees. A call the model got wrong, an unknown tool
// included, is answered in the JSON: only a failure of ours is an error.
func (t *toolbox) run(ctx context.Context, name string, input json.RawMessage) (string, string, error) {
	ctx, span := t.s.tracer.Start(ctx, "assistant.tool",
		trace.WithAttributes(attribute.String("assistant.tool.name", assistant.KnownTool(toolDefs, name))))
	outcome := "ok" // or "refused" for a call the model got wrong, else the error code
	defer func() {
		span.SetAttributes(attribute.String("assistant.tool.outcome", outcome))
		span.End()
	}()
	var (
		out  any
		step string
		err  error
	)
	switch name {
	case "find_member":
		out, step, err = t.findMember(ctx, input)
	case "member_payments":
		out, step, err = t.memberPayments(ctx, input)
	case "member_outings":
		out, step, err = t.memberOutings(ctx, input)
	case "member_membership":
		out, step, err = t.memberMembership(ctx, input)
	case "member_requests":
		out, step, err = t.memberRequests(ctx, input)
	case "find_outings":
		out, step, err = t.findOutings(ctx, input)
	case "outing":
		out, step, err = t.outing(ctx, input)
	case "cancellations":
		out, step, err = t.cancellations(ctx)
	case licenceTool:
		out, step, err = t.licenceStatus(ctx, input)
	case "read_fiche":
		out, step = t.readFiche(input)
	default:
		out, step = toolProblem{"outil inconnu : " + name}, "Outil inconnu demandé"
	}
	if err != nil {
		outcome = failureCode(ctx)
		telemetry.Fail(span, outcome)
		return "", "", err
	}
	raw, err := json.Marshal(out)
	var emails []string
	if err == nil {
		// Every imported text is free: a product, an outing title, an author or
		// a request summary may hold an address, a phone number or an IBAN. The
		// mask reads the string values, not the encoded document, where a line
		// break or an angle bracket sticks to what follows it.
		raw, emails, err = assistant.MaskJSON(raw, t.c.Emails)
	}
	if err != nil {
		outcome = "encode"
		telemetry.Fail(span, outcome)
		return "", "", fmt.Errorf("encode tool result: %w", err)
	}
	t.c.Emails = emails
	if _, refused := out.(toolProblem); refused {
		outcome = outcomeRefused
	}
	return string(raw), step, nil
}

// outcomeRefused is the outcome of a tool call its caller got wrong.
const outcomeRefused = "refused"

// failureCode is the stable code of a failed call: the deadline or the
// resolver's leaving when ctx is done, else the store.
func failureCode(ctx context.Context) string {
	switch err := ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		return outcomeTimeout
	case errors.Is(err, context.Canceled):
		return outcomeCanceled
	}
	return "store"
}

// decode reads a tool input; ok is false when it is not the expected object.
func decode[T any](input json.RawMessage) (T, bool) {
	var v T
	return v, json.Unmarshal(input, &v) == nil
}

// addSource records a piece of data the answer read, once.
func (t *toolbox) addSource(src assistant.Source) { t.sources = appendSource(t.sources, src) }

// appendSource adds src to sources unless one of the same label and link is there.
func appendSource(sources []assistant.Source, src assistant.Source) []assistant.Source {
	if slices.ContainsFunc(sources, func(o assistant.Source) bool { return o.Label == src.Label && o.Link == src.Link }) {
		return sources
	}
	return append(sources, src)
}

// importState is an import as the model reads it.
type importState struct {
	Recu   string `json:"recu,omitempty"         jsonschema:"Date et heure de réception du dernier import, JJ/MM/AAAA HH:MM ; absent sans import."`
	Du     string `json:"periode_du,omitempty"   jsonschema:"Premier jour que couvre l'export, JJ/MM/AAAA, quand il porte une période."`
	Au     string `json:"periode_au,omitempty"   jsonschema:"Dernier jour que couvre l'export, JJ/MM/AAAA."`
	Perime bool   `json:"perime"                 jsonschema:"Vrai quand le dernier import a dépassé son âge maximal : les données peuvent être en retard sur VPDive."`
	Aucun  bool   `json:"aucun_import,omitempty" jsonschema:"Vrai quand cet export n'a jamais été importé."`
}

// importState reads the latest import of kind.
func (s *Server) importState(ctx context.Context, kind imports.Kind) (importState, error) {
	info, ok, err := imports.Last(ctx, s.db, kind)
	if err != nil {
		return importState{}, err
	}
	return s.stateOf(info, ok), nil
}

// stateOf is the state of a latest import, as imports.Last read it: stale
// past the age of its banner.
func (s *Server) stateOf(info imports.Info, ok bool) importState {
	if !ok {
		return importState{Aucun: true}
	}
	st := importState{Recu: s.formatTime(info.ImportedAt), Du: s.formatDate(info.PeriodFrom), Au: s.formatDate(info.PeriodTo)}
	for _, a := range s.importAges() {
		if a.kind == info.Kind {
			st.Perime = s.now().Sub(info.ImportedAt) > a.maxAge
		}
	}
	return st
}

// describe is the state in a sentence, for the first message.
func (st importState) describe() string {
	if st.Aucun {
		return "aucun import"
	}
	out := "reçu le " + st.Recu
	if st.Du != "" {
		out += ", période du " + st.Du + " au " + st.Au
	}
	if st.Perime {
		out += ", périmé"
	}
	return out
}

// paymentImports reads the states of the payments and Mollie imports, which
// every result built from their lines carries.
func (t *toolbox) paymentImports(ctx context.Context) (vpdive, mollie importState, err error) {
	if vpdive, err = t.importOf(ctx, imports.Payments); err != nil {
		return importState{}, importState{}, err
	}
	if mollie, err = t.importOf(ctx, imports.Mollie); err != nil {
		return importState{}, importState{}, err
	}
	return vpdive, mollie, nil
}

// importOf reads kind's state and records it as a source.
func (t *toolbox) importOf(ctx context.Context, kind imports.Kind) (importState, error) {
	st, err := t.s.importState(ctx, kind)
	if err != nil {
		return importState{}, err
	}
	return t.importSource(kind, st), nil
}

// importSource records st, the state of kind's import, as a source.
func (t *toolbox) importSource(kind imports.Kind, st importState) importState {
	if !st.Aucun {
		date, _, _ := strings.Cut(st.Recu, " ")
		t.addSource(assistant.Source{Label: importLabel[kind], Date: date, Stale: st.Perime})
	}
	return st
}

// plainSpaces turns the no-break spaces of an amount or a cart into plain ones.
func plainSpaces(s string) string { return strings.ReplaceAll(s, "\u00a0", " ") }

// euros is an amount as the model reads it.
func euros(a payments.Amount) string { return plainSpaces(a.Euros()) }

// --- find_member

// candidate omits what is empty or false: a member's seasons never are, and
// a false homonyme would have the model say « pas d'homonyme ».
type candidate struct {
	Ref          string `json:"ref"                   jsonschema:"Référence de la personne pour le champ « adherent » des autres outils : son adresse mail, ou « campagne:… » pour une personne sans adresse."`
	Nom          string `json:"nom"                   jsonschema:"Prénom puis nom."`
	Saisons      string `json:"saisons,omitzero"      jsonschema:"Saisons d'adhésion que la liste des membres connaît."`
	Licence      string `json:"licence,omitzero"      jsonschema:"Fin de licence selon la liste des membres VPDive, qui n'est pas le registre de la fédération."`
	Organisation string `json:"organisation,omitzero" jsonschema:"Groupes VPDive de la personne ; « aucune », ou « non fournie par l'export »."`
	Source       string `json:"source,omitzero"       jsonschema:"« campagne » pour une personne connue de la seule campagne d'adhésion, sans adresse ; absent pour un membre."`
	Homonyme     bool   `json:"homonyme,omitzero"     jsonschema:"Vrai quand plusieurs personnes portent ce nom : ses paiements, ses sorties et son adhésion ne sont pas lisibles."`
}

// sourceCampaign marks a candidate known by the membership campaign only:
// no address, so no request of theirs can be read.
const sourceCampaign = "campagne"

type findMemberResult struct {
	Import         importState `json:"import_membres" jsonschema:"État de l'import de la liste des membres."`
	Correspondance string      `json:"correspondance" jsonschema:"exacte, partielle ou aucune."`
	Candidats      []candidate `json:"candidats"      jsonschema:"Personnes trouvées, 10 au plus."`
}

func (t *toolbox) findMember(ctx context.Context, input json.RawMessage) (any, string, error) {
	in, ok := decode[struct {
		Query string `json:"query"`
	}](input)
	if !ok || strings.TrimSpace(in.Query) == "" {
		return toolProblem{"query attendu : un nom, un prénom ou [email N]"}, "Recherche d'adhérent refusée", nil
	}
	email, _ := assistant.Placeholder(in.Query, t.c.Emails)
	out, err := t.candidates(ctx, in.Query, email, t.c.AddPerson)
	if err != nil {
		return nil, "", err
	}
	return out, "Recherche « " + strings.TrimSpace(in.Query) + " » : " + plural(len(out.Candidats), "candidat", "candidats"), nil
}

// candidates are the people query names: the member of email when one is
// given, else the members and the campaign people whose name holds its
// words. ref names each of them for the caller's later calls.
func (t *toolbox) candidates(ctx context.Context, query, email string, ref func(assistant.Person) string) (findMemberResult, error) {
	st, err := t.importOf(ctx, imports.Members)
	if err != nil {
		return findMemberResult{}, err
	}
	var matches []members.Match
	byEmail := email != ""
	if byEmail {
		matches, err = t.s.memberOf(ctx, email)
	} else {
		matches, err = t.s.members.Search(ctx, query, maxCandidates)
	}
	if err != nil {
		return findMemberResult{}, err
	}
	var extra []assistant.Person
	if !byEmail { // the campaign holds no address
		if extra, err = t.campaignPeople(ctx, query); err != nil {
			return findMemberResult{}, err
		}
		// Search's rule across both sources: an exact match hides the partial
		// ones, whose names then never reach the conversation.
		if len(extra) > 0 && !slices.ContainsFunc(matches, func(m members.Match) bool { return m.Exact }) {
			matches = nil
		}
	}
	out := findMemberResult{Import: st, Correspondance: "aucune", Candidats: []candidate{}}
	for _, m := range matches {
		out.Correspondance = "partielle"
		if m.Exact {
			out.Correspondance = "exacte"
		}
		out.Candidats = append(out.Candidats, candidateOf(m, ref))
	}
	for _, p := range extra[:min(len(extra), maxCandidates-len(out.Candidats))] {
		out.Candidats = append(out.Candidats, candidate{Ref: ref(p), Nom: p.Name, Source: sourceCampaign, Homonyme: p.Shared > 1})
		out.Correspondance = "exacte"
	}
	return out, nil
}

// memberOf is the member of email (an address the resolver typed, which
// reaches the model as [email N], or a request's), alone in its slice: none
// when that address is not a member's.
func (s *Server) memberOf(ctx context.Context, email string) ([]members.Match, error) {
	p, found, err := s.members.Find(ctx, email)
	if err != nil || !found {
		return nil, err
	}
	n, err := s.members.NameCount(ctx, p.NameHash)
	if err != nil {
		return nil, err
	}
	return []members.Match{{Profile: p, Email: email, Shared: n, Exact: true}}, nil
}

// candidateOf describes m for the model, under the name ref gives them.
func candidateOf(m members.Match, ref func(assistant.Person) string) candidate {
	view := newProfileView(m.Profile)
	name := strings.TrimSpace(m.FirstName + " " + m.LastName)
	orgs := "non fournie par l'export"
	if m.Organisation != nil {
		orgs = cmp.Or(*m.Organisation, "aucune")
	}
	return candidate{Ref: ref(assistant.Person{Name: name, Email: m.Email, NameHash: m.NameHash,
		Seasons: view.Seasons, Licence: view.Licence, Shared: m.Shared}),
		Nom: name, Saisons: view.Seasons, Licence: view.Licence, Organisation: orgs, Homonyme: m.Shared > 1}
}

// campaignPeople are the people of the campaign whose name holds every word
// of query and whom the members list does not name: a new member, before
// the next members import. A name the campaign holds twice is one person, a
// homonym with Shared 2: the tools by name hash then read nothing (design
// 2026-10-20 « Assistant »).
func (t *toolbox) campaignPeople(ctx context.Context, query string) ([]assistant.Person, error) {
	want := members.NameWords(query)
	if len(want) == 0 {
		return nil, nil
	}
	people, err := t.s.campaign.All(ctx) // a few hundred
	if err != nil {
		return nil, err
	}
	var out []assistant.Person
	seen := map[string]bool{}
	for _, m := range people {
		p := m.Line
		if len(out) == maxCandidates {
			break
		}
		if !allIn(want, members.NameWords(p.LastName+" "+p.FirstName)) {
			continue
		}
		hash := t.s.keys.Hash(secure.NameKey(p.LastName, p.FirstName))
		if seen[string(hash)] {
			continue
		}
		seen[string(hash)] = true
		n, err := t.s.members.NameCount(ctx, hash)
		if err != nil {
			return nil, err
		}
		if n > 0 { // the member's candidate stands for them
			continue
		}
		shared := 0
		if m.Ambiguous {
			shared = 2
		}
		out = append(out, assistant.Person{Name: strings.TrimSpace(p.FirstName + " " + p.LastName), NameHash: hash, Shared: shared})
	}
	return out, nil
}

// allIn tells whether every word of want is in words.
func allIn(want, words []string) bool {
	for _, w := range want {
		if !slices.Contains(words, w) {
			return false
		}
	}
	return true
}

// --- shared by the member tools

type memberInput struct {
	Ref string `json:"ref"`
	Du  string `json:"du"`
	Au  string `json:"au"`
}

// member reads a member tool's input: the person and the period, from back
// days before today by default to no end.
func (t *toolbox) member(input json.RawMessage, back int) (assistant.Person, time.Time, time.Time, *toolProblem) {
	in, ok := decode[memberInput](input)
	if !ok {
		return assistant.Person{}, time.Time{}, time.Time{}, &toolProblem{"ref attendu, du et au facultatifs"}
	}
	p, ok := t.c.Person(in.Ref)
	if !ok {
		return assistant.Person{}, time.Time{}, time.Time{}, &toolProblem{"référence inconnue : appelle d'abord find_member"}
	}
	if !slices.Contains(t.read, p.Ref) {
		if len(t.read) == maxPeople {
			return assistant.Person{}, time.Time{}, time.Time{}, &toolProblem{
				"trois personnes lues pour cette question, pas plus : demande au résolveur de préciser de qui il s'agit"}
		}
		t.read = append(t.read, p.Ref)
	}
	from, to, ok := t.s.period(in.Du, in.Au, back)
	if !ok {
		return assistant.Person{}, time.Time{}, time.Time{}, &toolProblem{"dates au format AAAA-MM-JJ, du avant au"}
	}
	return p, from, to, nil
}

// period reads du and au, Paris days: from defaults to back days before
// today, to (exclusive) to none.
func (s *Server) period(du, au string, back int) (from, to time.Time, ok bool) {
	from = midnight(s.now(), s.paris).AddDate(0, 0, -back)
	if du != "" {
		d, err := time.ParseInLocation(time.DateOnly, du, s.paris)
		if err != nil {
			return from, to, false
		}
		from = d
	}
	if au != "" {
		d, err := time.ParseInLocation(time.DateOnly, au, s.paris)
		if err != nil {
			return from, to, false
		}
		to = d.AddDate(0, 0, 1)
	}
	return from, to, to.IsZero() || to.After(from)
}

// periodJSON is the window a member tool read, Paris days, both included:
// what lies outside it was not read, which is not to say it is absent.
type periodJSON struct {
	Du string `json:"du" jsonschema:"Premier jour lu, JJ/MM/AAAA."`
	Au string `json:"au" jsonschema:"Dernier jour lu, inclus, JJ/MM/AAAA ; « sans fin » quand l'appel n'en donnait pas."`
}

// periodRead describes the window [from, to) of period.
func (s *Server) periodRead(from, to time.Time) periodJSON {
	p := periodJSON{Du: s.formatDate(from), Au: "sans fin"}
	if !to.IsZero() {
		p.Au = s.formatDate(to.AddDate(0, 0, -1))
	}
	return p
}

// paymentBlocks reads the VPDive and Mollie blocks of nameHash.
func (s *Server) paymentBlocks(ctx context.Context, nameHash []byte) (payments.Block, payments.MollieBlock, error) {
	pay, err := s.payments.Block(ctx, nameHash)
	if err != nil {
		return payments.Block{}, payments.MollieBlock{}, err
	}
	mol, err := s.mollie.Block(ctx, nameHash)
	return pay, mol, err
}

func within(t, from, to time.Time) bool {
	return !t.Before(from) && (to.IsZero() || t.Before(to))
}

// blockState names a payments block state for the model.
var blockState = map[payments.BlockState]string{
	payments.BlockNoLines: "aucun_import", payments.BlockPurged: "purge", payments.BlockNoMember: "hors_liste",
	payments.BlockAmbiguous: "ambigu", payments.BlockEmpty: "vide", payments.BlockLines: "ok",
}

type lineJSON struct {
	Date         string `json:"date,omitempty"        jsonschema:"Début de la sortie ou de la prestation payée (colonne « Du » de l'export), quand la ligne en a un."`
	Creee        string `json:"creee"                 jsonschema:"Date et heure de création de la ligne dans VPDive."`
	Intitule     string `json:"intitule"              jsonschema:"Produit ou prestation payés."`
	Type         string `json:"type,omitempty"        jsonschema:"Type de produit VPDive : carte, formation, sortie, etc."`
	Etat         string `json:"etat"                  jsonschema:"État de la ligne dans VPDive, tel qu'exporté."`
	Methode      string `json:"methode,omitempty"     jsonschema:"Moyen de paiement."`
	PrixUnitaire string `json:"prix_unitaire"         jsonschema:"Prix unitaire, en euros."`
	Quantite     string `json:"quantite"              jsonschema:"Quantité."`
	MontantPaye  string `json:"montant_paye"          jsonschema:"Montant déjà payé, en euros."`
	Reduction    string `json:"reduction,omitempty"   jsonschema:"Réduction appliquée, en euros, quand il y en a une."`
	PayeLe       string `json:"paye_le,omitempty"     jsonschema:"Date et heure du paiement, quand la ligne est payée."`
	Commentaire  string `json:"commentaire,omitempty" jsonschema:"Commentaire saisi dans VPDive sur la ligne."`
}

func (s *Server) lineJSON(l payments.Line) lineJSON {
	j := lineJSON{Date: s.formatTime(l.Starts), Creee: s.formatTime(l.Created), Intitule: l.Product, Type: l.ProductType,
		Etat: l.State, Methode: methodLabel(l.Method), PrixUnitaire: euros(l.UnitPrice), Quantite: l.Quantity.Number(),
		MontantPaye: euros(l.Paid), PayeLe: s.formatTime(l.PaidAt), Commentaire: l.Comment}
	if l.Discount != 0 {
		j.Reduction = euros(l.Discount)
	}
	return j
}

type mollieJSON struct {
	PayeLe      string `json:"paye_le"              jsonschema:"Date et heure de l'encaissement en ligne."`
	Produit     string `json:"produit"              jsonschema:"Produit VPDive payé."`
	Prestation  string `json:"prestation,omitempty" jsonschema:"Sortie ou location que la ligne paie."`
	Date        string `json:"date,omitempty"       jsonschema:"Date de la prestation, JJ/MM/AAAA."`
	Montant     string `json:"montant"              jsonschema:"Montant encaissé, en euros."`
	SoldeVPDive string `json:"solde_dans_vpdive"    jsonschema:"Ce que VPDive dit du solde de la ligne : Oui, Non, ou « vide » quand l'export ne dit rien."`
	Methode     string `json:"methode,omitempty"    jsonschema:"Moyen de paiement en ligne."`
}

func (s *Server) mollieJSON(l payments.MollieLine) mollieJSON {
	return mollieJSON{PayeLe: s.formatTime(l.PaidAt), Produit: l.Product, Prestation: l.Service, Date: s.formatDate(l.Starts),
		Montant: euros(l.Amount), SoldeVPDive: cmp.Or(l.Settled, "vide"), Methode: l.Method}
}

// --- member_payments

type paymentsResult struct {
	Import        importState  `json:"import_vpdive"            jsonschema:"État de l'import des paiements VPDive."`
	Etat          string       `json:"etat"                     jsonschema:"État des lignes VPDive : ok, vide, hors_liste, ambigu, purge ou aucun_import."`
	Periode       periodJSON   `json:"periode_lue"              jsonschema:"Période des lignes lues ; les soldes et les cartes viennent toujours, quelle que soit la période."`
	Soldes        []lineJSON   `json:"soldes"                   jsonschema:"Soldes de carnet et de formation de la personne."`
	Lignes        []lineJSON   `json:"lignes"                   jsonschema:"Lignes de paiement VPDive de la période, plus tous les achats de carte et de formation ; 100 au plus."`
	Tronque       bool         `json:"tronque,omitempty"        jsonschema:"Vrai quand il y avait plus de 100 lignes : resserre la période."`
	ImportMollie  importState  `json:"import_mollie"            jsonschema:"État de l'import des encaissements Mollie."`
	EtatMollie    string       `json:"etat_mollie"              jsonschema:"État des encaissements Mollie : mêmes valeurs que etat."`
	Mollie        []mollieJSON `json:"mollie"                   jsonschema:"Encaissements en ligne de la période, 100 au plus."`
	MollieTronque bool         `json:"mollie_tronque,omitempty" jsonschema:"Vrai quand il y avait plus de 100 encaissements : resserre la période."`
	ImportCartes  importState  `json:"import_cartes"            jsonschema:"État de l'import des cartes de plongées."`
	EtatCartes    string       `json:"etat_cartes"              jsonschema:"État des cartes : mêmes valeurs que etat."`
	Cartes        []carteJSON  `json:"cartes"                   jsonschema:"Toutes les cartes de plongées de la personne, quelle que soit la période."`
}

func (t *toolbox) memberPayments(ctx context.Context, input json.RawMessage) (any, string, error) {
	p, from, to, problem := t.member(input, paymentsBack)
	if problem != nil {
		return *problem, "Lecture de paiements refusée", nil
	}
	out, err := t.payments(ctx, p, from, to)
	if err != nil {
		return nil, "", err
	}
	return out, "Paiements de " + p.Name + " lus", nil
}

// payments reads the lines of p between from and to, with their balances
// and every card.
func (t *toolbox) payments(ctx context.Context, p assistant.Person, from, to time.Time) (paymentsResult, error) {
	if p.Shared > 1 { // a campaign homonym, whom no members count marks
		return t.ambiguousPayments(ctx, from, to)
	}
	pay, mol, err := t.s.paymentBlocks(ctx, p.NameHash)
	if err != nil {
		return paymentsResult{}, err
	}
	out := paymentsResult{Etat: blockState[pay.State], EtatMollie: blockState[mol.State], Periode: t.s.periodRead(from, to),
		Soldes: []lineJSON{}, Lignes: []lineJSON{}, Mollie: []mollieJSON{}}
	if out.Import, out.ImportMollie, err = t.paymentImports(ctx); err != nil {
		return paymentsResult{}, err
	}
	for _, l := range pay.Balances {
		out.Soldes = append(out.Soldes, t.s.lineJSON(l))
	}
	for _, l := range pay.Lines {
		card := l.ProductType == payments.TypeCard || l.ProductType == payments.TypeTraining
		if !card && !within(cmp.Or(l.Starts, l.Created), from, to) {
			continue
		}
		if len(out.Lignes) == maxLines {
			out.Tronque = true
			break
		}
		out.Lignes = append(out.Lignes, t.s.lineJSON(l))
	}
mollie:
	for _, m := range mol.Payments {
		for _, l := range m.Lines {
			if !within(l.PaidAt, from, to) {
				continue
			}
			if len(out.Mollie) == maxLines {
				out.MollieTronque = true
				break mollie
			}
			out.Mollie = append(out.Mollie, t.s.mollieJSON(l.MollieLine))
		}
	}
	cards, err := t.s.carnets.Block(ctx, p.NameHash)
	if err != nil {
		return paymentsResult{}, err
	}
	out.ImportCartes = t.importSource(imports.Carnets, t.s.stateOf(cards.Import, cards.Import.ID != 0))
	out.EtatCartes, out.Cartes = blockState[cards.State], make([]carteJSON, 0, len(cards.Cards))
	for _, v := range cards.Cards {
		out.Cartes = append(out.Cartes, t.s.carteJSON(v))
	}
	return out, nil
}

// ambiguousPayments answers a homonym without reading any line.
func (t *toolbox) ambiguousPayments(ctx context.Context, from, to time.Time) (paymentsResult, error) {
	amb := blockState[payments.BlockAmbiguous]
	out := paymentsResult{Etat: amb, EtatMollie: amb, EtatCartes: amb, Periode: t.s.periodRead(from, to),
		Soldes: []lineJSON{}, Lignes: []lineJSON{}, Mollie: []mollieJSON{}, Cartes: []carteJSON{}}
	var err error
	if out.Import, out.ImportMollie, err = t.paymentImports(ctx); err != nil {
		return paymentsResult{}, err
	}
	if out.ImportCartes, err = t.importOf(ctx, imports.Carnets); err != nil {
		return paymentsResult{}, err
	}
	return out, nil
}

// carteJSON is a card as the model reads it (design 2026-10-09 §7).
type carteJSON struct {
	Produit             string      `json:"produit"                   jsonschema:"Nom de la carte."`
	Etat                string      `json:"etat,omitempty"            jsonschema:"État de la carte dans VPDive, tel que reçu."`
	SoldeVPDive         string      `json:"solde_vpdive"              jsonschema:"Solde affiché par VPDive, en euros : négatif tant qu'il reste du crédit."`
	DebiteNet           string      `json:"debite_net"                jsonschema:"Total débité moins total recrédité, en euros."`
	Debits              int         `json:"debits"                    jsonschema:"Nombre de plongées débitées."`
	Recredits           int         `json:"recredits"                 jsonschema:"Nombre de plongées recréditées."`
	MontantsInhabituels int         `json:"montants_inhabituels"      jsonschema:"Nombre de lignes dont le montant diffère du prix habituel d'une plongée."`
	LignesNonLues       int         `json:"lignes_non_lues"           jsonschema:"Nombre de lignes de l'historique que l'outil n'a pas su lire."`
	TotauxPartiels      bool        `json:"totaux_partiels,omitempty" jsonschema:"Vrai quand des lignes non lues rendent les totaux incomplets."`
	Historique          []entryJSON `json:"historique"                jsonschema:"Lignes de l'historique de la carte."`
}

// entryJSON is a line of a card's history, absent fields omitted.
type entryJSON struct {
	Le         string `json:"le,omitempty"          jsonschema:"Date et heure de la ligne."`
	Par        string `json:"par,omitempty"         jsonschema:"Auteur de la ligne dans VPDive."`
	Action     string `json:"action"                jsonschema:"débit, recrédit, changement de prix, commentaire, ou l'action telle que VPDive la nomme."`
	Sortie     string `json:"sortie,omitempty"      jsonschema:"Sortie débitée ou recréditée."`
	DateSortie string `json:"date_sortie,omitempty" jsonschema:"Date de cette sortie."`
	Montant    string `json:"montant,omitempty"     jsonschema:"Montant de la ligne, en euros."`
	Repartie   string `json:"repartie,omitempty"    jsonschema:"Plongée payée sur deux cartes : la somme de ses deux parts, en euros."`
	Inhabituel bool   `json:"inhabituel,omitempty"  jsonschema:"Vrai quand le montant diffère du prix habituel d'une plongée."`
	Texte      string `json:"texte,omitempty"       jsonschema:"Changement de prix (ancien -> nouveau), ou texte du commentaire."`
}

// entryAction names a line's kind for the model; other lines keep VPDive's
// action.
var entryAction = map[carnets.Kind]string{
	carnets.KindDebit: "débit", carnets.KindRecredit: "recrédit", carnets.KindPrice: "changement de prix", carnets.KindComment: "commentaire",
}

func (s *Server) carteJSON(v carnets.View) carteJSON {
	c := carteJSON{Produit: v.Title, Etat: v.Status, SoldeVPDive: euros(v.Amount), DebiteNet: euros(v.Net()), Debits: v.Debits,
		Recredits: v.Recredits, MontantsInhabituels: v.Unusual, LignesNonLues: v.Unread, TotauxPartiels: v.Partial(),
		Historique: make([]entryJSON, 0, len(v.Lines))}
	for _, l := range v.Lines {
		e := entryJSON{Le: s.formatTime(l.Time), Par: l.By, Action: cmp.Or(entryAction[l.Kind], l.Action), Inhabituel: l.Unusual}
		switch l.Kind {
		case carnets.KindDebit, carnets.KindRecredit:
			e.Sortie, e.DateSortie, e.Montant = l.Outing, l.Date, euros(l.Amount)
			if l.Split != 0 {
				e.Repartie = euros(l.Split)
			}
		case carnets.KindPrice:
			e.Texte = euros(l.From) + " -> " + euros(l.To)
		case carnets.KindComment, carnets.KindOther:
			e.Texte = l.Detail
		}
		c.Historique = append(c.Historique, e)
	}
	return c
}

// --- member_outings

type unregJSON struct {
	Nom string `json:"nom,omitempty" jsonschema:"Personne désinscrite ; absent quand c'est la personne demandée."`
	Le  string `json:"le"            jsonschema:"Date et heure de la désinscription."`
	Par string `json:"par,omitempty" jsonschema:"Qui a désinscrit."`
}

type outingJSON struct {
	ID              string       `json:"id"                          jsonschema:"Identifiant de la sortie, à passer à comite_get_outing."`
	Titre           string       `json:"titre"                       jsonschema:"Titre de la sortie."`
	Debut           string       `json:"debut"                       jsonschema:"Date et heure de début."`
	Annulee         bool         `json:"annulee,omitempty"           jsonschema:"Vrai quand la sortie est annulée."`
	Participation   string       `json:"participation,omitempty"     jsonschema:"inscrit, liste_attente ou non_inscrit ; absent quand la personne n'est plus parmi les participants."`
	Personnes       string       `json:"personnes,omitempty"         jsonschema:"Nombre de places de l'inscription quand elle en compte plusieurs, invités compris."`
	InscritInvite   bool         `json:"inscrit_en_invite,omitempty" jsonschema:"Vrai quand la personne est inscrite comme invitée d'un autre membre."`
	Roles           string       `json:"roles,omitempty"             jsonschema:"Rôles de la personne sur la sortie."`
	Panier          string       `json:"panier,omitempty"            jsonschema:"État du panier de l'inscription : ce qui est payé et ce qui reste dû."`
	Desinscriptions []unregJSON  `json:"desinscriptions,omitempty"   jsonschema:"Désinscriptions de la personne pour cette sortie."`
	LignesVPDive    []lineJSON   `json:"lignes_vpdive,omitempty"     jsonschema:"Lignes de paiement VPDive rattachées à la sortie."`
	LignesMollie    []mollieJSON `json:"lignes_mollie,omitempty"     jsonschema:"Encaissements Mollie rattachés à la sortie."`
	Signaux         []string     `json:"signaux,omitempty"           jsonschema:"Incohérences repérées entre l'inscription et les paiements, en clair."`
}

type outingsResult struct {
	Import       importState  `json:"import_calendrier" jsonschema:"État de l'import du calendrier."`
	ImportVPDive importState  `json:"import_vpdive"     jsonschema:"État de l'import des paiements VPDive : les lignes et les signaux des sorties en viennent."`
	ImportMollie importState  `json:"import_mollie"     jsonschema:"État de l'import des encaissements Mollie."`
	Etat         string       `json:"etat"              jsonschema:"ok, vide, hors_liste, ambigu (homonyme) ou aucun_calendrier."`
	Periode      periodJSON   `json:"periode_lue"       jsonschema:"Période des sorties lues."`
	Sorties      []outingJSON `json:"sorties"           jsonschema:"Sorties de la personne sur la période, 50 au plus."`
	Tronque      bool         `json:"tronque,omitempty" jsonschema:"Vrai quand il y avait plus de 50 sorties : resserre la période."`
}

var outingsStateName = map[outingsState]string{
	outingsNoCalendar: "aucun_calendrier", outingsNoMember: "hors_liste", outingsAmbiguous: "ambigu",
	outingsEmpty: "vide", outingsList: "ok",
}

// places is the head count of a registration of several places: the person
// registered and the guests their cart covers. Nothing for one place.
func places(people int) string {
	if people < 2 {
		return ""
	}
	return strconv.Itoa(people) + " (" + plural(people-1, "invité", "invités") + ")"
}

func participation(p calendar.Participant) string {
	switch {
	case !p.Registered:
		return "non_inscrit"
	case p.WaitingList:
		return "liste_attente"
	}
	return "inscrit"
}

func (s *Server) outingJSON(o outing) outingJSON {
	j := outingJSON{ID: o.Event.ID, Titre: o.Event.Title, Debut: eventWhen(o.Event, s.paris), Annulee: o.Event.Cancelled(), Signaux: o.Signals()}
	if p := o.Participant; p != nil {
		j.Participation, j.Personnes, j.InscritInvite = participation(*p), places(p.People), p.Guest
		j.Roles, j.Panier = s.labels.roles(p.Roles), plainSpaces(cartText(p.Payment))
	}
	for _, u := range o.Unregistrations {
		j.Desinscriptions = append(j.Desinscriptions, unregJSON{Le: s.formatTime(u.Time), Par: u.By})
	}
	for _, l := range o.Lines {
		j.LignesVPDive = append(j.LignesVPDive, s.lineJSON(l))
	}
	for _, l := range o.Mollie {
		j.LignesMollie = append(j.LignesMollie, s.mollieJSON(l.MollieLine))
	}
	return j
}

// outingsOf is the outings block of p; a campaign homonym, whom no members
// count marks, reads nothing.
func (t *toolbox) outingsOf(ctx context.Context, p assistant.Person) (outingsBlock, error) {
	if p.Shared > 1 {
		return outingsBlock{State: outingsAmbiguous}, nil
	}
	pay, mol, err := t.s.paymentBlocks(ctx, p.NameHash)
	if err != nil {
		return outingsBlock{}, err
	}
	return t.s.outingsBlock(ctx, t.s.now(), members.Profile{NameHash: p.NameHash}, true, pay, mol)
}

func (t *toolbox) memberOutings(ctx context.Context, input json.RawMessage) (any, string, error) {
	p, from, to, problem := t.member(input, outingsBack)
	if problem != nil {
		return *problem, "Lecture de sorties refusée", nil
	}
	out, err := t.outings(ctx, p, from, to)
	if err != nil {
		return nil, "", err
	}
	return out, "Sorties de " + p.Name + " lues", nil
}

// outings reads the outings of p that overlap [from, to).
func (t *toolbox) outings(ctx context.Context, p assistant.Person, from, to time.Time) (outingsResult, error) {
	b, err := t.outingsOf(ctx, p)
	if err != nil {
		return outingsResult{}, err
	}
	out := outingsResult{Etat: outingsStateName[b.State], Periode: t.s.periodRead(from, to), Sorties: []outingJSON{}}
	if out.Import, err = t.importOf(ctx, imports.Calendar); err != nil {
		return outingsResult{}, err
	}
	if out.ImportVPDive, out.ImportMollie, err = t.paymentImports(ctx); err != nil {
		return outingsResult{}, err
	}
	for _, o := range append(b.Recent, b.Older...) {
		if o.Event.Until().Before(from) || (!to.IsZero() && !o.Event.Start.Before(to)) {
			continue
		}
		if len(out.Sorties) == maxOutings {
			out.Tronque = true
			break
		}
		out.Sorties = append(out.Sorties, t.s.outingJSON(o))
	}
	return out, nil
}

// --- member_requests

type requestJSON struct {
	Ref       string `json:"ref"`
	Categorie string `json:"categorie"`
	Statut    string `json:"statut"`
	Deposee   string `json:"deposee_le"`
	Resume    string `json:"resume"`
}

type requestsResult struct {
	Demandes []requestJSON `json:"demandes"`
	Tronque  bool          `json:"tronque,omitempty"`
	Note     string        `json:"note"`
}

func (t *toolbox) memberRequests(ctx context.Context, input json.RawMessage) (any, string, error) {
	p, _, _, problem := t.member(input, 0)
	if problem != nil {
		return *problem, "Lecture de demandes refusée", nil
	}
	if p.Email == "" { // a result, not an error: an error ends the whole answer
		return requestsResult{Demandes: []requestJSON{}, Note: "pas d'adresse : personne connue par la campagne seulement"},
			"Demandes de " + p.Name + " : pas d'adresse", nil
	}
	rows, err := t.s.tickets.ByEmail(ctx, p.Email)
	if err != nil {
		return nil, "", err
	}
	out := requestsResult{Demandes: []requestJSON{},
		Note: "Seules les demandes déposées dans cet outil sont connues : SMS, mails et messages VPDive n'y sont pas."}
	for _, r := range rows {
		if len(out.Demandes) == maxLines {
			out.Tronque = true
			break
		}
		out.Demandes = append(out.Demandes, requestJSON{Ref: r.Ref, Categorie: t.s.tickets.Catalog.CategoryLabel(r.Category),
			Statut: r.Status.Label(), Deposee: t.s.formatDate(r.SubmittedAt), Resume: cmp.Or(r.Summary, r.Excerpt)})
		t.addSource(assistant.Source{Label: r.Ref, Link: "/demandes/" + strconv.FormatInt(r.ID, 10)})
	}
	return out, "Demandes de " + p.Name + " lues", nil
}

// --- member_membership

// campaignEntryJSON is an entry of the « Adhésion » or « License » cell.
type campaignEntryJSON struct {
	Libelle   string `json:"libelle,omitempty"    jsonschema:"Intitulé de l'adhésion ou de la licence."`
	Etat      string `json:"etat,omitempty"       jsonschema:"Réglé, Au panier, ou « état inconnu : … »."`
	Montant   string `json:"montant,omitempty"    jsonschema:"Montant, en euros."`
	Moyen     string `json:"moyen,omitempty"      jsonschema:"Moyen de paiement, connu pour un paiement saisi à la main ou passé par VPayDive."`
	Le        string `json:"le,omitempty"         jsonschema:"Date du paiement, quand elle est connue."`
	AVerifier string `json:"a_verifier,omitempty" jsonschema:"Entrée que l'outil n'a pas su lire, telle que reçue."`
}

// membershipJSON is a campaign line, with the fields of the request block.
type membershipJSON struct {
	Nom        string              `json:"nom"                 jsonschema:"Prénom puis nom."`
	Statut     string              `json:"statut"              jsonschema:"Statut du dossier dans VPDive : En cours ou Validé."`
	Validation string              `json:"validation"          jsonschema:"Quand et par qui le dossier a été validé, ou « Pas encore validé »."`
	Adhesion   []campaignEntryJSON `json:"adhesion"            jsonschema:"Entrées d'adhésion."`
	Licence    []campaignEntryJSON `json:"licence"             jsonschema:"Entrées de licence : ce qui a été payé dans VPDive, pas la validité à la fédération."`
	Reglement  string              `json:"reglement,omitempty" jsonschema:"Règlement, tel que l'export le résume."`
}

type membershipResult struct {
	Import   importState      `json:"import_campagne"     jsonschema:"État de l'import de la campagne d'adhésion."`
	ExportDu string           `json:"export_du,omitempty" jsonschema:"Date et heure de l'export VPDive : ce qui est plus récent n'y figure pas."`
	Etat     string           `json:"etat"                jsonschema:"ok, vide, hors_liste, purge, aucun_import, ou homonyme (nom porté par plusieurs personnes : aucun détail)."`
	Lignes   []membershipJSON `json:"lignes"              jsonschema:"Lignes de la personne dans la campagne."`
}

func (t *toolbox) memberMembership(ctx context.Context, input json.RawMessage) (any, string, error) {
	p, _, _, problem := t.member(input, 0)
	if problem != nil {
		return *problem, "Lecture de campagne refusée", nil
	}
	out, err := t.membership(ctx, p)
	if err != nil {
		return nil, "", err
	}
	return out, "Campagne d'adhésion de " + p.Name + " lue", nil
}

// membership reads the campaign lines of p: none in detail for a homonym.
func (t *toolbox) membership(ctx context.Context, p assistant.Person) (membershipResult, error) {
	state, info, people, err := t.s.campaign.NameLines(ctx, p.NameHash)
	if err != nil {
		return membershipResult{}, err
	}
	out := membershipResult{Import: t.importSource(imports.Campaign, t.s.stateOf(info, info.ID != 0)),
		ExportDu: t.s.formatTime(info.ExportedAt), Etat: blockState[state], Lignes: []membershipJSON{}}
	if state == payments.BlockAmbiguous {
		out.Etat = "homonyme"
	}
	for _, l := range people {
		j := membershipJSON{Nom: strings.TrimSpace(l.FirstName + " " + l.LastName), Statut: l.Status, Validation: "Pas encore validé",
			Adhesion: t.s.entriesJSON(l.Membership), Licence: t.s.entriesJSON(l.Licence), Reglement: l.Settlement}
		if l.Status == campaign.StatusValidated {
			j.Validation = t.s.validation(l)
		}
		out.Lignes = append(out.Lignes, j)
	}
	return out, nil
}

func (s *Server) entriesJSON(entries []campaign.Entry) []campaignEntryJSON {
	out := make([]campaignEntryJSON, len(entries))
	for i, e := range entries {
		if e.Unreadable() {
			out[i] = campaignEntryJSON{AVerifier: e.Raw}
			continue
		}
		out[i] = campaignEntryJSON{Libelle: e.Label, Etat: stateText(e.State), Montant: euros(e.Amount), Moyen: e.Method, Le: s.formatDate(e.PaidOn)}
	}
	return out
}

// --- find_outings and outing

type eventJSON struct {
	ID           string `json:"id"                      jsonschema:"Identifiant de la sortie, à passer à comite_get_outing."`
	Titre        string `json:"titre"                   jsonschema:"Titre de la sortie."`
	Debut        string `json:"debut"                   jsonschema:"Date et heure de début."`
	Categorie    string `json:"categorie,omitempty"     jsonschema:"Catégorie du calendrier."`
	Activite     string `json:"activite,omitempty"      jsonschema:"Activité."`
	Annulee      bool   `json:"annulee,omitempty"       jsonschema:"Vrai quand la sortie est annulée."`
	Inscrits     int    `json:"inscrits"                jsonschema:"Personnes inscrites, invités compris."`
	ListeAttente int    `json:"liste_attente,omitempty" jsonschema:"Personnes en liste d'attente, invités compris."`
}

// eventJSON describes ev with its head count: the people registered, and
// those on the waiting list.
func (s *Server) eventJSON(ev calendar.Event) eventJSON {
	j := eventJSON{ID: ev.ID, Titre: ev.Title, Debut: eventWhen(ev, s.paris), Categorie: s.labels.category(ev.Category).Label,
		Activite: label(s.labels.Activities, ev.Activity), Annulee: ev.Cancelled()}
	for _, p := range ev.Participants {
		switch {
		case p.Registered && p.WaitingList:
			j.ListeAttente += max(1, p.People)
		case p.Registered:
			j.Inscrits += max(1, p.People)
		}
	}
	return j
}

type eventsResult struct {
	Import  importState `json:"import_calendrier" jsonschema:"État de l'import du calendrier."`
	Sorties []eventJSON `json:"sorties"           jsonschema:"Sorties de la période, 50 au plus."`
	Tronque bool        `json:"tronque,omitempty" jsonschema:"Vrai quand il y avait plus de 50 sorties : resserre la période ou précise le texte."`
}

func (t *toolbox) findOutings(ctx context.Context, input json.RawMessage) (any, string, error) {
	in, ok := decode[struct {
		Du    string `json:"du"`
		Au    string `json:"au"`
		Texte string `json:"texte"`
	}](input)
	if !ok {
		return toolProblem{"du et au attendus, au format AAAA-MM-JJ"}, findOutingsRefused, nil
	}
	from, to, problem := t.s.outingsPeriod(in.Du, in.Au)
	if problem != nil {
		return *problem, findOutingsRefused, nil
	}
	out, err := t.events(ctx, from, to, in.Texte)
	if err != nil {
		return nil, "", err
	}
	return out, "Recherche des sorties du " + t.s.formatDate(from) + " : " + plural(len(out.Sorties), "sortie", "sorties"), nil
}

// outingsPeriod reads the window of a calendar search: both days given, 62
// days at most.
func (s *Server) outingsPeriod(du, au string) (from, to time.Time, problem *toolProblem) {
	if du == "" || au == "" {
		return from, to, &toolProblem{"du et au attendus, au format AAAA-MM-JJ"}
	}
	from, to, ok := s.period(du, au, 0)
	if !ok {
		return from, to, &toolProblem{"dates au format AAAA-MM-JJ, du avant au"}
	}
	if to.Sub(from) > maxOutingsRange*24*time.Hour+time.Hour { // an hour of slack for a time change
		return from, to, &toolProblem{"62 jours au plus entre du et au"}
	}
	return from, to, nil
}

// events are the calendar's outings of [from, to) whose title holds texte.
func (t *toolbox) events(ctx context.Context, from, to time.Time, texte string) (eventsResult, error) {
	events, err := t.s.calendar.Range(ctx, from, to, true)
	if err != nil {
		return eventsResult{}, err
	}
	out := eventsResult{Sorties: []eventJSON{}}
	if out.Import, err = t.importOf(ctx, imports.Calendar); err != nil {
		return eventsResult{}, err
	}
	text := normTitle(texte)
	for _, ev := range events {
		if text != "" && !strings.Contains(normTitle(ev.Title), text) {
			continue
		}
		if len(out.Sorties) == maxOutings {
			out.Tronque = true
			break
		}
		out.Sorties = append(out.Sorties, t.s.eventJSON(ev))
	}
	return out, nil
}

type participantJSON struct {
	Nom           string `json:"nom"                         jsonschema:"Nom du participant, tel que le calendrier le donne."`
	Participation string `json:"participation"               jsonschema:"inscrit, liste_attente ou non_inscrit."`
	Personnes     string `json:"personnes,omitempty"         jsonschema:"Nombre de places de l'inscription quand elle en compte plusieurs, invités compris."`
	InscritInvite bool   `json:"inscrit_en_invite,omitempty" jsonschema:"Vrai quand la personne est inscrite comme invitée d'un autre membre."`
	Roles         string `json:"roles,omitempty"             jsonschema:"Rôles sur la sortie."`
	Panier        string `json:"panier"                      jsonschema:"État du panier de l'inscription : ce qui est payé et ce qui reste dû."`
	Homonyme      bool   `json:"homonyme,omitempty"          jsonschema:"Vrai quand plusieurs membres portent ce nom."`
}

type outingResult struct {
	Import          importState       `json:"import_calendrier" jsonschema:"État de l'import du calendrier."`
	Sortie          eventJSON         `json:"sortie"            jsonschema:"La sortie."`
	Participants    []participantJSON `json:"participants"      jsonschema:"Participants de la sortie."`
	Desinscriptions []unregJSON       `json:"desinscriptions"   jsonschema:"Désinscriptions de la sortie."`
}

func (t *toolbox) outing(ctx context.Context, input json.RawMessage) (any, string, error) {
	in, ok := decode[struct {
		ID string `json:"id"`
	}](input)
	if !ok || in.ID == "" {
		return toolProblem{"id attendu"}, "Lecture de sortie refusée", nil
	}
	out, err := t.outingByID(ctx, in.ID)
	if errors.Is(err, calendar.ErrNotFound) {
		return toolProblem{"sortie inconnue : cherche-la avec find_outings"}, "Sortie inconnue", nil
	}
	if err != nil {
		return nil, "", err
	}
	return out, "Sortie « " + out.Sortie.Titre + " » lue", nil
}

// outingByID reads one outing with its participants and unregistrations;
// calendar.ErrNotFound when the calendar holds no such id.
func (t *toolbox) outingByID(ctx context.Context, id string) (outingResult, error) {
	o, err := t.s.calendar.Outing(ctx, id) // one snapshot: a push in between never mixes two exports
	if err != nil {
		return outingResult{}, err
	}
	out := outingResult{Participants: []participantJSON{}, Desinscriptions: []unregJSON{},
		Sortie: t.s.eventJSON(o.Event), Import: t.importSource(imports.Calendar, t.s.stateOf(o.Import, o.Imported))}
	for _, p := range o.Participants {
		out.Participants = append(out.Participants, participantJSON{Nom: p.Name, Participation: participation(p), Personnes: places(p.People),
			InscritInvite: p.Guest, Roles: t.s.labels.roles(p.Roles), Panier: plainSpaces(cartText(p.Payment)), Homonyme: p.Members > 1})
	}
	for _, u := range o.Unregistrations {
		out.Desinscriptions = append(out.Desinscriptions, unregJSON{Nom: strings.TrimSpace(u.FirstName + " " + u.LastName),
			Le: t.s.formatTime(u.Time), Par: u.By})
	}
	t.addSource(assistant.Source{Label: o.Title, Link: "/calendrier/" + o.ID})
	return out, nil
}

// --- cancellations

type cancelledJSON struct {
	Titre     string `json:"titre"                    jsonschema:"Titre de la sortie annulée."`
	Date      string `json:"date,omitempty"           jsonschema:"Date de la sortie, JJ/MM/AAAA."`
	Payeurs   int    `json:"payeurs"                  jsonschema:"Nombre de personnes qui ont payé."`
	Lignes    int    `json:"lignes"                   jsonschema:"Nombre de lignes payées à supprimer dans VPDive."`
	ParCarnet string `json:"a_recrediter_sur_carnets" jsonschema:"Total payé par carnet, à recréditer, en euros."`
	EnArgent  string `json:"paye_en_argent"           jsonschema:"Total payé en argent, en euros."`
}

type cancellationsResult struct {
	Import  importState     `json:"import_vpdive" jsonschema:"État de l'import des paiements VPDive."`
	Sorties []cancelledJSON `json:"sorties"       jsonschema:"Sorties annulées à régulariser."`
}

func (t *toolbox) cancellations(ctx context.Context) (any, string, error) {
	out, err := t.cancelled(ctx)
	if err != nil {
		return nil, "", err
	}
	return out, "Sorties annulées lues", nil
}

// cancelled are the cancelled outings whose paid lines await removal.
func (t *toolbox) cancelled(ctx context.Context) (cancellationsResult, error) {
	c, err := t.s.payments.Cancellations(ctx)
	if err != nil {
		return cancellationsResult{}, err
	}
	out := cancellationsResult{Sorties: []cancelledJSON{}}
	if out.Import, err = t.importOf(ctx, imports.Payments); err != nil {
		return cancellationsResult{}, err
	}
	for _, o := range c.Outings {
		out.Sorties = append(out.Sorties, cancelledJSON{Titre: o.Title, Date: t.s.formatDate(o.Starts), Payeurs: o.Persons,
			Lignes: o.Lines, ParCarnet: euros(o.ByCarnet), EnArgent: euros(o.ByMoney)})
	}
	t.addSource(assistant.Source{Label: "Annulations", Link: "/annulations"})
	return out, nil
}

// --- read_fiche

type linkJSON struct {
	Libelle string `json:"libelle"`
	URL     string `json:"url"`
}

type ficheResult struct {
	ID              string     `json:"id"`
	Titre           string     `json:"titre"`
	ReponseAdherent string     `json:"reponse_adherent"`
	Procedure       string     `json:"procedure"`
	Liens           []linkJSON `json:"liens"`
}

func (t *toolbox) readFiche(input json.RawMessage) (any, string) {
	in, ok := decode[struct {
		ID string `json:"id"`
	}](input)
	f, found := t.s.kb.Get(in.ID)
	if !ok || !found {
		return toolProblem{"fiche inconnue : choisis un identifiant de la liste"}, "Fiche inconnue"
	}
	out := ficheResult{ID: f.ID, Titre: f.Title, ReponseAdherent: f.AnswerText, Procedure: f.ProcedureText, Liens: []linkJSON{}}
	for _, key := range f.Links {
		if l, ok := t.s.vpdive[key]; ok {
			out.Liens = append(out.Liens, linkJSON{Libelle: l.Label, URL: l.URL})
		}
	}
	t.addSource(assistant.Source{Label: "Fiche « " + f.Title + " »", Link: "/fiches#" + f.ID})
	return out, "Fiche « " + f.Title + " » lue"
}

var licenceEtat = map[licenceState]string{
	licenceNone: "introuvable", licenceValid: "valide", licenceExpired: "non valide", licenceSeveral: "plusieurs",
}

// licenceNumber is the form of a licence number: A-13-634166.
var licenceNumber = regexp.MustCompile(`^[A-Za-z]{1,2}-\d{2}-\d+$`)

type licenceInput struct {
	Ref    string `json:"ref"`
	Numero string `json:"numero"`
}

type licenceJSON struct {
	Etat           string `json:"etat"                       jsonschema:"valide, non valide, introuvable, ou plusieurs (plusieurs licences correspondent)."`
	ValableJusquAu string `json:"valable_jusqu_au,omitempty" jsonschema:"Dernier jour de validité, JJ/MM/AAAA, quand une seule licence correspond."`
}

// licenceStatus reads the federation's register for one person, or one
// licence number: the verdict and the last day, no contact detail.
func (t *toolbox) licenceStatus(ctx context.Context, input json.RawMessage) (any, string, error) {
	if t.s.licences == nil {
		return toolProblem{"registre de la FFESSM non configuré"}, licenceRefused, nil
	}
	in, ok := decode[licenceInput](input)
	if !ok || (in.Ref == "") == (in.Numero == "") {
		return toolProblem{"ref ou numero attendu, un seul des deux"}, licenceRefused, nil
	}
	var (
		person *assistant.Person
		who    = "licence " + in.Numero
	)
	if in.Ref != "" {
		p, _, _, problem := t.member(input, 0)
		if problem != nil {
			return *problem, licenceRefused, nil
		}
		person, who = &p, p.Name
	}
	out, err := t.licence(ctx, person, in.Numero)
	switch {
	case errors.Is(err, errLicenceNoEmail):
		return toolProblem{"pas d'adresse mail pour cette personne : demande son numéro de licence au résolveur"}, licenceRefused, nil
	case errors.Is(err, errLicenceNumber):
		return toolProblem{"numéro de licence attendu, sous la forme A-13-634166"}, licenceRefused, nil
	case errors.Is(err, errLicenceUnreachable):
		return toolProblem{"registre de la FFESSM injoignable, dis-le au résolveur"}, "Registre FFESSM injoignable", nil
	case err != nil:
		return nil, "", err
	}
	return out, "Licence FFESSM de " + who + " lue", nil
}

// What a licence lookup cannot answer, for its caller to put in words.
var (
	errLicenceNoEmail     = errors.New("no address to search the register by")
	errLicenceNumber      = errors.New("not a licence number")
	errLicenceUnreachable = errors.New("register unreachable")
)

// licence reads the register for p, by their address, or for a licence
// number when p is nil.
func (t *toolbox) licence(ctx context.Context, p *assistant.Person, numero string) (licenceJSON, error) {
	var (
		rows []licenceRow
		err  error
	)
	switch {
	case p == nil && !licenceNumber.MatchString(numero): // a name here would find homonyms
		return licenceJSON{}, errLicenceNumber
	case p == nil:
		rows, err = t.s.licenceRows(ctx, numero)
	case p.Email == "":
		return licenceJSON{}, errLicenceNoEmail
	default:
		rows, err = t.s.licenceOf(ctx, p.Email, p.Name)
	}
	if err != nil {
		if ctx.Err() != nil {
			return licenceJSON{}, err
		}
		return licenceJSON{}, errLicenceUnreachable
	}
	t.addSource(assistant.Source{Label: "Registre FFESSM", Link: "/licences"})
	sum := t.s.summarize(rows)
	return licenceJSON{Etat: licenceEtat[sum.State], ValableJusquAu: sum.Until}, nil
}
