package web

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/calendar"
	"github.com/SkYNewZ/sos-vpdive/internal/campaign"
	"github.com/SkYNewZ/sos-vpdive/internal/ffessm"
	"github.com/SkYNewZ/sos-vpdive/internal/imports"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

// mcpInstructions is all a client's model knows of the app before its first
// call: what the data is, where it comes from and how the tools chain.
const mcpInstructions = `Outil de support d'un club de plongée associatif. Les adhérents y déposent des demandes (problème de paiement, de carnet de plongées, d'inscription à une sortie, d'adhésion, de compte) ; le comité bénévole les traite. Ce serveur donne au comité un accès en lecture seule à ces demandes et aux données du club : aucun outil ne modifie quoi que ce soit.

D'où viennent les données. Le club gère ses adhérents, ses sorties et ses paiements dans VPDive, un logiciel tiers. L'outil n'en reçoit que des exports, importés régulièrement : liste des membres, paiements VPDive, encaissements Mollie (paiements en ligne), calendrier des sorties, cartes de plongées (carnets) et campagne d'adhésion. Chaque résultat porte l'état de l'import dont il vient : date de réception, période couverte, et « perime » quand l'import a dépassé son âge maximal. Une donnée absente d'un import n'est pas une donnée inexistante : elle peut être plus récente que l'export, ou hors de sa période. comite_import_status donne la fraîcheur de tous les imports. Les demandes, elles, sont à jour en permanence.

Comment retrouver une personne. Les exports VPDive rattachent les lignes à un nom, pas à un identifiant. comite_find_member cherche une personne et renvoie pour chacune une référence « ref » ; les outils comite_member_* la prennent dans leur champ « adherent ». L'adresse mail d'une demande est une référence valide quand elle correspond à un membre. Un nom porté par plusieurs personnes est un homonyme : ses paiements, ses sorties et son adhésion ne sont pas lisibles, parce qu'on ne peut pas dire à qui les lignes appartiennent.

Formats. Les dates sont au format JJ/MM/AAAA et les heures JJ/MM/AAAA HH:MM, à l'heure de Paris ; les dates passées en paramètre sont au format AAAA-MM-JJ. Les montants sont des textes en euros (« 25,00 € ») ; un solde de carnet négatif dans VPDive signifie qu'il reste du crédit. Les listes sont plafonnées : un champ « tronque » ou « has_more » signale une suite.

Ce qui n'est pas renvoyé. Les numéros de téléphone et les IBAN sont remplacés par [téléphone] et [iban]. Les captures d'écran jointes aux demandes, les fiches d'aide et les résumés produits par l'assistant intégré de l'outil ne sont pas disponibles ici.`

