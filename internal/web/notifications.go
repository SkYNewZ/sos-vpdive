package web

import (
	"crypto/ecdh"
	"encoding/base64"
	"net/http"
	"net/url"

	"github.com/SkYNewZ/sos-vpdive/internal/push"
)

// maxEndpoint bounds the push address a browser may register.
const maxEndpoint = 2048

// notificationsData feeds templates/notifications.html.
type notificationsData struct {
	WebPush    bool   // VAPID keys configured
	VAPIDKey   string // applicationServerKey for PushManager.subscribe
	Subscribed bool   // this session has a subscription
	Pushover   bool   // PUSHOVER_APP_TOKEN set
	PushoverOn bool   // this account has a user key
}

// notificationsPage lets a resolver turn push on for this device, and tells
// whether Pushover reaches them (spec §9.6, §6 as amended).
func (s *Server) notificationsPage(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	d := notificationsData{
		Pushover:   s.cfg.PushoverToken != "",
		PushoverOn: s.cfg.PushoverToken != "" && sess.account.PushoverUserKey != "",
	}
	if s.cfg.VAPID != nil {
		subscribed, err := s.push.HasSession(r.Context(), sess.hash)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d.WebPush, d.VAPIDKey, d.Subscribed = true, s.cfg.VAPID.PublicKey, subscribed
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
	if s.cfg.VAPID == nil {
		s.notFound(w, r)
		return
	}
	if !s.postForm(w, r) {
		return
	}
	sub, ok := parseSubscription(r.PostForm, s.cfg.PushAllowedHosts)
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
	if s.cfg.VAPID == nil {
		s.notFound(w, r)
		return
	}
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

// parseSubscription checks what PushManager gave: an HTTPS endpoint on an
// allowed push service, a P-256 public key and a 16-byte auth secret.
func parseSubscription(v url.Values, hosts []string) (push.Subscription, bool) {
	endpoint := v.Get("endpoint")
	u, err := url.Parse(endpoint)
	if err != nil || len(endpoint) > maxEndpoint || u.Scheme != "https" || u.User != nil || !push.HostAllowed(hosts, u.Hostname()) {
		return push.Subscription{}, false
	}
	p256dh, err := base64.RawURLEncoding.DecodeString(v.Get("p256dh"))
	if err != nil {
		return push.Subscription{}, false
	}
	if _, err := ecdh.P256().NewPublicKey(p256dh); err != nil {
		return push.Subscription{}, false
	}
	auth, err := base64.RawURLEncoding.DecodeString(v.Get("auth"))
	if err != nil || len(auth) != 16 {
		return push.Subscription{}, false
	}
	return push.Subscription{Endpoint: endpoint, P256DH: p256dh, Auth: auth}, true
}
