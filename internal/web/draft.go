package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/kb"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/suggest"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

// Screen 2 « Avant d'envoyer » (spec §3.2, §3.3): the fiches the model chose,
// « Ça règle mon problème » and « Envoyer ma demande quand même ». Every
// action carries the draft token in its body.
const draftField = "brouillon"

type beforeData struct {
	Token       string
	Fiches      []kb.Fiche
	OpenRequest bool
}

// chooser asks the model about a stored draft, within the daily cap of spec
// §5.5; nil without a model. Any failure files the request without
// suggestions.
func (s *Server) chooser(sub tickets.Submission) tickets.Chooser {
	if s.suggest == nil {
		return nil
	}
	return func(ctx context.Context) (tickets.Suggestion, bool) {
		// The key carries the day in Paris: the count starts again at midnight there.
		day := s.now().In(s.paris).Format(time.DateOnly)
		allowed, err := s.limiter.allow(ctx, "llm-day:"+day, s.cfg.LLM.DailyLimit, counterRetention)
		switch {
		case err != nil:
			s.logger.ErrorContext(ctx, "model daily cap", "error", err)
			return tickets.Suggestion{}, false
		case !allowed:
			s.logger.InfoContext(ctx, "model daily cap reached, request sent without suggestions")
			return tickets.Suggestion{}, false
		}
		start := time.Now()
		res, err := s.suggest.Choose(ctx, s.suggestRequest(sub), s.fiches)
		s.recordSuggestion(ctx, res, time.Since(start), err)
		if err != nil {
			s.logger.WarnContext(ctx, "model call failed, request sent without suggestions", "error", err)
			return tickets.Suggestion{}, false
		}
		return tickets.Suggestion{KBIDs: res.IDs, Summary: res.Summary}, true
	}
}

// suggestRequest is what the model reads: category, dedicated fields and
// description, never the identity fields (spec §5.2).
func (s *Server) suggestRequest(sub tickets.Submission) suggest.Request {
	req := suggest.Request{Category: s.tickets.Catalog.CategoryLabel(sub.Fields.Category), Description: sub.Description}
	for _, f := range s.tickets.Catalog.Display(sub.Fields) {
		req.Fields = append(req.Fields, suggest.Field{Label: f.Label, Value: f.Value})
	}
	return req
}

// renderBefore shows screen 2 for draft d, behind its token; n, when set,
// says what the last action did.
// ponytail: a redeploy that removes every fiche of a draft leaves screen 2
// with an empty list (rare, accepted by the owner); hide the list and « Ça
// règle mon problème » if it ever shows up.
func (s *Server) renderBefore(w http.ResponseWriter, r *http.Request, status int, token string, d tickets.Draft, n *notice) {
	data := beforeData{Token: token, OpenRequest: d.OpenRequest}
	for _, id := range d.KBIDs {
		if f, ok := s.kb.Get(id); ok {
			data.Fiches = append(data.Fiches, f)
		}
	}
	p := s.newPage(r, "Avant d'envoyer")
	if n != nil {
		p.Notices = append(p.Notices, *n)
	}
	p.Data = data
	s.render(w, r, status, "before", p)
}

// draft finds the draft of token. It has answered when ok is false: the
// draft is gone, or already filed (its confirmation).
func (s *Server) draft(w http.ResponseWriter, r *http.Request, token string) (tickets.Draft, bool) {
	d, err := s.tickets.DraftByToken(r.Context(), token)
	switch {
	case errors.Is(err, tickets.ErrDraftGone):
		s.draftGone(w, r)
	case err != nil:
		s.serverError(w, r, err)
	case d.Ref != "":
		redirectSent(w, r, d.Ref)
	default:
		return d, true
	}
	return tickets.Draft{}, false
}

func (s *Server) draftGone(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusGone, "draft_gone", s.newPage(r, "Demande expirée"))
}

// draftToken reads the token of a screen 2 form. It has answered when ok is
// false.
func (s *Server) draftToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyLimit)
	if err := r.ParseForm(); err != nil {
		s.writeText(w, r, http.StatusBadRequest, "Formulaire illisible. Réessaie.\n")
		return "", false
	}
	token := r.PostForm.Get(draftField)
	if !secure.IsToken(token) {
		s.draftGone(w, r)
		return "", false
	}
	return token, true
}

// confirmDraft is « Envoyer ma demande quand même ». Confirming twice shows
// the same confirmation (spec §3.2).
func (s *Server) confirmDraft(w http.ResponseWriter, r *http.Request) {
	token, ok := s.draftToken(w, r)
	if !ok {
		return
	}
	ref, err := s.tickets.ConfirmDraft(r.Context(), token)
	switch {
	case errors.Is(err, tickets.ErrDraftGone):
		s.draftGone(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		redirectSent(w, r, ref)
	}
}

// abandonDraft is « Ça règle mon problème »: no request, one avoided request
// counted. A request already filed keeps its confirmation.
func (s *Server) abandonDraft(w http.ResponseWriter, r *http.Request) {
	token, ok := s.draftToken(w, r)
	if !ok {
		return
	}
	ref, err := s.tickets.Abandon(r.Context(), token)
	switch {
	case errors.Is(err, tickets.ErrDraftGone):
		s.draftGone(w, r)
	case err != nil:
		s.serverError(w, r, err)
	case ref != "":
		redirectSent(w, r, ref)
	default:
		http.Redirect(w, r, "/demandes/abandonnee", http.StatusSeeOther)
	}
}

func (s *Server) solvedPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "solved", s.newPage(r, "Tant mieux"))
}

// resendDraftLink mails again the tracking links of the address, for the
// member who has a request in progress (spec §3.3), within the limits of
// the lost-link page. Screen 2 comes back with the outcome.
func (s *Server) resendDraftLink(w http.ResponseWriter, r *http.Request) {
	token, ok := s.draftToken(w, r)
	if !ok {
		return
	}
	d, ok := s.draft(w, r, token)
	if !ok {
		return
	}
	if !d.OpenRequest {
		s.renderBefore(w, r, http.StatusOK, token, d, nil)
		return
	}
	ctx := r.Context()
	allowed, err := s.limiter.allowAll(ctx, s.recoverIPRule(r), recoverEmailRule(d.Email))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !allowed {
		s.renderBefore(w, r, http.StatusTooManyRequests, token, d, &notice{Kind: noticeError, Text: tooManyLinksText})
		return
	}
	if err := s.tickets.SendLinks(ctx, d.Email); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderBefore(w, r, http.StatusOK, token, d, &notice{Kind: noticeSuccess,
		Text: "Le lien de suivi de ta demande en cours part par mail. Regarde aussi dans les indésirables."})
}