// newMCPServer builds the server and its tools, once: nothing in it depends
// on the request.
func (s *Server) newMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "sos-vpdive-comite", Title: "Support du club, côté comité", Version: "1"},
		&mcp.ServerOptions{Instructions: mcpInstructions})

	addTool(s, server, toolSpec{Name: "comite_find_member", Title: "Chercher un adhérent", Scope: scopeRead,
		Description: "Cherche une personne dans la liste des membres importée de VPDive, par nom, prénom ou les deux (ordre libre, accents et casse ignorés), ou par adresse mail exacte. Cherche aussi dans la campagne d'adhésion les personnes que la liste des membres ne connaît pas encore, marquées source « campagne » : un nouvel adhérent avant le prochain import des membres. Renvoie au plus 10 candidats ; une correspondance exacte masque les correspondances partielles. Chaque candidat porte une référence « ref » à passer au champ « adherent » des outils comite_member_* : l'adresse mail d'un membre, ou une référence « campagne:… » pour une personne sans adresse. « homonyme » signale un nom porté par plusieurs personnes, dont les paiements, les sorties et l'adhésion ne sont pas lisibles.",
	}, mcpCall.findMember)

	addTool(s, server, toolSpec{Name: "comite_member_payments", Title: "Paiements d'un adhérent", Scope: scopeRead,
		Description: "Paiements d'une personne dans les trois exports qui en parlent. « lignes » : ses lignes de l'export des paiements VPDive sur la période (par défaut les 120 derniers jours), 100 au plus, plus tous ses achats de carte et de formation quelle que soit la date. « soldes » : ses soldes de carnet et de formation. « mollie » : ses encaissements en ligne sur la période, avec pour chacun ce que VPDive en dit (« solde_dans_vpdive »). « cartes » : chacune de ses cartes de plongées avec tout son historique (plongées débitées ou recréditées, changements de prix, commentaires), quelle que soit la période. Chaque source a son état : « ok », « vide » (aucune ligne), « hors_liste » (personne absente de la liste des membres), « ambigu » (homonyme : rien n'est lu), « purge » (données effacées 90 jours après le dernier import) ou « aucun_import ».",
	}, mcpCall.memberPayments)

	addTool(s, server, toolSpec{Name: "comite_member_outings", Title: "Sorties d'un adhérent", Scope: scopeRead,
		Description: "Sorties du calendrier VPDive où la personne figure, des 90 derniers jours à toutes celles à venir par défaut, 50 au plus. Pour chaque sortie : son inscription (inscrit, liste d'attente, non inscrit), le nombre de places, ses rôles, l'état de son panier, ses désinscriptions, les lignes de paiement VPDive et Mollie rattachées à la sortie, et des « signaux » : les incohérences repérées entre l'inscription et les paiements. « etat » vaut « ok », « vide », « hors_liste », « ambigu » (homonyme) ou « aucun_calendrier ».",
	}, mcpCall.memberOutings)

	addTool(s, server, toolSpec{Name: "comite_member_membership", Title: "Adhésion d'un adhérent", Scope: scopeRead,
		Description: "Lignes de la personne dans la campagne d'adhésion VPDive de la saison. Statut « En cours » : dossier pas encore validé par le club, ce qui bloque le paiement de la saison ; « Validé » : dossier validé, avec la date et l'auteur de la validation. Chaque entrée d'adhésion et de licence a un état : « Réglé » est payé, « Au panier » est ajouté au panier sans paiement ; le moyen et la date ne sont connus que pour un paiement saisi à la main ou passé par VPayDive. La date de l'export (« export_du ») fait foi : un paiement ou une validation plus récents n'y figurent pas. « etat » vaut « homonyme » pour un nom porté par plusieurs personnes, sans détail. La ligne « licence » dit si la licence a été payée dans VPDive ; pour savoir si elle est valide à la fédération, c'est comite_member_licence.",
	}, mcpCall.memberMembership)

	addTool(s, server, toolSpec{Name: "comite_find_outings", Title: "Chercher des sorties", Scope: scopeRead,
		Description: "Sorties du calendrier VPDive entre deux dates incluses (62 jours au plus), filtrées par un mot du titre quand « texte » est donné, 50 au plus. Pour chacune : identifiant, titre, date, catégorie, activité, annulation, nombre d'inscrits et de personnes en liste d'attente (invités compris). L'identifiant se passe à comite_get_outing.",
	}, mcpCall.findOutings)

	addTool(s, server, toolSpec{Name: "comite_get_outing", Title: "Lire une sortie", Scope: scopeRead,
		Description: "Une sortie du calendrier par son identifiant (champ « id » de comite_find_outings ou de comite_member_outings) : chaque participant avec son inscription, ses rôles et l'état de son panier, puis les désinscriptions avec leur date et leur auteur. « homonyme » signale un participant dont le nom est porté par plusieurs membres.",
	}, mcpCall.getOuting)

	addTool(s, server, toolSpec{Name: "comite_list_cancellations", Title: "Sorties annulées à régulariser", Scope: scopeRead,
		Description: "Sorties annulées dont des lignes payées attendent encore d'être supprimées dans VPDive : pour chacune, le nombre de payeurs et de lignes, le total à recréditer sur les carnets et le total payé en argent. C'est la liste de travail du trésorier après une annulation ; une sortie en sort quand ses lignes ont été régularisées dans VPDive et qu'un nouvel export a été importé.",
	}, mcpCall.listCancellations)

	addTool(s, server, toolSpec{Name: "comite_list_memberships", Title: "Suivi des adhésions", Scope: scopeRead,
		Description: "Toutes les personnes de la campagne d'adhésion VPDive de la saison, triées par nom, rangées en trois sections : « a-valider » (adhésion et licence réglées, dossier pas encore validé dans VPDive), « valides » (dossier validé ; « impaye » signale une adhésion ou une licence non réglée) et « incomplets » (une seule des deux réglée, ou au panier sans paiement). « section » limite la liste à l'une d'elles. « invites » compte les personnes sans adhésion ni licence, absentes de la liste. Pagination par « limit » (200 par défaut, 500 au plus) et « offset » ; « total » compte les personnes de la sélection.",
	}, mcpCall.listMemberships)

	addTool(s, server, toolSpec{Name: "comite_list_anomalies", Title: "Paiements à vérifier", Scope: scopeRead,
		Description: "Lignes de paiement que le trésorier doit regarder, de la plus ancienne à la plus récente : celles que Mollie a encaissées et que VPDive ne compte pas comme soldées, et les paiements partiels de l'export VPDive. Pour chacune : la personne quand la liste des membres permet de l'identifier, la date, le produit, la prestation, le montant. « masquees » compte les lignes que le comité a déjà écartées à la main ; elles ne sont pas listées.",
	}, mcpCall.listAnomalies)

	addTool(s, server, toolSpec{Name: "comite_import_status", Title: "État des imports", Scope: scopeRead,
		Description: "État de chacun des six imports de VPDive (liste des membres, paiements, encaissements Mollie, calendrier, cartes de plongées, campagne d'adhésion) : date du dernier import, auteur (un compte du comité, ou « script » pour l'import automatique), période couverte, nombre de lignes, âge maximal avant rappel et « perime » quand il est dépassé. « passages » liste les 10 derniers passages des 30 derniers jours, réussis ou non, avec leur résultat : « imported » (données remplacées), « unchanged » (fichier identique au précédent), « refused » (fichier refusé par l'outil) ou « failed » (le script n'a pas pu lire VPDive ou envoyer le fichier).",
	}, mcpCall.importStatus)

	if s.licences != nil {
		addTool(s, server, toolSpec{Name: "comite_member_licence", Title: "Licence FFESSM d'une personne", Scope: scopeRead,
			Description: "Statut de la licence d'une personne dans le registre de la fédération (FFESSM), interrogé en direct : « valide », « non valide », « introuvable » ou « plusieurs » (plusieurs licences correspondent), avec le dernier jour de validité. Donne soit « adherent » (la recherche se fait par l'adresse mail du membre, puis ne garde que les licences à son nom), soit « numero » (un numéro de licence sous la forme A-13-634166), jamais les deux. Une adresse inconnue du registre répond « introuvable » : la personne peut avoir pris sa licence sous une autre adresse. À ne pas confondre avec la ligne « licence » de comite_member_membership, qui dit si la licence a été payée dans VPDive. 30 recherches par heure au plus par connexion, avec comite_search_licences.",
		}, mcpCall.memberLicence)

		addTool(s, server, toolSpec{Name: "comite_search_licences", Title: "Chercher dans le registre FFESSM", Scope: scopeRead,
			Description: "Cherche des licenciés dans le registre de la fédération (FFESSM), interrogé en direct, par nom et prénom (dans un ordre ou l'autre), numéro de licence ou adresse mail complète : 3 caractères au moins, 10 résultats au plus. Le registre couvre tous les clubs et compte beaucoup d'homonymes : le club de chaque résultat aide à les distinguer. Renvoie le numéro, le nom, le club, le dernier jour de validité et si la licence est valide aujourd'hui ; ni date de naissance ni coordonnées. 30 recherches par heure au plus par connexion, avec comite_member_licence.",
		}, mcpCall.searchLicences)
	}
	return server
}

