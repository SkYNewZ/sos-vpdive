package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/tickets"
)

type assistantHistory struct {
	Items          []assistant.HistoryItem
	Selected       string
	Previous, Next string
}

func historyPage(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 0 || n > 1_000_000 {
		return 0
	}
	return n
}
func (s *Server) history(r *http.Request, selected string) (assistantHistory, error) {
	sess, _ := sessionFrom(r.Context())
	page := historyPage(r)
	items, more, err := s.convs.List(r.Context(), sess.account.Username, page)
	if err != nil {
		return assistantHistory{}, err
	}
	h := assistantHistory{Items: items, Selected: selected}
	link := func(n int) string {
		q := url.Values{"page": {strconv.Itoa(n)}}
		if r.URL.Query().Get("nouvelle") == "1" {
			q.Set("nouvelle", "1")
		}
		return r.URL.Path + "?" + q.Encode()
	}
	if page > 0 {
		h.Previous = link(page - 1)
	}
	if more {
		h.Next = link(page + 1)
	}
	return h, nil
}

func (s *Server) conversationPanel(r *http.Request, c assistant.Conversation, csrf, left string) (assistantPanel, error) {
	p := assistantPanel{CSRF: csrf, Conversation: c.ID, TicketID: c.TicketID, Exchanges: views(c.Exchanges), Remaining: left}
	if c.ID == "" {
		return p, nil
	}
	p.Deadline = c.Expires().UnixMilli()
	if !c.CanResume(s.now()) {
		p.ReadOnly = "Cette conversation est terminée. Ouvre une nouvelle conversation pour poursuivre."
	}
	if c.TicketID != 0 {
		row, err := s.tickets.Row(r.Context(), c.TicketID)
		if err != nil && !errors.Is(err, tickets.ErrNotFound) {
			return p, err
		}
		if errors.Is(err, tickets.ErrNotFound) || !row.Status.Open() {
			p.ReadOnly = "Cette demande est close : l’assistant ne l’analyse plus."
		}
	}
	return p, nil
}

// generateAssistantTitle claims one bounded background call after the first saved exchange.
func (s *Server) generateAssistantTitle(ctx context.Context, c assistant.Conversation) {
	if s.summary == nil || len(c.Exchanges) != 1 {
		return
	}
	claimed, err := s.convs.ClaimTitle(ctx, c.Account, c.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "claim assistant title", "error", err)
		return
	}
	if !claimed {
		return
	}
	question, _ := assistant.Mask(c.Exchanges[0].Question, nil)
	ctx = context.WithoutCancel(ctx)
	s.titleJobs.Go(func() {
		start := time.Now()
		res, callErr := s.summary.Title(ctx, question)
		s.recordSuggestion(ctx, s.cfg.Summary, res, time.Since(start), callErr)
		if callErr != nil {
			return
		}
		if err := s.convs.SetTitle(ctx, c.Account, c.ID, res.Summary); err != nil {
			s.logger.ErrorContext(ctx, "save assistant title", "error", err)
		}
	})
}

// WaitAssistantTitles drains bounded title calls before the database closes.
func (s *Server) WaitAssistantTitles() { s.titleJobs.Wait() }
