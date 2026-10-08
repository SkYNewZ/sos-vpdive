package web

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/kb"
	"github.com/SkYNewZ/sos-vpdive/internal/members"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
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
	for _, p := range append(append([]string{}, required...), optional...) {
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
		Description: "Cherche des adhérents dans la liste des membres par nom, prénom ou les deux (ordre libre, accents et casse ignorés), ou par « [email N] » tel qu'il apparaît dans la saisie. Renvoie au plus 10 candidats, chacun avec une référence m1, m2… à passer aux autres outils."},
	{Name: "member_payments", InputSchema: schema([]string{refArg}, "du", "au"),
		Description: "Paiements VPDive et encaissements Mollie d'un adhérent (référence m1…) : soldes de carnet et de formation, achats de carte, puis les lignes de la période, par défaut les 120 derniers jours. Dates du et au : AAAA-MM-JJ."},
	{Name: "member_outings", InputSchema: schema([]string{refArg}, "du", "au"),
		Description: "Sorties VPDive d'un adhérent : inscription, panier, lignes de paiement rattachées, signaux, désinscriptions. Par défaut, des 90 derniers jours à toutes celles à venir. Dates du et au : AAAA-MM-JJ."},
	{Name: "member_requests", InputSchema: schema([]string{refArg}),
		Description: "Demandes déposées dans cet outil par l'adresse de l'adhérent : référence, catégorie, statut, date, résumé."},
	{Name: "find_outings", InputSchema: schema([]string{"du", "au"}, "texte"),
		Description: "Sorties du calendrier entre deux dates (62 jours au plus), filtrées par un mot du titre si texte est donné : identifiant, titre, date, catégorie, nombre d'inscrits."},
	{Name: "outing", InputSchema: schema([]string{"id"}),
		Description: "Une sortie du calendrier par son identifiant : chaque participant avec son inscription et l'état de son panier, puis les désinscriptions."},
	{Name: "cancellations", InputSchema: schema(nil),
		Description: "Sorties annulées dont des lignes payées attendent la suppression dans VPDive : nombre de payeurs, total à recréditer sur les carnets, total payé en argent."},
	{Name: "read_fiche", InputSchema: schema([]string{"id"}),
		Description: "Une fiche d'aide du club par son identifiant : réponse pour l'adhérent, procédure pour le résolveur, liens VPDive."},
}

// unknownTool stands for a tool name the model made up, in telemetry.
const unknownTool = "unknown"

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
func (t *toolbox) tools() assistant.Tools { return assistant.Tools{Defs: toolDefs, Run: t.run} }

// toolProblem is a call the model got wrong: it reads why and may try again.
type toolProblem struct {
	Erreur string `json:"erreur"`
}

// run executes one call and returns the JSON the model reads, masked, and
// the step the resolver sees. A call the model got wrong, an unknown tool
// included, is answered in the JSON: only a failure of ours is an error.
func (t *toolbox) run(ctx context.Context, name string, input json.RawMessage) (string, string, error) {
	// The model chooses name: telemetry never records one it made up.
	spanName := unknownTool
	if slices.ContainsFunc(toolDefs, func(d assistant.Tool) bool { return d.Name == name }) {
		spanName = name
	}
	ctx, span := t.s.tracer.Start(ctx, "assistant.tool", trace.WithAttributes(attribute.String("assistant.tool.name", spanName)))
	defer span.End()
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
	case "member_requests":
		out, step, err = t.memberRequests(ctx, input)
	case "find_outings":
		out, step, err = t.findOutings(ctx, input)
	case "outing":
		out, step, err = t.outing(ctx, input)
	case "cancellations":
		out, step, err = t.cancellations(ctx)
	case "read_fiche":
		out, step = t.readFiche(input)
	default:
		out, step = toolProblem{"outil inconnu : " + name}, "Outil inconnu demandé"
	}
	if err != nil {
		telemetry.Fail(span, "store")
		return "", "", err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", "", fmt.Errorf("encode tool result: %w", err)
	}
	// Every imported text is free: a product, an outing title, an author or
	// a request summary may hold an address, a phone number or an IBAN. One
	// mask over the whole result covers them all; its replacements hold no
	// quote, so the JSON stays valid.
	result, emails := assistant.Mask(string(raw), t.c.Emails)
	t.c.Emails = emails
	return result, step, nil
}

