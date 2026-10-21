package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
)

func chatIdentity(a admins.Account) chatPerson {
	return chatPerson{Username: a.Username, Name: a.Name, Avatar: string(a.Avatar)}
}

func (s *Server) requireChatSession(w http.ResponseWriter, r *http.Request) (session, bool) {
	sess, ok, err := s.lookupSession(r)
	if err != nil {
		s.writeText(w, r, http.StatusServiceUnavailable, "Session momentanément indisponible : réessaie.\n")
		return session{}, false
	}
	if !ok || sess.account.MustChangePassword {
		s.writeText(w, r, http.StatusForbidden, "Session expirée : reconnecte-toi.\n")
		return session{}, false
	}
	return sess, true
}

func (s *Server) chatStatus(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireChatSession(w, r)
	if !ok {
		return
	}
	w.Header().Set("X-Chat-Session", s.keys.CSRFToken(sess.hash))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) chatSend(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireChatSession(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			s.writeText(w, r, http.StatusRequestEntityTooLarge, "Message trop volumineux.\n")
		} else {
			s.writeText(w, r, http.StatusBadRequest, "Message illisible.\n")
		}
		return
	}
	var input struct {
		CSRF string `json:"csrf"`
		Text string `json:"text"`
	}
	if !utf8.Valid(raw) || json.Unmarshal(raw, &input) != nil {
		s.writeText(w, r, http.StatusUnprocessableEntity, "Message illisible.\n")
		return
	}
	// Recheck after reading the body: an ended session must not send a message.
	sess, ok := s.requireChatSession(w, r)
	if !ok {
		return
	}
	if !s.keys.CheckCSRF(sess.hash, input.CSRF) {
		s.forbidCSRF(w, r)
		return
	}
	text := strings.TrimSpace(input.Text)
	if text == "" || utf8.RuneCountInString(text) > 2000 {
		s.writeText(w, r, http.StatusUnprocessableEntity, "Écris un message de 1 à 2 000 caractères.\n")
		return
	}
	msg, err := s.chat.send(string(sess.hash), chatIdentity(sess.account), text)
	switch {
	case errors.Is(err, errChatRateLimited):
		s.writeText(w, r, http.StatusTooManyRequests, "Attends quelques secondes avant d’envoyer un autre message.\n")
	case errors.Is(err, errChatUnavailable):
		s.writeText(w, r, http.StatusConflict, "Le chat se reconnecte. Réessaie après la reconnexion.\n")
	case err != nil:
		s.writeText(w, r, http.StatusServiceUnavailable, "Chat indisponible.\n")
	default:
		body, err := json.Marshal(msg)
		if err != nil {
			s.writeText(w, r, http.StatusInternalServerError, "Message illisible.\n")
			return
		}
		s.write(w, r, http.StatusOK, "application/json", body)
	}
}
