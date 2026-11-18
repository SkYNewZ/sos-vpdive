package web

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// Limits of the request tools.
const (
	ticketsPage    = 50
	ticketsPageMax = 100
	ticketMessages = 100 // of a request's thread: the latest
	statsMonths    = 12  // months comite_ticket_stats covers by default
)

// ticketStatuses are the statuses a confirmed request goes through.
var ticketStatuses = []tickets.Status{tickets.StatusTodo, tickets.StatusInProgress, tickets.StatusWaiting, tickets.StatusDone}

// addTicketTools registers the tools that read the requests. Their
// descriptions name the categories of config/categories.yaml as they are.
func (s *Server) addTicketTools(server *mcp.Server) {
	cats := make([]string, len(s.tickets.Catalog.Categories))
	for i, c := range s.tickets.Catalog.Categories {
		cats[i] = c.ID + " (" + c.Label + ")"
	}
	categories := strings.Join(cats, ", ")

	addTool(s, server, toolSpec{Name: "comite_list_tickets", Title: "Lister les demandes", Scope: scopeRead,
		Description: "Demandes déposées par les adhérents, de la plus récente à la plus ancienne, avec pour chacune sa référence, son demandeur, sa catégorie, son statut, le compte du comité qui la traite, ses dates et le début de sa description. Filtres facultatifs : statuts, catégorie, compte qui traite, adresse du demandeur, période de dépôt. Statuts : todo (à traiter, personne ne l'a prise), in_progress (en cours), waiting (en attente d'une réponse de l'adhérent), done (fait, close). Catégories : " + categories + ". Pagination par « limit » (50 par défaut, 100 au plus) et « offset » ; « total » compte les demandes de la sélection. Les brouillons jamais envoyés n'apparaissent pas ; une demande close est supprimée après le délai de conservation du club. Aucune recherche plein texte : pour lire une demande en entier, comite_get_ticket.",
	}, mcpCall.listTickets)

	addTool(s, server, toolSpec{Name: "comite_get_ticket", Title: "Lire une demande", Scope: scopeRead,
		Description: "Une demande en entier, par son identifiant numérique ou sa référence (CPP-0042) : demandeur et adresse, champs du formulaire, description, fil des messages du plus ancien au plus récent (les 100 derniers), journal des actions, autres demandes de la même adresse, et le membre que cette adresse désigne dans la liste des membres. Un message « interne » est une note que le comité s'est écrite : l'adhérent ne la voit pas, à la différence des autres messages. Le membre rapproché porte la référence « ref » à passer aux outils comite_member_* pour lire ses paiements, ses sorties et son adhésion ; le rapprochement se fait par l'adresse, jamais par le nom saisi. Les captures d'écran jointes ne sont pas renvoyées.",
	}, mcpCall.getTicket)

	addTool(s, server, toolSpec{Name: "comite_ticket_stats", Title: "Statistiques des demandes", Scope: scopeRead,
		Description: "Chiffres des demandes pour un rapport ou un graphique. « demandes_en_place » : le nombre de demandes présentes aujourd'hui par statut, quelle que soit la période. « par_mois » : pour chaque mois (heure de Paris) et chaque catégorie de la période, les demandes déposées, les demandes closes, le délai moyen de traitement en heures (du dépôt à la clôture) et les demandes évitées, c'est-à-dire résolues par une fiche d'aide avant l'envoi. Par défaut les 12 derniers mois ; « du » et « au » sont des mois au format AAAA-MM. Un mois sans aucune demande n'a pas de ligne. Catégories : " + categories + ".",
	}, mcpCall.ticketStats)
}

type listTicketsIn struct {
	Statuts   []string `json:"statuts,omitempty"   jsonschema:"Statuts gardés, parmi todo, in_progress, waiting et done. Sans valeur : tous."`
	Categorie string   `json:"categorie,omitempty" jsonschema:"Identifiant de catégorie, parmi ceux que liste la description de l'outil. Sans valeur : toutes."`
	Assigne   string   `json:"assigne,omitempty"   jsonschema:"Identifiant du compte du comité qui traite la demande, ou « - » pour les demandes que personne n'a prises. Sans valeur : tous."`
	Email     string   `json:"email,omitempty"     jsonschema:"Adresse mail du demandeur : seulement les demandes déposées avec cette adresse."`
	Du        string   `json:"du,omitempty"        jsonschema:"Premier jour de dépôt gardé, au format AAAA-MM-JJ (heure de Paris). Sans valeur : pas de début."`
	Au        string   `json:"au,omitempty"        jsonschema:"Dernier jour de dépôt gardé, inclus, au format AAAA-MM-JJ. Sans valeur : pas de fin."`
	Limit     int      `json:"limit,omitempty"     jsonschema:"Nombre de demandes renvoyées : 50 par défaut, 100 au plus."`
	Offset    int      `json:"offset,omitempty"    jsonschema:"Nombre de demandes à sauter depuis la plus récente : 0 par défaut."`
}