// --- members

type findMemberIn struct {
	Query string `json:"query" jsonschema:"Nom, prénom ou les deux, dans un ordre ou l'autre, accents et casse ignorés ; ou l'adresse mail complète d'un adhérent."`
}

func (c mcpCall) findMember(ctx context.Context, in findMemberIn) (findMemberResult, error) {
	query, email := strings.TrimSpace(in.Query), ""
	if query == "" {
		return findMemberResult{}, refusalError("query attendu : un nom, un prénom ou une adresse mail")
	}
	if strings.Contains(query, "@") {
		var err error
		if email, err = secure.NormalizeEmail(query); err != nil {
			return findMemberResult{}, refusalError("adresse mail invalide : elle ne doit pas contenir d'espace")
		}
	}
	return c.t.candidates(ctx, query, email, personHandle)
}

type memberIn struct {
	Adherent string `json:"adherent" jsonschema:"Référence de la personne : la valeur « ref » d'un candidat de comite_find_member, ou l'adresse mail d'une demande."`
}

type memberPeriodIn struct {
	Adherent string `json:"adherent"     jsonschema:"Référence de la personne : la valeur « ref » d'un candidat de comite_find_member, ou l'adresse mail d'une demande."`
	Du       string `json:"du,omitempty" jsonschema:"Premier jour lu, au format AAAA-MM-JJ (heure de Paris). Sans valeur : la période par défaut de l'outil."`
	Au       string `json:"au,omitempty" jsonschema:"Dernier jour lu, inclus, au format AAAA-MM-JJ. Sans valeur : pas de fin."`
}

