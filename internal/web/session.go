package web

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// Session cookie (spec §11.2): __Host- prefix, so Secure, Path=/ and no
// Domain: it never reaches the members site or VPDive.
const (
	sessionCookieName = "__Host-session"
	sessionTTL        = 30 * 24 * time.Hour
	sessionRetention  = 24 * time.Hour
)

type session struct {
	account admins.Account
	hash    []byte // SHA-256 of the session token
}

func sessionFrom(ctx context.Context) (session, bool) {
	s, ok := ctx.Value(ctxSession).(session)
	return s, ok
}

// startSession stores a new session and sets its cookie. A new token is
// drawn at each login.
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, a admins.Account) error {
	token, err := secure.NewToken()
	if err != nil {
		return err
	}
	now := s.now()
	err = store.Tx(ctx, s.db, "session.create", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (token_hash, username, credential_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
			secure.TokenHash(token), a.Username, a.CredentialHash(), now.Unix(), now.Add(sessionTTL).Unix())
		return err
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", MaxAge: int(sessionTTL / time.Second),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// sessionOf returns the request's session. A session that expired, whose
// account left the accounts file, or whose password changed is deleted.
func (s *Server) sessionOf(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return session{}, false
	}
	ctx := r.Context()
	hash := secure.TokenHash(c.Value)
	var (
		username   string
		credential []byte
		expires    int64
	)
	err = s.db.QueryRowContext(ctx, `SELECT username, credential_hash, expires_at FROM sessions WHERE token_hash = ?`, hash).
		Scan(&username, &credential, &expires)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logger.ErrorContext(ctx, "read session", "error", err)
		}
		return session{}, false
	}
	a, ok := s.admins.Get(username)
	if !ok || subtle.ConstantTimeCompare(credential, a.CredentialHash()) != 1 || s.now().Unix() >= expires {
		s.deleteSession(ctx, hash)
		return session{}, false
	}
	return session{account: a, hash: hash}, true
}

func (s *Server) deleteSession(ctx context.Context, hash []byte) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash); err != nil {
		s.logger.ErrorContext(ctx, "delete session", "error", err)
	}
}

// signedIn lets only committee members through. A page request goes to the
// login page; any other request is refused.
func (s *Server) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.sessionOf(r)
		if !ok {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/connexion", http.StatusSeeOther)
				return
			}
			s.writeText(w, r, http.StatusForbidden, "Session expirée : reconnecte-toi.\n")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxSession, sess)))
	}
}

// csrfValid checks the anti-CSRF token bound to the request's session.
func (s *Server) csrfValid(r *http.Request, token string) bool {
	sess, ok := sessionFrom(r.Context())
	return ok && s.keys.CheckCSRF(sess.hash, token)
}

// RevokeSessions deletes the sessions of accounts removed from the accounts
// file or whose password changed, and closes their event streams (spec §4.1).
func (s *Server) RevokeSessions(ctx context.Context, usernames []string) error {
	err := store.Tx(ctx, s.db, "sessions.revoke", func(ctx context.Context, tx *sql.Tx) error {
		for _, u := range usernames {
			if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE username = ?`, u); err != nil {
				return fmt.Errorf("revoke sessions: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.broker.disconnect(func(sub *subscriber) bool { return slices.Contains(usernames, sub.username) })
	return nil
}

// Purge applies the retention of spec §8.3 to sessions (expired for a day)
// and to login counters. It runs from the daily background job.
func (s *Server) Purge(ctx context.Context) error {
	cutoff := s.now().Add(-sessionRetention).Unix()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, cutoff); err != nil {
		return fmt.Errorf("purge sessions: %w", err)
	}
	return s.limiter.purge(ctx)
}