type ticketRowJSON struct {
	ID               int64  `json:"id"                            jsonschema:"Identifiant numérique de la demande, à passer à comite_get_ticket."`
	Ref              string `json:"ref"                           jsonschema:"Référence que l'adhérent connaît, sous la forme CPP-0042."`
	Demandeur        string `json:"demandeur"                     jsonschema:"Prénom et nom saisis par le demandeur."`
	Categorie        string `json:"categorie"                     jsonschema:"Identifiant de la catégorie."`
	CategorieLibelle string `json:"categorie_libelle"             jsonschema:"Nom de la catégorie ; « retiré » signale une catégorie qui n'existe plus."`
	Statut           string `json:"statut"                        jsonschema:"todo, in_progress, waiting ou done."`
	StatutLibelle    string `json:"statut_libelle"                jsonschema:"À traiter, En cours, En attente de l'adhérent ou Fait."`
	Assigne          string `json:"assigne,omitempty"             jsonschema:"Identifiant du compte du comité qui traite la demande ; absent tant que personne ne l'a prise."`
	Deposee          string `json:"deposee_le"                    jsonschema:"Date et heure du dépôt, JJ/MM/AAAA HH:MM."`
	Close            string `json:"close_le,omitempty"            jsonschema:"Date et heure de la clôture ; absent tant que la demande est ouverte."`
	Extrait          string `json:"extrait"                       jsonschema:"Début de la description, 140 caractères au plus."`
	AdherentRepondu  bool   `json:"adherent_a_repondu_en_dernier" jsonschema:"Vrai quand le dernier message visible de l'adhérent est le sien : la demande attend une réponse du comité."`
}

func (s *Server) ticketRowJSON(r tickets.Row) ticketRowJSON {
	return ticketRowJSON{ID: r.ID, Ref: r.Ref, Demandeur: strings.TrimSpace(r.FirstName + " " + r.LastName),
		Categorie: r.Category, CategorieLibelle: s.tickets.Catalog.CategoryLabel(r.Category),
		Statut: string(r.Status), StatutLibelle: r.Status.Label(), Assigne: r.Assignee,
		Deposee: s.formatTime(r.SubmittedAt), Close: s.formatTime(r.ClosedAt), Extrait: r.Excerpt, AdherentRepondu: r.MemberRepliedLast}
}

type ticketsResult struct {
	Total    int             `json:"total"    jsonschema:"Nombre de demandes de la sélection, toutes pages confondues."`
	HasMore  bool            `json:"has_more" jsonschema:"Vrai quand il reste des demandes après cette page : rappelle avec offset augmenté de limit."`
	Demandes []ticketRowJSON `json:"demandes" jsonschema:"Demandes de la page, de la plus récente à la plus ancienne."`
}

// days reads an optional window of Paris days: from is zero without du, to
// (exclusive) without au.
func (s *Server) days(du, au string) (from, to time.Time, err error) {
	refused := refusalError("du et au : format AAAA-MM-JJ, du avant au")
	if du != "" {
		if from, err = time.ParseInLocation(time.DateOnly, du, s.paris); err != nil {
			return from, to, refused
		}
	}
	if au != "" {
		if to, err = time.ParseInLocation(time.DateOnly, au, s.paris); err != nil {
			return from, to, refused
		}
		if to = to.AddDate(0, 0, 1); !to.After(from) {
			return from, to, refused
		}
	}
	return from, to, nil
}