// memberPeriod reads the person and the window of a member tool.
func (c mcpCall) memberPeriod(ctx context.Context, in memberPeriodIn, back int) (assistant.Person, time.Time, time.Time, error) {
	p, err := c.person(ctx, in.Adherent)
	if err != nil {
		return assistant.Person{}, time.Time{}, time.Time{}, err
	}
	from, to, ok := c.s.period(in.Du, in.Au, back)
	if !ok {
		return assistant.Person{}, time.Time{}, time.Time{}, refusalError("du et au format AAAA-MM-JJ, du avant au")
	}
	return p, from, to, nil
}

func (c mcpCall) memberPayments(ctx context.Context, in memberPeriodIn) (paymentsResult, error) {
	p, from, to, err := c.memberPeriod(ctx, in, paymentsBack)
	if err != nil {
		return paymentsResult{}, err
	}
	return c.t.payments(ctx, p, from, to)
}

func (c mcpCall) memberOutings(ctx context.Context, in memberPeriodIn) (outingsResult, error) {
	p, from, to, err := c.memberPeriod(ctx, in, outingsBack)
	if err != nil {
		return outingsResult{}, err
	}
	return c.t.outings(ctx, p, from, to)
}

func (c mcpCall) memberMembership(ctx context.Context, in memberIn) (membershipResult, error) {
	p, err := c.person(ctx, in.Adherent)
	if err != nil {
		return membershipResult{}, err
	}
	return c.t.membership(ctx, p)
}

// --- the federation's register

// registerAllowed counts one more lookup of the connection in the register,
// which the club's own account pays for.
func (c mcpCall) registerAllowed(ctx context.Context) error {
	ok, err := c.s.limiter.allow(ctx, "mcp-licence:"+strconv.FormatInt(c.connection, 10), mcpLicenceLimit, time.Hour)
	if err != nil {
		return err
	}
	if !ok {
		return refusalError("30 recherches par heure au plus dans le registre de la FFESSM pour cette connexion : réessaie plus tard")
	}
	return nil
}

type memberLicenceIn struct {
	Adherent string `json:"adherent,omitempty" jsonschema:"Référence de la personne : la valeur « ref » d'un candidat de comite_find_member, ou l'adresse mail d'une demande. À donner seul, sans numero."`
	Numero   string `json:"numero,omitempty"   jsonschema:"Numéro de licence FFESSM, sous la forme A-13-634166. À donner seul, sans adherent."`
}

func (c mcpCall) memberLicence(ctx context.Context, in memberLicenceIn) (licenceJSON, error) {
	in.Adherent, in.Numero = strings.TrimSpace(in.Adherent), strings.TrimSpace(in.Numero)
	if (in.Adherent == "") == (in.Numero == "") {
		return licenceJSON{}, refusalError("adherent ou numero attendu, un seul des deux")
	}
	var person *assistant.Person
	if in.Adherent != "" {
		p, err := c.person(ctx, in.Adherent)
		if err != nil {
			return licenceJSON{}, err
		}
		person = &p
	}
	if err := c.registerAllowed(ctx); err != nil {
		return licenceJSON{}, err
	}
	out, err := c.t.licence(ctx, person, in.Numero)
	switch {
	case errors.Is(err, errLicenceNoEmail):
		return licenceJSON{}, refusalError("cette personne n'est connue que par la campagne d'adhésion, sans adresse mail : donne son numéro de licence")
	case errors.Is(err, errLicenceNumber):
		return licenceJSON{}, refusalError("numéro de licence attendu, sous la forme A-13-634166")
	case errors.Is(err, errLicenceUnreachable):
		return licenceJSON{}, refusalError("le registre de la FFESSM ne répond pas : réessaie dans un moment")
	}
	return out, err
}