// decode reads a tool input; ok is false when it is not the expected object.
func decode[T any](input json.RawMessage) (T, bool) {
	var v T
	return v, json.Unmarshal(input, &v) == nil
}

// addSource records a piece of data the answer read, once.
func (t *toolbox) addSource(src assistant.Source) {
	for _, s := range t.sources {
		if s.Label == src.Label && s.Link == src.Link {
			return
		}
	}
	t.sources = append(t.sources, src)
}

// importState is an import as the model reads it.
type importState struct {
	Recu   string `json:"recu,omitempty"` // Paris date and time
	Du     string `json:"periode_du,omitempty"`
	Au     string `json:"periode_au,omitempty"`
	Perime bool   `json:"perime"`
	Aucun  bool   `json:"aucun_import,omitempty"`
}

// importState reads the latest import of kind, stale past the age of its banner.
func (s *Server) importState(ctx context.Context, kind imports.Kind) (importState, error) {
	info, ok, err := imports.Last(ctx, s.db, kind)
	if err != nil {
		return importState{}, err
	}
	if !ok {
		return importState{Aucun: true}, nil
	}
	st := importState{Recu: s.formatTime(info.ImportedAt), Du: s.formatDate(info.PeriodFrom), Au: s.formatDate(info.PeriodTo)}
	for _, a := range s.importAges() {
		if a.kind == kind {
			st.Perime = s.now().Sub(info.ImportedAt) > a.maxAge
		}
	}
	return st, nil
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

// importOf reads kind's state and records it as a source.
func (t *toolbox) importOf(ctx context.Context, kind imports.Kind) (importState, error) {
	st, err := t.s.importState(ctx, kind)
	if err == nil && !st.Aucun {
		date, _, _ := strings.Cut(st.Recu, " ")
		t.addSource(assistant.Source{Label: importLabel[kind], Date: date, Stale: st.Perime})
	}
	return st, err
}

// plainSpaces turns the no-break spaces of an amount or a cart into plain ones.
func plainSpaces(s string) string { return strings.ReplaceAll(s, "\u00a0", " ") }

// euros is an amount as the model reads it.
func euros(a payments.Amount) string { return plainSpaces(a.Euros()) }

// --- find_member

type candidate struct {
	Ref      string `json:"ref"`
	Nom      string `json:"nom"`
	Saisons  string `json:"saisons"`
	Licence  string `json:"licence"`
	Homonyme bool   `json:"homonyme"`
}

type findMemberResult struct {
	Import         importState `json:"import_membres"`
	Correspondance string      `json:"correspondance"` // exacte, partielle or aucune
	Candidats      []candidate `json:"candidats"`
}

func (t *toolbox) findMember(ctx context.Context, input json.RawMessage) (any, string, error) {
	in, ok := decode[struct {
		Query string `json:"query"`
	}](input)
	if !ok || strings.TrimSpace(in.Query) == "" {
		return toolProblem{"query attendu : un nom, un prénom ou [email N]"}, "Recherche d'adhérent refusée", nil
	}
	st, err := t.importOf(ctx, imports.Members)
	if err != nil {
		return nil, "", err
	}
	var matches []members.Match
	if email, ok := assistant.Placeholder(in.Query, t.c.Emails); ok {
		matches, err = t.placeholderMatch(ctx, email)
	} else {
		matches, err = t.s.members.Search(ctx, in.Query, maxCandidates)
	}
	if err != nil {
		return nil, "", err
	}
	out := findMemberResult{Import: st, Correspondance: "aucune", Candidats: []candidate{}}
	for _, m := range matches {
		out.Correspondance = map[bool]string{true: "exacte", false: "partielle"}[m.Exact]
		view := newProfileView(m.Profile)
		name := strings.TrimSpace(m.FirstName + " " + m.LastName)
		ref := t.c.AddPerson(assistant.Person{Name: name, Email: m.Email, NameHash: m.NameHash,
			Seasons: view.Seasons, Licence: view.Licence, Shared: m.Shared})
		out.Candidats = append(out.Candidats, candidate{Ref: ref, Nom: name, Saisons: view.Seasons, Licence: view.Licence, Homonyme: m.Shared > 1})
	}
	return out, "Recherche « " + strings.TrimSpace(in.Query) + " » : " + plural(len(out.Candidats), "candidat", "candidats"), nil
}

// placeholderMatch is the member of an address the resolver typed (it
// reaches the model as [email N]): no match when that address is not a
// member's.
func (t *toolbox) placeholderMatch(ctx context.Context, email string) ([]members.Match, error) {
	p, found, err := t.s.members.Find(ctx, email)
	if err != nil || !found {
		return nil, err
	}
	n, err := t.s.members.NameCount(ctx, p.NameHash)
	if err != nil {
		return nil, err
	}
	return []members.Match{{Profile: p, Email: email, Shared: n, Exact: true}}, nil
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
	today := s.now().In(s.paris)
	from = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, s.paris).AddDate(0, 0, -back)
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

func within(t, from, to time.Time) bool {
	return !t.Before(from) && (to.IsZero() || t.Before(to))
}

// blockState names a payments block state for the model.
var blockState = map[payments.BlockState]string{
	payments.BlockNoLines: "aucun_import", payments.BlockPurged: "purge", payments.BlockNoMember: "hors_liste",
	payments.BlockAmbiguous: "ambigu", payments.BlockEmpty: "vide", payments.BlockLines: "ok",
}

type lineJSON struct {
	Date         string `json:"date,omitempty"` // « Du », the outing's start
	Creee        string `json:"creee"`
	Intitule     string `json:"intitule"`
	Type         string `json:"type,omitempty"`
	Etat         string `json:"etat"`
	Methode      string `json:"methode,omitempty"`
	PrixUnitaire string `json:"prix_unitaire"`
	Quantite     string `json:"quantite"`
	MontantPaye  string `json:"montant_paye"`
	Reduction    string `json:"reduction,omitempty"`
	PayeLe       string `json:"paye_le,omitempty"`
}

func (s *Server) lineJSON(l payments.Line) lineJSON {
	j := lineJSON{Date: s.formatTime(l.Starts), Creee: s.formatTime(l.Created), Intitule: l.Product, Type: l.ProductType,
		Etat: l.State, Methode: methodLabel(l.Method), PrixUnitaire: euros(l.UnitPrice), Quantite: l.Quantity.Number(),
		MontantPaye: euros(l.Paid), PayeLe: s.formatTime(l.PaidAt)}
	if l.Discount != 0 {
		j.Reduction = euros(l.Discount)
	}
	return j
}

type mollieJSON struct {
	PayeLe      string `json:"paye_le"`
	Produit     string `json:"produit"`
	Prestation  string `json:"prestation,omitempty"`
	Date        string `json:"date,omitempty"`
	Montant     string `json:"montant"`
	SoldeVPDive string `json:"solde_dans_vpdive"` // « Payé » as read: Oui, Non, or else
	Methode     string `json:"methode,omitempty"`
}

func (s *Server) mollieJSON(l payments.MollieLine) mollieJSON {
	return mollieJSON{PayeLe: s.formatTime(l.PaidAt), Produit: l.Product, Prestation: l.Service, Date: s.formatDate(l.Starts),
		Montant: euros(l.Amount), SoldeVPDive: cmp.Or(l.Settled, "vide"), Methode: l.Method}
}

// --- member_payments

type paymentsResult struct {
	Import       importState  `json:"import_vpdive"`
	Etat         string       `json:"etat"`
	Soldes       []lineJSON   `json:"soldes"`
	Lignes       []lineJSON   `json:"lignes"`
	Tronque      bool         `json:"tronque,omitempty"`
	ImportMollie importState  `json:"import_mollie"`
	EtatMollie   string       `json:"etat_mollie"`
	Mollie       []mollieJSON `json:"mollie"`
}

func (t *toolbox) memberPayments(ctx context.Context, input json.RawMessage) (any, string, error) {
	p, from, to, problem := t.member(input, paymentsBack)
	if problem != nil {
		return *problem, "Lecture de paiements refusée", nil
	}
	pay, err := t.s.payments.Block(ctx, p.NameHash)
	if err != nil {
		return nil, "", err
	}
	mol, err := t.s.mollie.Block(ctx, p.NameHash)
	if err != nil {
		return nil, "", err
	}
	out := paymentsResult{Etat: blockState[pay.State], EtatMollie: blockState[mol.State], Soldes: []lineJSON{}, Lignes: []lineJSON{}, Mollie: []mollieJSON{}}
	if out.Import, err = t.importOf(ctx, imports.Payments); err != nil {
		return nil, "", err
	}
	if out.ImportMollie, err = t.importOf(ctx, imports.Mollie); err != nil {
		return nil, "", err
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
	for _, m := range mol.Payments {
		for _, l := range m.Lines {
			if within(l.PaidAt, from, to) {
				out.Mollie = append(out.Mollie, t.s.mollieJSON(l.MollieLine))
			}
		}
	}
	return out, "Paiements de " + p.Name + " lus", nil
}

// --- member_outings

type unregJSON struct {
	Nom string `json:"nom,omitempty"`
	Le  string `json:"le"`
	Par string `json:"par,omitempty"`
}

type outingJSON struct {
	ID              string       `json:"id"`
	Titre           string       `json:"titre"`
	Debut           string       `json:"debut"`
	Annulee         bool         `json:"annulee,omitempty"`
	Participation   string       `json:"participation,omitempty"` // inscrit, liste_attente, non_inscrit
	Personnes       int          `json:"personnes,omitempty"`
	Invite          bool         `json:"invite,omitempty"`
	Roles           string       `json:"roles,omitempty"`
	Panier          string       `json:"panier,omitempty"`
	Desinscriptions []unregJSON  `json:"desinscriptions,omitempty"`
	LignesVPDive    []lineJSON   `json:"lignes_vpdive,omitempty"`
	LignesMollie    []mollieJSON `json:"lignes_mollie,omitempty"`
	Signaux         []string     `json:"signaux,omitempty"`
}

type outingsResult struct {
	Import  importState  `json:"import_calendrier"`
	Etat    string       `json:"etat"`
	Sorties []outingJSON `json:"sorties"`
	Tronque bool         `json:"tronque,omitempty"`
}

var outingsStateName = map[outingsState]string{
	outingsNoCalendar: "aucun_calendrier", outingsNoMember: "hors_liste", outingsAmbiguous: "ambigu",
	outingsEmpty: "vide", outingsList: "ok",
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
		j.Participation, j.Personnes, j.Invite = participation(*p), p.People, p.Guest
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

func (t *toolbox) memberOutings(ctx context.Context, input json.RawMessage) (any, string, error) {
	p, from, to, problem := t.member(input, outingsBack)
	if problem != nil {
		return *problem, "Lecture de sorties refusée", nil
	}
	pay, err := t.s.payments.Block(ctx, p.NameHash)
	if err != nil {
		return nil, "", err
	}
	mol, err := t.s.mollie.Block(ctx, p.NameHash)
	if err != nil {
		return nil, "", err
	}
	b, err := t.s.outingsBlock(ctx, t.s.now(), members.Profile{NameHash: p.NameHash}, true, pay, mol)
	if err != nil {
		return nil, "", err
	}
	out := outingsResult{Etat: outingsStateName[b.State], Sorties: []outingJSON{}}
	if out.Import, err = t.importOf(ctx, imports.Calendar); err != nil {
		return nil, "", err
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
	return out, "Sorties de " + p.Name + " lues", nil
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
	Note     string        `json:"note"`
}

func (t *toolbox) memberRequests(ctx context.Context, input json.RawMessage) (any, string, error) {
	p, _, _, problem := t.member(input, 0)
	if problem != nil {
		return *problem, "Lecture de demandes refusée", nil
	}
	rows, err := t.s.tickets.ByEmail(ctx, p.Email)
	if err != nil {
		return nil, "", err
	}
	out := requestsResult{Demandes: []requestJSON{},
		Note: "Seules les demandes déposées dans cet outil sont connues : SMS, mails et messages VPDive n'y sont pas."}
	for _, r := range rows {
		out.Demandes = append(out.Demandes, requestJSON{Ref: r.Ref, Categorie: t.s.tickets.Catalog.CategoryLabel(r.Category),
			Statut: r.Status.Label(), Deposee: t.s.formatDate(r.SubmittedAt), Resume: cmp.Or(r.Summary, r.Excerpt)})
		t.addSource(assistant.Source{Label: r.Ref, Link: "/demandes/" + strconv.FormatInt(r.ID, 10)})
	}
	return out, "Demandes de " + p.Name + " lues", nil
}

// --- find_outings and outing

type eventJSON struct {
	ID           string `json:"id"`
	Titre        string `json:"titre"`
	Debut        string `json:"debut"`
	Categorie    string `json:"categorie,omitempty"`
	Activite     string `json:"activite,omitempty"`
	Annulee      bool   `json:"annulee,omitempty"`
	Inscrits     int    `json:"inscrits"`
	ListeAttente int    `json:"liste_attente,omitempty"`
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
	Import  importState `json:"import_calendrier"`
	Sorties []eventJSON `json:"sorties"`
}

func (t *toolbox) findOutings(ctx context.Context, input json.RawMessage) (any, string, error) {
	in, ok := decode[struct {
		Du    string `json:"du"`
		Au    string `json:"au"`
		Texte string `json:"texte"`
	}](input)
	if !ok || in.Du == "" || in.Au == "" {
		return toolProblem{"du et au attendus, au format AAAA-MM-JJ"}, findOutingsRefused, nil
	}
	from, to, ok := t.s.period(in.Du, in.Au, 0)
	if !ok {
		return toolProblem{"dates au format AAAA-MM-JJ, du avant au"}, findOutingsRefused, nil
	}
	if to.Sub(from) > maxOutingsRange*24*time.Hour+time.Hour { // an hour of slack for a time change
		return toolProblem{"62 jours au plus entre du et au"}, findOutingsRefused, nil
	}
	events, err := t.s.calendar.Range(ctx, from, to, true)
	if err != nil {
		return nil, "", err
	}
	out := eventsResult{Sorties: []eventJSON{}}
	if out.Import, err = t.importOf(ctx, imports.Calendar); err != nil {
		return nil, "", err
	}
	text := normTitle(in.Texte)
	for _, ev := range events {
		if text != "" && !strings.Contains(normTitle(ev.Title), text) {
			continue
		}
		out.Sorties = append(out.Sorties, t.s.eventJSON(ev))
	}
	return out, "Sorties du " + t.s.formatDate(from) + " cherchées : " + plural(len(out.Sorties), "sortie", "sorties"), nil
}

type participantJSON struct {
	Nom           string `json:"nom"`
	Participation string `json:"participation"`
	Personnes     int    `json:"personnes,omitempty"`
	Invite        bool   `json:"invite,omitempty"`
	Roles         string `json:"roles,omitempty"`
	Panier        string `json:"panier"`
	Homonyme      bool   `json:"homonyme,omitempty"`
}

type outingResult struct {
	Import          importState       `json:"import_calendrier"`
	Sortie          eventJSON         `json:"sortie"`
	Participants    []participantJSON `json:"participants"`
	Desinscriptions []unregJSON       `json:"desinscriptions"`
}

func (t *toolbox) outing(ctx context.Context, input json.RawMessage) (any, string, error) {
	in, ok := decode[struct {
		ID string `json:"id"`
	}](input)
	if !ok || in.ID == "" {
		return toolProblem{"id attendu"}, "Lecture de sortie refusée", nil
	}
	ev, err := t.s.calendar.Event(ctx, in.ID)
	if errors.Is(err, calendar.ErrNotFound) {
		return toolProblem{"sortie inconnue : cherche-la avec find_outings"}, "Sortie inconnue", nil
	}
	if err != nil {
		return nil, "", err
	}
	left, err := t.s.calendar.Unregistrations(ctx, in.ID)
	if err != nil {
		return nil, "", err
	}
	out := outingResult{Participants: []participantJSON{}, Desinscriptions: []unregJSON{},
		Sortie: t.s.eventJSON(ev)}
	if out.Import, err = t.importOf(ctx, imports.Calendar); err != nil {
		return nil, "", err
	}
	for _, p := range ev.Participants {
		out.Participants = append(out.Participants, participantJSON{Nom: p.Name, Participation: participation(p), Personnes: p.People,
			Invite: p.Guest, Roles: t.s.labels.roles(p.Roles), Panier: plainSpaces(cartText(p.Payment)), Homonyme: p.Members > 1})
	}
	for _, u := range left {
		out.Desinscriptions = append(out.Desinscriptions, unregJSON{Nom: strings.TrimSpace(u.FirstName + " " + u.LastName),
			Le: t.s.formatTime(u.Time), Par: u.By})
	}
	t.addSource(assistant.Source{Label: ev.Title, Link: "/calendrier/" + ev.ID})
	return out, "Sortie « " + ev.Title + " » lue", nil
}

// --- cancellations

type cancelledJSON struct {
	Titre     string `json:"titre"`
	Date      string `json:"date,omitempty"`
	Payeurs   int    `json:"payeurs"`
	Lignes    int    `json:"lignes"`
	ParCarnet string `json:"a_recrediter_sur_carnets"`
	EnArgent  string `json:"paye_en_argent"`
}

type cancellationsResult struct {
	Import  importState     `json:"import_vpdive"`
	Sorties []cancelledJSON `json:"sorties"`
}

func (t *toolbox) cancellations(ctx context.Context) (any, string, error) {
	c, err := t.s.payments.Cancellations(ctx)
	if err != nil {
		return nil, "", err
	}
	out := cancellationsResult{Sorties: []cancelledJSON{}}
	if out.Import, err = t.importOf(ctx, imports.Payments); err != nil {
		return nil, "", err
	}
	for _, o := range c.Outings {
		out.Sorties = append(out.Sorties, cancelledJSON{Titre: o.Title, Date: t.s.formatDate(o.Starts), Payeurs: o.Persons,
			Lignes: o.Lines, ParCarnet: euros(o.ByCarnet), EnArgent: euros(o.ByMoney)})
	}
	t.addSource(assistant.Source{Label: "Annulations", Link: "/annulations"})
	return out, "Sorties annulées lues", nil
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
	out := ficheResult{ID: f.ID, Titre: f.Title, ReponseAdherent: f.AnswerText, Procedure: blocksText(f.Procedure), Liens: []linkJSON{}}
	for _, key := range f.Links {
		if l, ok := t.s.vpdive[key]; ok {
			out.Liens = append(out.Liens, linkJSON{Libelle: l.Label, URL: l.URL})
		}
	}
	t.addSource(assistant.Source{Label: "Fiche « " + f.Title + " »", Link: "/fiches#" + f.ID})
	return out, "Fiche « " + f.Title + " » lue"
}

// blocksText writes blocks back as plain text, lists numbered or dashed.
func blocksText(bs []kb.Block) string {
	var b strings.Builder
	for _, bl := range bs {
		for i, item := range bl.Items {
			switch bl.Kind {
			case kb.Paragraph:
				// no prefix
			case kb.Numbers:
				b.WriteString(strconv.Itoa(i+1) + ". ")
			case kb.Bullets:
				b.WriteString("- ")
			}
			b.WriteString(item + "\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