func (c mcpCall) listTickets(ctx context.Context, in listTicketsIn) (ticketsResult, error) {
	s := c.s
	statuses := ticketStatuses
	if len(in.Statuts) > 0 {
		statuses = make([]tickets.Status, len(in.Statuts))
		for i, st := range in.Statuts {
			if statuses[i] = tickets.Status(st); !slices.Contains(ticketStatuses, statuses[i]) {
				return ticketsResult{}, refusalError("statut inconnu : todo, in_progress, waiting ou done")
			}
		}
	}
	if in.Categorie != "" && !s.tickets.Catalog.Has(in.Categorie) {
		return ticketsResult{}, refusalError("catégorie inconnue : la description de l'outil liste les identifiants")
	}
	from, to, err := s.days(in.Du, in.Au)
	if err != nil {
		return ticketsResult{}, err
	}
	limit, offset, err := pageOf(in.Limit, in.Offset, ticketsPage, ticketsPageMax)
	if err != nil {
		return ticketsResult{}, err
	}
	var rows []tickets.Row
	if email := strings.TrimSpace(in.Email); email != "" {
		if _, err := secure.NormalizeEmail(email); err != nil {
			return ticketsResult{}, refusalError("adresse mail invalide : elle ne doit pas contenir d'espace")
		}
		rows, err = s.tickets.ByEmail(ctx, email)
	} else {
		rows, err = s.tickets.Board(ctx, tickets.Filter{Statuses: statuses, Category: in.Categorie, Assignee: in.Assigne})
	}
	if err != nil {
		return ticketsResult{}, err
	}
	rows = slices.DeleteFunc(rows, func(r tickets.Row) bool {
		return !slices.Contains(statuses, r.Status) || (in.Categorie != "" && r.Category != in.Categorie) ||
			(in.Assigne == tickets.AssigneeNobody && r.Assignee != "") ||
			(in.Assigne != "" && in.Assigne != tickets.AssigneeNobody && r.Assignee != in.Assigne) ||
			!within(r.SubmittedAt, from, to)
	})
	slices.SortStableFunc(rows, func(a, b tickets.Row) int {
		return cmp.Or(b.SubmittedAt.Compare(a.SubmittedAt), cmp.Compare(b.ID, a.ID))
	})
	out := ticketsResult{Total: len(rows), Demandes: []ticketRowJSON{}}
	rows, out.HasMore = pageSlice(rows, limit, offset)
	for _, r := range rows {
		out.Demandes = append(out.Demandes, s.ticketRowJSON(r))
	}
	return out, nil
}

type getTicketIn struct {
	Demande string `json:"demande" jsonschema:"Identifiant numérique de la demande (son champ « id »), ou sa référence sous la forme CPP-0042."`
}

type champJSON struct {
	Champ  string `json:"champ"  jsonschema:"Nom du champ du formulaire."`
	Valeur string `json:"valeur" jsonschema:"Ce que le demandeur a saisi ou choisi."`
}

type messageJSON struct {
	Le       string `json:"le"            jsonschema:"Date et heure du message, JJ/MM/AAAA HH:MM."`
	Auteur   string `json:"auteur"        jsonschema:"« adhérent », ou le nom et la fonction du membre du comité qui a écrit."`
	Adherent bool   `json:"de_l_adherent" jsonschema:"Vrai quand le message vient de l'adhérent."`
	Interne  bool   `json:"interne"       jsonschema:"Vrai pour une note que le comité s'est écrite : l'adhérent ne la voit pas."`
	Texte    string `json:"texte"         jsonschema:"Texte du message."`
}

type journalJSON struct {
	Le        string `json:"le"        jsonschema:"Date et heure de l'action, JJ/MM/AAAA HH:MM."`
	Par       string `json:"par"       jsonschema:"Qui a agi : un membre du comité, « l'adhérent », ou « automatique »."`
	Evenement string `json:"evenement" jsonschema:"Ce qui s'est passé : demande envoyée, prise en charge, réponse, mise en attente, clôture, etc."`
}