type searchLicencesIn struct {
	Query string `json:"query" jsonschema:"Nom et prénom dans un ordre ou l'autre, numéro de licence, ou adresse mail complète : 3 caractères au moins."`
}

type licenceFound struct {
	Numero         string `json:"numero"                     jsonschema:"Numéro de licence, à passer à comite_member_licence."`
	Nom            string `json:"nom"                        jsonschema:"NOM en majuscules puis prénom, comme le registre les tient."`
	Club           string `json:"club,omitempty"             jsonschema:"Club auprès duquel la licence a été prise."`
	ValableJusquAu string `json:"valable_jusqu_au,omitempty" jsonschema:"Dernier jour de validité, JJ/MM/AAAA."`
	Valide         bool   `json:"valide"                     jsonschema:"Vrai quand le registre dit la licence active et que sa date de fin n'est pas passée."`
}

type licenceSearchResult struct {
	Resultats []licenceFound `json:"resultats" jsonschema:"Licenciés trouvés, 10 au plus ; vide quand le registre ne connaît personne."`
}

func (c mcpCall) searchLicences(ctx context.Context, in searchLicencesIn) (licenceSearchResult, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return licenceSearchResult{}, refusalError("query attendu : un nom, un numéro de licence ou une adresse mail")
	}
	if err := c.registerAllowed(ctx); err != nil {
		return licenceSearchResult{}, err
	}
	rows, err := c.s.licenceRows(ctx, query)
	switch {
	case errors.Is(err, ffessm.ErrQueryTooShort):
		return licenceSearchResult{}, refusalError("3 caractères au moins pour chercher dans le registre")
	case err != nil && ctx.Err() != nil:
		return licenceSearchResult{}, err
	case err != nil:
		return licenceSearchResult{}, refusalError("le registre de la FFESSM ne répond pas : réessaie dans un moment")
	}
	out := licenceSearchResult{Resultats: make([]licenceFound, 0, len(rows))}
	for _, r := range rows {
		out.Resultats = append(out.Resultats, licenceFound{Numero: r.Number, Nom: r.Name, Club: r.Club,
			ValableJusquAu: c.s.formatDate(r.Until), Valide: c.s.licenceValid(r)})
	}
	return out, nil
}

// --- calendar

type findOutingsIn struct {
	Du    string `json:"du"              jsonschema:"Premier jour cherché, au format AAAA-MM-JJ (heure de Paris)."`
	Au    string `json:"au"              jsonschema:"Dernier jour cherché, inclus, au format AAAA-MM-JJ : 62 jours au plus après du."`
	Texte string `json:"texte,omitempty" jsonschema:"Mot ou début de mot du titre de la sortie, accents et casse ignorés. Sans valeur : toutes les sorties de la période."`
}

func (c mcpCall) findOutings(ctx context.Context, in findOutingsIn) (eventsResult, error) {
	from, to, problem := c.s.outingsPeriod(in.Du, in.Au)
	if problem != nil {
		return eventsResult{}, refusalError(problem.Erreur)
	}
	return c.t.events(ctx, from, to, in.Texte)
}

type getOutingIn struct {
	ID string `json:"id" jsonschema:"Identifiant de la sortie : le champ « id » d'un résultat de comite_find_outings ou de comite_member_outings."`
}

func (c mcpCall) getOuting(ctx context.Context, in getOutingIn) (outingResult, error) {
	out, err := c.t.outingByID(ctx, strings.TrimSpace(in.ID))
	if errors.Is(err, calendar.ErrNotFound) {
		return outingResult{}, refusalError("sortie inconnue : cherche-la avec comite_find_outings")
	}
	return out, err
}

