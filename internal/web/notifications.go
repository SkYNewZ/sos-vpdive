package web

import (
	"net/http"

	"github.com/SkYNewZ/sos-vpdive/internal/push"
)

// notificationsData feeds templates/notifications.html.
type notificationsData struct {
	VAPIDKey   string // applicationServerKey for PushManager.subscribe; "" without Web Push
	Subscribed bool   // this session has a subscription
	Pushover   bool   // PUSHOVER_APP_TOKEN set
	PushoverOn bool   // this account has a user key
}

// notificationsPage lets a resolver turn push on for this device, and tells
// whether Pushover reaches them (spec §9.6, §6 as amended).
func (s *Server) notificationsPage(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	d := notificationsData{Pushover: s.cfg.PushoverToken != "", PushoverOn: sess.account.PushoverUserKey != ""}
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
	s.render(w, r, http.StatusOK, "notifications", p)
}

// subscribePush stores this device's subscription, tied to its session.
func (s *Server) subscribePush(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	f := r.PostForm
	sub, ok := push.ParseSubscription(s.cfg.PushAllowedHosts, f.Get("endpoint"), f.Get("p256dh"), f.Get("auth"))
	if !ok {
		s.writeText(w, r, http.StatusBadRequest, "Abonnement refusé : l'appareil a donné une adresse de notification non reconnue.\n")
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