type ticketJSON struct {
	Demande          ticketRowJSON   `json:"demande"                     jsonschema:"La demande, avec les champs de comite_list_tickets."`
	Email            string          `json:"email"                       jsonschema:"Adresse mail du demandeur. Quand elle désigne un membre, c'est aussi la référence « adherent » des outils comite_member_*."`
	Champs           []champJSON     `json:"champs"                      jsonschema:"Champs du formulaire propres à la catégorie de la demande."`
	Description      string          `json:"description"                 jsonschema:"Texte de la demande, tel que le demandeur l'a écrit."`
	Messages         []messageJSON   `json:"messages"                    jsonschema:"Fil des messages, du plus ancien au plus récent, notes internes comprises."`
	MessagesTronques bool            `json:"messages_tronques,omitempty" jsonschema:"Vrai quand le fil compte plus de 100 messages : seuls les 100 derniers sont renvoyés."`
	Journal          []journalJSON   `json:"journal"                     jsonschema:"Journal des actions sur la demande, de la plus ancienne à la plus récente."`
	AutresDemandes   []ticketRowJSON `json:"autres_demandes"             jsonschema:"Autres demandes déposées avec la même adresse : les ouvertes d'abord, puis des plus récentes aux plus anciennes."`
	Adherent         *candidate      `json:"adherent,omitempty"          jsonschema:"Le membre que l'adresse de la demande désigne dans la liste des membres ; absent quand elle n'y figure pas."`
}

// ticketID reads the id or the ref of a request.
func (c mcpCall) ticketID(ctx context.Context, raw string) (int64, error) {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return id, nil
	}
	if !refPattern.MatchString(raw) {
		return 0, refusalError("demande attendue : un identifiant numérique ou une référence sous la forme CPP-0042")
	}
	return c.s.tickets.IDByRef(ctx, raw)
}

func (c mcpCall) getTicket(ctx context.Context, in getTicketIn) (ticketJSON, error) {
	s := c.s
	id, err := c.ticketID(ctx, in.Demande)
	var (
		d      *tickets.Detail
		row    tickets.Row
		others []tickets.Row
	)
	if err == nil {
		d, err = s.tickets.Detail(ctx, id)
	}
	if err == nil {
		row, err = s.tickets.Row(ctx, id)
	}
	if err == nil {
		others, err = s.tickets.Others(ctx, id)
	}
	if errors.Is(err, tickets.ErrNotFound) {
		return ticketJSON{}, refusalError("aucune demande ne porte cet identifiant ou cette référence : cherche-la avec comite_list_tickets")
	}
	if err != nil {
		return ticketJSON{}, err
	}
	// Summary, SuggestedCategory and KBIDs are the app's own model output
	// and Captures are screenshots: none of them leaves by this tool.
	out := ticketJSON{Demande: s.ticketRowJSON(row), Email: d.Email, Description: d.Description,
		Champs: []champJSON{}, Messages: []messageJSON{}, Journal: []journalJSON{}, AutresDemandes: []ticketRowJSON{}}
	for _, f := range s.tickets.Catalog.Display(d.Fields) {
		out.Champs = append(out.Champs, champJSON{Champ: f.Label, Valeur: f.Value})
	}
	messages := d.Messages
	if len(messages) > ticketMessages {
		messages, out.MessagesTronques = messages[len(messages)-ticketMessages:], true
	}
	for _, m := range messages {
		author := "adhérent"
		if !m.FromMember {
			author = s.tickets.AccountName(m.Author)
		}
		out.Messages = append(out.Messages, messageJSON{Le: s.formatTime(m.CreatedAt), Auteur: author, Adherent: m.FromMember, Interne: m.Internal, Texte: m.Body})
	}
	for _, e := range d.Events {
		out.Journal = append(out.Journal, journalJSON{Le: s.formatTime(e.CreatedAt), Par: s.actorName(e.Actor), Evenement: s.tickets.Describe(e)})
	}
	for _, r := range others {
		out.AutresDemandes = append(out.AutresDemandes, s.ticketRowJSON(r))
	}
	matches, err := s.memberOf(ctx, d.Email)
	if err != nil {
		return ticketJSON{}, err
	}
	if len(matches) > 0 {
		m := candidateOf(matches[0], personHandle)
		out.Adherent = &m
	}
	return out, nil
}

type ticketStatsIn struct {
	Du string `json:"du,omitempty" jsonschema:"Premier mois, au format AAAA-MM. Sans valeur : onze mois avant au."`
	Au string `json:"au,omitempty" jsonschema:"Dernier mois, inclus, au format AAAA-MM. Sans valeur : le mois en cours."`
}

type statusCountJSON struct {
	Statut        string `json:"statut"         jsonschema:"todo, in_progress, waiting ou done."`
	StatutLibelle string `json:"statut_libelle" jsonschema:"À traiter, En cours, En attente de l'adhérent ou Fait."`
	Nombre        int    `json:"nombre"         jsonschema:"Demandes en place dans ce statut aujourd'hui."`
}