type noInput struct{}

func (c mcpCall) listCancellations(ctx context.Context, _ noInput) (cancellationsResult, error) {
	return c.t.cancelled(ctx)
}

// --- membership campaign

type listMembershipsIn struct {
	Section string `json:"section,omitempty" jsonschema:"Limite la liste à une section : a-valider, valides ou incomplets. Sans valeur : les trois."`
	Limit   int    `json:"limit,omitempty"   jsonschema:"Nombre de personnes renvoyées : 200 par défaut, 500 au plus."`
	Offset  int    `json:"offset,omitempty"  jsonschema:"Nombre de personnes à sauter depuis le début de la sélection : 0 par défaut."`
}

type campaignPersonJSON struct {
	Nom        string              `json:"nom"                  jsonschema:"Nom puis prénom."`
	Section    string              `json:"section"              jsonschema:"a-valider, valides ou incomplets."`
	Validation string              `json:"validation,omitempty" jsonschema:"Pour un dossier validé : quand et par qui. Sinon : ce qui manque ou ce qui est attendu."`
	Adhesion   []campaignEntryJSON `json:"adhesion"             jsonschema:"Entrées d'adhésion de la personne dans la campagne."`
	Licence    []campaignEntryJSON `json:"licence"              jsonschema:"Entrées de licence de la personne dans la campagne."`
	Impaye     bool                `json:"impaye,omitempty"     jsonschema:"Dossier validé alors que l'adhésion ou la licence n'est pas réglée."`
	Homonyme   bool                `json:"homonyme,omitempty"   jsonschema:"Nom que la campagne ou la liste des membres porte plusieurs fois : la ligne n'est rattachée à aucun membre."`
}

type membershipsResult struct {
	Import    importState          `json:"import_campagne" jsonschema:"État de l'import de la campagne d'adhésion."`
	Etat      string               `json:"etat"            jsonschema:"ok ; aucun_import quand la campagne n'a jamais été importée ; purge quand ses données ont été effacées, 90 jours après le dernier import."`
	Total     int                  `json:"total"           jsonschema:"Nombre de personnes de la sélection, toutes pages confondues."`
	HasMore   bool                 `json:"has_more"        jsonschema:"Vrai quand il reste des personnes après cette page : rappelle avec offset augmenté de limit."`
	Personnes []campaignPersonJSON `json:"personnes"       jsonschema:"Personnes de la page, triées par nom puis prénom."`
	Invites   int                  `json:"invites"         jsonschema:"Personnes de la campagne sans adhésion ni licence : absentes des trois sections."`
}

// Page sizes of comite_list_memberships.
const (
	membershipsPage    = 200
	membershipsPageMax = 500
)

func (c mcpCall) listMemberships(ctx context.Context, in listMembershipsIn) (membershipsResult, error) {
	d, err := c.s.memberships(ctx)
	if err != nil {
		return membershipsResult{}, err
	}
	out := membershipsResult{Import: c.s.stateOf(d.Import, d.Imported), Etat: blockState[payments.BlockLines], Invites: d.Invited, Personnes: []campaignPersonJSON{}}
	switch {
	case !d.Imported:
		out.Etat = blockState[payments.BlockNoLines]
	case d.Purged:
		out.Etat = blockState[payments.BlockPurged]
	}
	known := false
	var all []campaignPersonJSON
	for _, sec := range d.Sections {
		known = known || sec.ID == in.Section
		if in.Section != "" && sec.ID != in.Section {
			continue
		}
		for _, p := range sec.People {
			all = append(all, campaignPersonJSON{Nom: p.Name, Section: sec.ID, Validation: p.Validation,
				Adhesion: c.s.entriesJSON(entriesOf(p.Membership)), Licence: c.s.entriesJSON(entriesOf(p.Licence)),
				Impaye: p.Unpaid, Homonyme: p.Homonym})
		}
	}
	if in.Section != "" && !known {
		return membershipsResult{}, refusalError("section inconnue : a-valider, valides ou incomplets")
	}
	limit, offset, problem := pageOf(in.Limit, in.Offset, membershipsPage, membershipsPageMax)
	if problem != nil {
		return membershipsResult{}, problem
	}
	out.Total = len(all)
	if offset < len(all) {
		out.Personnes = all[offset:min(len(all), offset+limit)]
	}
	out.HasMore = offset+len(out.Personnes) < len(all)
	return out, nil
}

