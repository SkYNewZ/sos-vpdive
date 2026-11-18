package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/SkYNewZ/sos-vpdive/internal/admins"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/store"
)

// connectionsPath lists the applications an account let read the committee's
// data, under « Mon compte ».
const connectionsPath = accountPath + "/connexions"

// tokenPair are the tokens of a connection, as the client receives them
// once: the database keeps their hashes.
type tokenPair struct {
	access  string
	refresh string
}

func newTokenPair() (tokenPair, error) {
	access, err := secure.NewToken()
	if err != nil {
		return tokenPair{}, err
	}
	refresh, err := secure.NewToken()
	return tokenPair{access: access, refresh: refresh}, err
}

// createConnection opens the connection of a to the client of req. Like a
// session, it holds while the account keeps its password.
func (s *Server) createConnection(ctx context.Context, a admins.Account, req oauthRequest) (tokenPair, error) {
	pair, err := newTokenPair()
	if err != nil {
		return tokenPair{}, err
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO mcp_connections (username, credential_hash, client_id, client_name, scope,
		   access_hash, access_expires_at, refresh_hash, created_at, last_used_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Username, a.CredentialHash(), req.clientID, req.clientName, scopeRead,
		secure.TokenHash(pair.access), now.Add(accessTTL).Unix(), secure.TokenHash(pair.refresh),
		now.Unix(), now.Unix(), now.Add(connectionTTL).Unix())
	if err != nil {
		return tokenPair{}, fmt.Errorf("create connection: %w", err)
	}
	return pair, nil
}

// rotateConnection gives the connection of refresh new tokens, for another
// 30 days. The token it replaces works no more: if it comes back later than
// a crossed refresh would, someone else holds it and the connection ends.
func (s *Server) rotateConnection(ctx context.Context, refresh, clientID string) (tokenPair, error) {
	if !secure.IsToken(refresh) {
		return tokenPair{}, errInvalidGrant
	}
	pair, err := newTokenPair()
	if err != nil {
		return tokenPair{}, err
	}
	hash, now, refused := secure.TokenHash(refresh), s.now(), false
	err = store.Tx(ctx, s.db, "mcp.refresh", func(ctx context.Context, tx *sql.Tx) error {
		var (
			id               int64
			username, client string
			credential       []byte
			expires          int64
			retired          sql.NullInt64 // when hash was replaced; NULL for the token in force
		)
		err := tx.QueryRowContext(ctx,
			`SELECT c.id, c.username, c.credential_hash, c.client_id, c.expires_at, r.retired_at
			 FROM mcp_connections c LEFT JOIN mcp_retired_tokens r ON r.connection_id = c.id AND r.refresh_hash = ?
			 WHERE c.refresh_hash = ? OR r.refresh_hash IS NOT NULL`, hash, hash).
			Scan(&id, &username, &credential, &client, &expires, &retired)
		if errors.Is(err, sql.ErrNoRows) {
			refused = true
			return nil
		}
		if err != nil {
			return err
		}
		_, live := s.admins.Current(username, credential)
		refused = true
		switch {
		case client != clientID, retired.Valid && now.Before(time.Unix(retired.Int64, 0).Add(refreshLeeway)):
			return nil
		case retired.Valid, !live, now.Unix() >= expires:
			_, err := tx.ExecContext(ctx, `DELETE FROM mcp_connections WHERE id = ?`, id)
			return err
		}
		refused = false
		if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_retired_tokens (refresh_hash, connection_id, retired_at) VALUES (?, ?, ?)`,
			hash, id, now.Unix()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE mcp_connections SET access_hash = ?, access_expires_at = ?, refresh_hash = ?, expires_at = ?, last_used_at = ? WHERE id = ?`,
			secure.TokenHash(pair.access), now.Add(accessTTL).Unix(), secure.TokenHash(pair.refresh),
			now.Add(connectionTTL).Unix(), now.Unix(), id)
		return err
	})
	if err != nil {
		return tokenPair{}, fmt.Errorf("refresh connection: %w", err)
	}
	if refused {
		return tokenPair{}, errInvalidGrant
	}
	return pair, nil
}

// revokeStaleConnections deletes the connections whose account is gone or
// whose password changed, as RevokeStale does for sessions.
func (s *Server) revokeStaleConnections(ctx context.Context) error {
	type stored struct {
		id         int64
		username   string
		credential []byte
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, credential_hash FROM mcp_connections`)
	all, err := store.Collect(rows, err, func(rows *sql.Rows) (st stored, err error) {
		err = rows.Scan(&st.id, &st.username, &st.credential)
		return st, err
	})
	if err != nil {
		return fmt.Errorf("read connections: %w", err)
	}
	for _, st := range all {
		if _, ok := s.admins.Current(st.username, st.credential); ok {
			continue
		}
		// Only while it still holds the credential read above.
		if _, err := s.db.ExecContext(ctx, `DELETE FROM mcp_connections WHERE id = ? AND credential_hash = ?`, st.id, st.credential); err != nil {
			return fmt.Errorf("revoke stale connection: %w", err)
		}
	}
	return nil
}

// connectionRow is a connection as « Connexions » lists it.
type connectionRow struct {
	ID       int64
	Name     string
	Host     string // of the client id document
	Access   string
	Created  time.Time
	LastUsed time.Time
}

// accessLabel names what a connection's scope lets its application do.
var accessLabel = map[string]string{scopeRead: "Lecture seule"}

// connectionsOf lists the connections of username, the latest first.
func (s *Server) connectionsOf(ctx context.Context, username string) ([]connectionRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, client_id, client_name, scope, created_at, last_used_at FROM mcp_connections
		 WHERE username = ? ORDER BY created_at DESC, id DESC`, username)
	out, err := store.Collect(rows, err, func(rows *sql.Rows) (c connectionRow, err error) {
		var (
			clientID, scope string
			created, used   int64
		)
		if err = rows.Scan(&c.ID, &clientID, &c.Name, &scope, &created, &used); err != nil {
			return c, err
		}
		c.Host, c.Access = clientID, accessLabel[scope]
		if u, err := url.Parse(clientID); err == nil && u.Hostname() != "" {
			c.Host = u.Hostname()
		}
		c.Created, c.LastUsed = time.Unix(created, 0), time.Unix(used, 0)
		return c, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read connections: %w", err)
	}
	return out, nil
}

// connectionsData feeds templates/connexions.html.
type connectionsData struct {
	URL         string // of the MCP server, to paste into the application
	Connections []connectionRow
}

func (s *Server) connectionsPage(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	rows, err := s.connectionsOf(r.Context(), sess.account.Username)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p, err := s.adminPage(r, "Connexions")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if r.URL.Query().Get("revoquee") == "1" {
		p.Notices = append(p.Notices, notice{Kind: noticeSuccess, Text: "Accès retiré. L'application ne peut plus lire les données."})
	}
	p.Data = connectionsData{URL: s.mcpURL(), Connections: rows}
	s.render(w, r, http.StatusOK, "connexions", p)
}

// revokeConnection ends one connection of the signed-in account: its tokens
// work no more.
func (s *Server) revokeConnection(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	sess, _ := sessionFrom(r.Context())
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM mcp_connections WHERE id = ? AND username = ?`, id, sess.account.Username); err != nil {
		s.serverError(w, r, fmt.Errorf("revoke connection: %w", err))
		return
	}
	http.Redirect(w, r, connectionsPath+"?revoquee=1", http.StatusSeeOther)
}