type monthStatJSON struct {
	Mois             string  `json:"mois"                          jsonschema:"Mois, au format AAAA-MM (heure de Paris)."`
	Categorie        string  `json:"categorie"                     jsonschema:"Identifiant de la catégorie."`
	CategorieLibelle string  `json:"categorie_libelle"             jsonschema:"Nom de la catégorie."`
	Deposees         int     `json:"deposees"                      jsonschema:"Demandes déposées ce mois et encore en place."`
	Closes           int     `json:"closes"                        jsonschema:"Demandes closes ce mois, supprimées ou purgées depuis comprises."`
	HeuresMoyennes   float64 `json:"heures_moyennes_de_traitement" jsonschema:"Délai moyen, en heures, du dépôt à la clôture des demandes closes ce mois ; 0 quand aucune ne l'a été."`
	Evitees          int     `json:"evitees"                       jsonschema:"Demandes qu'une fiche d'aide a résolues avant l'envoi, ce mois."`
}

type monthRangeJSON struct {
	Du string `json:"du" jsonschema:"Premier mois lu, AAAA-MM."`
	Au string `json:"au" jsonschema:"Dernier mois lu, inclus, AAAA-MM."`
}

type ticketStatsResult struct {
	Periode monthRangeJSON    `json:"periode"           jsonschema:"Mois que couvre « par_mois »."`
	EnPlace []statusCountJSON `json:"demandes_en_place" jsonschema:"Demandes présentes aujourd'hui par statut, quelle que soit la période."`
	ParMois []monthStatJSON   `json:"par_mois"          jsonschema:"Une ligne par mois et par catégorie où il s'est passé quelque chose, dans l'ordre des mois."`
	Note    string            `json:"note"              jsonschema:"Ce que les chiffres comptent et ne comptent pas."`
}

func (c mcpCall) ticketStats(ctx context.Context, in ticketStatsIn) (ticketStatsResult, error) {
	s := c.s
	month := func(raw string, def time.Time) (time.Time, error) {
		if raw == "" {
			return def, nil
		}
		return time.ParseInLocation(tickets.MonthLayout, raw, s.paris)
	}
	refused := refusalError("du et au : format AAAA-MM, du avant au")
	to, err := month(in.Au, s.now().In(s.paris))
	if err != nil {
		return ticketStatsResult{}, refused
	}
	from, err := month(in.Du, time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, s.paris).AddDate(0, 1-statsMonths, 0))
	if err != nil {
		return ticketStatsResult{}, refused
	}
	du, au := from.Format(tickets.MonthLayout), to.Format(tickets.MonthLayout)
	if du > au {
		return ticketStatsResult{}, refused
	}
	stats, err := s.tickets.Stats(ctx)
	if err != nil {
		return ticketStatsResult{}, err
	}
	out := ticketStatsResult{Periode: monthRangeJSON{Du: du, Au: au}, EnPlace: make([]statusCountJSON, 0, len(ticketStatuses)), ParMois: []monthStatJSON{},
		Note: "« deposees » ne compte que les demandes encore en place : une demande close puis supprimée, ou purgée " +
			plural(s.cfg.RetentionDays, "jour", "jours") + " après sa clôture, ne compte plus que dans « closes ». " +
			"« evitees » compte les demandes qu'une fiche d'aide a résolues avant l'envoi."}
	for _, st := range ticketStatuses {
		out.EnPlace = append(out.EnPlace, statusCountJSON{Statut: string(st), StatutLibelle: st.Label(), Nombre: stats.ByStatus[st]})
	}
	for _, m := range stats.Months {
		if m.Month < du || m.Month > au {
			continue
		}
		j := monthStatJSON{Mois: m.Month, Categorie: m.Category, CategorieLibelle: s.tickets.Catalog.CategoryLabel(m.Category),
			Deposees: m.Submitted, Closes: m.Closed, Evitees: m.Avoided}
		if m.Closed > 0 {
			j.HeuresMoyennes = float64(m.HoursToClose*10/int64(m.Closed)) / 10 // one decimal
		}
		out.ParMois = append(out.ParMois, j)
	}
	return out, nil
}