// entriesOf are the campaign entries under the page's view of them.
func entriesOf(view []campaignEntry) []campaign.Entry {
	out := make([]campaign.Entry, len(view))
	for i, e := range view {
		out[i] = e.Entry
	}
	return out
}

// pageOf reads a page request: limit defaults to def and stops at maximum.
func pageOf(limit, offset, def, maximum int) (int, int, error) {
	switch {
	case limit < 0 || offset < 0:
		return 0, 0, refusalError("limit et offset sont des entiers positifs")
	case limit == 0:
		limit = def
	}
	return min(limit, maximum), offset, nil
}

// --- payments to check

type anomalyJSON struct {
	Signal       string `json:"signal"                  jsonschema:"Pourquoi la ligne est à vérifier : « encaissée par Mollie, non soldée dans VPDive » ou « paiement partiel »."`
	Personne     string `json:"personne,omitempty"      jsonschema:"Prénom et nom du membre à qui la ligne revient ; absent quand la liste des membres ne permet pas de le dire."`
	Homonyme     bool   `json:"homonyme,omitempty"      jsonschema:"Plusieurs membres portent ce nom : la ligne peut revenir à un autre."`
	Date         string `json:"date,omitempty"          jsonschema:"Date du paiement, JJ/MM/AAAA ; pour un paiement partiel sans date, celle de la création de la ligne."`
	Produit      string `json:"produit"                 jsonschema:"Produit VPDive payé."`
	Prestation   string `json:"prestation,omitempty"    jsonschema:"Mollie seulement : la sortie ou la location que la ligne paie."`
	Montant      string `json:"montant"                 jsonschema:"Mollie : montant encaissé. Paiement partiel : montant déjà payé."`
	PrixUnitaire string `json:"prix_unitaire,omitempty" jsonschema:"Paiement partiel seulement : le prix attendu."`
}

type anomaliesResult struct {
	ImportVPDive importState   `json:"import_vpdive"    jsonschema:"État de l'import des paiements VPDive, d'où viennent les paiements partiels."`
	ImportMollie importState   `json:"import_mollie"    jsonschema:"État de l'import des encaissements Mollie, d'où viennent les lignes non soldées."`
	Resume       string        `json:"resume,omitempty" jsonschema:"Décompte des lignes à vérifier par signal, en une phrase ; absent quand il n'y en a aucune."`
	AVerifier    []anomalyJSON `json:"a_verifier"       jsonschema:"Lignes à vérifier, de la plus ancienne à la plus récente."`
	Masquees     int           `json:"masquees"         jsonschema:"Nombre de lignes que le comité a écartées à la main : elles ne sont pas listées."`
}

// signalLabel names why a line is to check.
var signalLabel = map[payments.CheckSignal]string{
	payments.SignalUnsettled: "encaissée par Mollie, non soldée dans VPDive",
	payments.SignalPartial:   "paiement partiel",
}

func (c mcpCall) listAnomalies(ctx context.Context, _ noInput) (anomaliesResult, error) {
	open, masked, err := c.s.checks.List(ctx)
	if err != nil {
		return anomaliesResult{}, err
	}
	unsettled, partial := payments.Tally(open)
	out := anomaliesResult{Resume: checksSummary(unsettled, partial), AVerifier: make([]anomalyJSON, 0, len(open)), Masquees: len(masked)}
	if out.ImportVPDive, out.ImportMollie, err = c.t.paymentImports(ctx); err != nil {
		return anomaliesResult{}, err
	}
	for _, k := range open {
		j := anomalyJSON{Signal: signalLabel[k.Signal], Personne: k.Person.Name, Homonyme: k.Person.Shared, Date: c.s.formatDate(k.Date),
			Produit: k.Product, Prestation: k.Service, Montant: euros(k.Amount)}
		if k.Signal == payments.SignalPartial {
			j.PrixUnitaire = euros(k.UnitPrice)
		}
		out.AVerifier = append(out.AVerifier, j)
	}
	return out, nil
}

