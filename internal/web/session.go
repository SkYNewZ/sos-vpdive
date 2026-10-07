package web

import (
	"context"
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
// drawn at each login and each password change. replaces, when set, is the
// session the new one takes over from: its push subscriptions move to the
// new one, then it ends.
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, a admins.Account, replaces []byte) error {
	token, err := secure.NewToken()
	if err != nil {
		return err
	}
	now, hash := s.now(), secure.TokenHash(token)
	err = store.Tx(ctx, s.db, "session.create", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (token_hash, username, credential_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
			hash, a.Username, a.CredentialHash(), now.Unix(), now.Add(sessionTTL).Unix()); err != nil || replaces == nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE push_subscriptions SET session_token_hash = ? WHERE session_token_hash = ?`, hash, replaces); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, replaces)
		return err
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", MaxAge: int(sessionTTL / time.Second),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	if replaces != nil {
		s.disconnectSession(replaces)
	}
	return nil
}

// disconnectSession closes the event streams of a session that ended.
func (s *Server) disconnectSession(hash []byte) {
	s.broker.disconnect(func(sub *subscriber) bool { return sub.session == string(hash) })
}

// sessionOf returns the request's session. A session that expired, whose
// account is gone, or whose password changed is deleted.
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
	a, ok := s.admins.Current(username, credential)
	if !ok || s.now().Unix() >= expires {
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
// login page; any other request is refused. A temporary password leads every
// page to « Mon compte » until it is changed.
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
		if sess.account.MustChangePassword && r.URL.Path != accountPath && r.URL.Path != "/deconnexion" {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, accountPath, http.StatusSeeOther)
				return
			}
			s.writeText(w, r, http.StatusForbidden, "Choisis d'abord ton mot de passe.\n")
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

// AccountsChanged is the accounts registry's OnChange: it ends the sessions
// of removed accounts and changed passwords, and returns the open requests
// of removed accounts to « à traiter » (spec §4.1).
func (s *Server) AccountsChanged(ctx context.Context) {
	if err := s.RevokeStale(ctx); err != nil {
		s.logger.ErrorContext(ctx, "revoke sessions", "error", err)
	}
	if err := s.tickets.ReleaseMissing(ctx, s.admins.Has); err != nil {
		s.logger.ErrorContext(ctx, "release requests of removed accounts", "error", err)
	}
}

// RevokeStale deletes the sessions whose account is gone or whose password
// changed, and closes their event streams. Their push subscriptions go with
// them (ON DELETE CASCADE). It runs at startup, for changes made while the
// service was stopped (reset-password, a restored backup), and after every
// account change.
func (s *Server) RevokeStale(ctx context.Context) error {
	type stored struct {
		hash       []byte
		username   string
		credential []byte
	}
	rows, err := s.db.QueryContext(ctx, `SELECT token_hash, username, credential_hash FROM sessions`)
	all, err := store.Collect(rows, err, func(rows *sql.Rows) (st stored, err error) {
		err = rows.Scan(&st.hash, &st.username, &st.credential)
		return st, err
	})
	if err != nil {
		return fmt.Errorf("read sessions: %w", err)
	}
	var gone []string
	err = store.Tx(ctx, s.db, "sessions.revoke_stale", func(ctx context.Context, tx *sql.Tx) error {
		gone = gone[:0]
		for _, st := range all {
			if _, ok := s.admins.Current(st.username, st.credential); ok {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, st.hash); err != nil {
				return fmt.Errorf("revoke stale session: %w", err)
			}
			gone = append(gone, string(st.hash))
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.broker.disconnect(func(sub *subscriber) bool { return slices.Contains(gone, sub.session) })
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
