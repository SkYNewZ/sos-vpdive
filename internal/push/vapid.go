package push

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

const (
	// tokenLifetime stays under the one day push services accept.
	tokenLifetime = 12 * time.Hour
	// tokenRenewal: a token is replaced an hour before it expires, so it is
	// never refreshed more than once an hour (Apple's rule).
	tokenRenewal = time.Hour
)

// vapid signs the tokens that identify the server to push services (RFC
// 8292), one per push service origin, kept for their lifetime.
type vapid struct {
	cfg *config.VAPID
	now func() time.Time

	mu     sync.Mutex
	tokens map[string]signed
}

type signed struct {
	jwt     string
	expires time.Time
}

func newVAPID(cfg *config.VAPID, now func() time.Time) *vapid {
	return &vapid{cfg: cfg, now: now, tokens: map[string]signed{}}
}

// authorization returns the Authorization header for a push service origin.
func (v *vapid) authorization(audience string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	t, ok := v.tokens[audience]
	if !ok || !now.Before(t.expires.Add(-tokenRenewal)) {
		var err error
		if t, err = v.sign(audience, now.Add(tokenLifetime)); err != nil {
			return "", err
		}
		v.tokens[audience] = t
	}
	return "vapid t=" + t.jwt + ", k=" + v.cfg.PublicKey, nil
}

func (v *vapid) sign(audience string, expires time.Time) (signed, error) {
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{audience, expires.Unix(), v.cfg.Subject})
	if err != nil {
		return signed{}, fmt.Errorf("vapid claims: %w", err)
	}
	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, v.cfg.PrivateKey, sum[:])
	if err != nil {
		return signed{}, fmt.Errorf("vapid signature: %w", err)
	}
	sig := make([]byte, 64) // ES256 is r||s, 32 bytes each (RFC 7518 §3.4)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signed{jwt: unsigned + "." + enc.EncodeToString(sig), expires: expires}, nil
}