// --- imports

type runJSON struct {
	Le       string `json:"le"       jsonschema:"Date et heure du passage, JJ/MM/AAAA HH:MM."`
	Par      string `json:"par"      jsonschema:"Identifiant du compte du comité qui a déposé le fichier, ou « script » pour l'import automatique."`
	Resultat string `json:"resultat" jsonschema:"imported (données remplacées), unchanged (fichier identique au précédent), refused (fichier refusé par l'outil) ou failed (le script n'a pas abouti)."`
	Detail   string `json:"detail"   jsonschema:"Le résultat en une phrase : nombre de lignes importées, ou raison du refus ou de l'échec."`
}

type importJSON struct {
	Type         string    `json:"type"                 jsonschema:"members, payments, vpaydive (encaissements Mollie), calendar, carnets (cartes de plongées) ou campaign (campagne d'adhésion)."`
	Libelle      string    `json:"libelle"              jsonschema:"Nom de l'import en français."`
	Recu         string    `json:"recu,omitempty"       jsonschema:"Date et heure du dernier import, JJ/MM/AAAA HH:MM ; absent quand il n'y en a aucun."`
	Par          string    `json:"par,omitempty"        jsonschema:"Qui a fait le dernier import : identifiant d'un compte du comité, ou « script »."`
	PeriodeDu    string    `json:"periode_du,omitempty" jsonschema:"Premier jour que couvre l'export, JJ/MM/AAAA, quand l'export porte une période."`
	PeriodeAu    string    `json:"periode_au,omitempty" jsonschema:"Dernier jour que couvre l'export, JJ/MM/AAAA."`
	Lignes       int       `json:"lignes"               jsonschema:"Nombre de lignes importées : comptes, lignes de paiement, événements, cartes ou personnes."`
	Ignorees     int       `json:"ignorees"             jsonschema:"Lignes du fichier que l'import a ignorées parce qu'illisibles."`
	AgeMaxHeures int       `json:"age_max_heures"       jsonschema:"Âge du dernier import, en heures, au-delà duquel le comité reçoit un rappel."`
	Perime       bool      `json:"perime"               jsonschema:"Le dernier import a dépassé son âge maximal : les données peuvent être en retard sur VPDive."`
	AucunImport  bool      `json:"aucun_import"         jsonschema:"Cet export n'a jamais été importé."`
	Passages     []runJSON `json:"passages"             jsonschema:"Les 10 derniers passages des 30 derniers jours, du plus récent au plus ancien, réussis ou non."`
}

type importsResult struct {
	Imports []importJSON `json:"imports" jsonschema:"Un état par export de VPDive."`
}

// mcpRuns is how many runs of an export comite_import_status lists.
const mcpRuns = 10

func (c mcpCall) importStatus(ctx context.Context, _ noInput) (importsResult, error) {
	s := c.s
	ages := s.importAges()
	out := importsResult{Imports: make([]importJSON, 0, len(ages))}
	since := s.now().AddDate(0, 0, -journalDays)
	for _, a := range ages {
		info, ok, err := imports.Last(ctx, s.db, a.kind)
		if err != nil {
			return importsResult{}, err
		}
		runs, err := imports.Runs(ctx, s.db, s.keys, a.kind, since)
		if err != nil {
			return importsResult{}, err
		}
		st := s.stateOf(info, ok)
		j := importJSON{Type: string(a.kind), Libelle: importLabel[a.kind], Recu: st.Recu, Par: info.ImportedBy, PeriodeDu: st.Du, PeriodeAu: st.Au,
			Lignes: info.Rows, Ignorees: info.Skipped, AgeMaxHeures: int(a.maxAge / time.Hour), Perime: st.Perime, AucunImport: st.Aucun,
			Passages: make([]runJSON, 0, min(len(runs), mcpRuns))}
		for _, r := range runs[:min(len(runs), mcpRuns)] {
			j.Passages = append(j.Passages, runJSON{Le: s.formatTime(r.At), Par: r.By, Resultat: string(r.Result), Detail: outcome(r, runUnits[a.kind])})
		}
		out.Imports = append(out.Imports, j)
	}
	return out, nil
}
