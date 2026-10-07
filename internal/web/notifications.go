package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/push"
)

// notificationsData feeds templates/notifications.html.
type notificationsData struct {
	VAPIDKey   string // applicationServerKey for PushManager.subscribe; "" without Web Push
	Subscribed bool   // this session has a subscription
	Pushover   bool   // PUSHOVER_APP_TOKEN set
	PushoverOn bool   // this account has a user key

	PushoverError string // the key typed was refused
}

// notificationsPage lets a resolver turn push on for this device, and tells
// whether Pushover reaches them (spec §9.6, §6 as amended).
func (s *Server) notificationsPage(w http.ResponseWriter, r *http.Request) {
	s.renderNotifications(w, r, http.StatusOK, nil, "")
}

func (s *Server) renderNotifications(w http.ResponseWriter, r *http.Request, status int, n *notice, pushoverError string) {
	sess, _ := sessionFrom(r.Context())
	d := notificationsData{Pushover: s.cfg.PushoverToken != "", PushoverOn: sess.account.PushoverUserKey != "", PushoverError: pushoverError}
	if s.cfg.VAPID != nil {
		subscribed, err := s.push.HasSession(r.Context(), sess.hash)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d.VAPIDKey, d.Subscribed = s.cfg.VAPID.PublicKey, subscribed
	}
	p, err := s.adminPage(r, "Notifications")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Data = d
	if n != nil {
		p.Notices = append(p.Notices, *n)
	}
	s.render(w, r, status, "notifications", p)
}

// testPush sends a test notification to this device and says how it went,
// the push service's reason included (« M'envoyer une notification de test »).
func (s *Server) testPush(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	n := notice{Kind: noticeSuccess, Text: "Notification envoyée : elle doit s'afficher sur cet appareil dans quelques secondes."}
	switch err := s.pushTest(r.Context(), sess.hash); {
	case err == nil:
	case errors.Is(err, push.ErrNoSubscription):
		n = notice{Kind: noticeWarning, Text: "Les notifications ne sont pas activées sur cet appareil."}
	default:
		s.logger.WarnContext(r.Context(), "test push not delivered", "error", err)
		n = notice{Kind: noticeError, Text: "Le service de notification a refusé l'envoi. Détail technique : " + err.Error() + "."}
	}
	s.renderNotifications(w, r, http.StatusOK, &n, "")
}

// savePushover sets or removes the resolver's Pushover user key (spec §6 as
// amended). The key is sealed and never shown back.
func (s *Server) savePushover(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	key := strings.TrimSpace(r.PostForm.Get("cle"))
	if r.PostForm.Get("action") == "retirer" {
		key = ""
	}
	switch err := s.admins.SetPushoverKey(r.Context(), sess.account.Username, key); {
	case errors.Is(err, admins.ErrPushoverKey):
		s.renderNotifications(w, r, http.StatusUnprocessableEntity, nil,
			"Une clé Pushover fait 30 lettres et chiffres. Copie-la depuis pushover.net.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, "/notifications", http.StatusSeeOther)
	}
}

// subscribePush stores this device's subscription, tied to its session.
func (s *Server) subscribePush(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	f := r.PostForm
	sub, ok := push.ParseSubscription(s.cfg.PushAllowedHosts, f.Get("endpoint"), f.Get("p256dh"), f.Get("auth"))
	if !ok {
		s.writeText(w, r, http.StatusBadRequest, "Abonnement refusé : le site ne reconnaît pas l'abonnement envoyé par ton appareil.\n")
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.push.Save(r.Context(), sess.hash, sess.account.Username, sub); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// unsubscribePush removes this device's subscription (« Désactiver »).
func (s *Server) unsubscribePush(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.push.DeleteSession(r.Context(), sess.hash); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
