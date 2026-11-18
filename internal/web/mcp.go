package web

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/assistant"
	"github.com/SkYNewZ/sos-vpdive/internal/payments"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

// The MCP server lets an assistant a committee member connected (Claude,
// ChatGPT) read the committee's data: tools only, no session, every answer
// in JSON (docs/development.md « MCP Server »). It runs none of the app's
// own model features; the client's model works on what the tools return.
const (
	mcpCallLimit    = 600 // requests per hour and connection
	mcpLicenceLimit = 30  // register lookups per hour and connection
	mcpBodyLimit    = 1 << 20
	mcpTouchEvery   = time.Minute // how often a connection's last use is written
)

// connectionKey is where a token's connection id sits in its TokenInfo.
const connectionKey = "connection"

// lastUsedKey is where its last use sits, as read with the token.
const lastUsedKey = "last_used"

// mcpHandler serves the MCP endpoint: a foreign Origin is refused, then the
// bearer token names the connection, whose calls are counted.
func (s *Server) mcpHandler() http.HandlerFunc {
	server := s.newMCPServer()
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true, // statusRecorder cannot flush a stream
		// The app listens on loopback behind its proxy under the site's own
		// host name, which this protection would refuse; route already
		// answers 404 to any other host.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        mcpBodyLimit,
	})
	bearer := auth.RequireBearerToken(s.verifyAccess, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.adminOrigin() + oauthResourceMeta,
		// verifyAccess checks the expiry itself, on the server's clock.
		AllowMissingExpiration: true,
	})
	next := bearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		info := auth.TokenInfoFromContext(ctx)
		id := connectionOf(info)
		ok, err := s.limiter.allow(ctx, "mcp:"+strconv.FormatInt(id, 10), mcpCallLimit, time.Hour)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if !ok {
			w.Header().Set("Retry-After", "3600")
			s.writeText(w, r, http.StatusTooManyRequests, "Trop d'appels pour cette connexion : réessaie dans une heure.\n")
			return
		}
		// A write only when the page's « utilisée le » would change.
		if used, _ := info.Extra[lastUsedKey].(int64); s.now().Unix()-used >= int64(mcpTouchEvery/time.Second) {
			if _, err := s.db.ExecContext(ctx, `UPDATE mcp_connections SET last_used_at = ? WHERE id = ?`, s.now().Unix(), id); err != nil {
				s.logger.ErrorContext(ctx, "touch connection", "error", err)
			}
		}
		streamable.ServeHTTP(w, r)
	}))
	origin := s.adminOrigin()
	return func(w http.ResponseWriter, r *http.Request) {
		// A browser on another site must not reach the endpoint (MCP
		// transport security); a client's server sends no Origin.
		if o := r.Header.Get("Origin"); o != "" && o != origin {
			s.writeText(w, r, http.StatusForbidden, "Requête refusée : origine non valide.\n")
			return
		}
		next.ServeHTTP(w, r)
	}
}

// connectionOf is the connection id verifyAccess put in info.
func connectionOf(info *auth.TokenInfo) int64 {
	if info == nil {
		return 0
	}
	id, _ := info.Extra[connectionKey].(int64)
	return id
}

// verifyAccess names the connection of an access token: one that is live,
// whose account still holds the password it was granted under.
func (s *Server) verifyAccess(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	if !secure.IsToken(token) {
		return nil, auth.ErrInvalidToken
	}
	var (
		id         int64
		username   string
		credential []byte
		scope      string
		expires    int64
		used       int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, credential_hash, scope, access_expires_at, last_used_at FROM mcp_connections WHERE access_hash = ?`, secure.TokenHash(token)).
		Scan(&id, &username, &credential, &scope, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, auth.ErrInvalidToken
	}
	if err != nil {
		if ctx.Err() != nil { // a client that left is expected: below Error, out of Sentry
			s.logger.WarnContext(ctx, "client left", "error", err)
		} else {
			s.logger.ErrorContext(ctx, "read connection", "error", err)
		}
		return nil, errors.New("internal error") // the SDK answers with the error's text
	}
	if _, live := s.admins.Current(username, credential); !live || s.now().Unix() >= expires {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{Scopes: strings.Fields(scope), UserID: username, Extra: map[string]any{connectionKey: id, lastUsedKey: used}}, nil
}

// mcpCall is one tool call of a connection.
type mcpCall struct {
	s          *Server
	t          *toolbox // the assistant's tool bodies; its sources go unused
	connection int64
}

// refusalError is a call the client got wrong, or one the data cannot answer:
// its text goes back as the tool's error, for the client's model to read.
type refusalError string

func (r refusalError) Error() string { return string(r) }

// errMCPInternal is all a client learns of a failure of ours.
var errMCPInternal = errors.New("erreur interne de l'outil : réessaie dans un instant")

// toolSpec describes a tool to its client. Scope is what the connection
// must have been granted to call it: read for every tool so far, and a tool
// that needs no more is announced as read-only.
type toolSpec struct {
	Name        string
	Title       string
	Description string
	Scope       string
}

// addTool registers a tool whose input and output schemas come from In and
// Out. It is the one place where a call is traced, logged by its name only,
// checked against the connection's scope, and where its result is masked:
// phone numbers, IBANs and RIBs never leave, addresses do.
func addTool[In, Out any](s *Server, server *mcp.Server, spec toolSpec, run func(mcpCall, context.Context, In) (Out, error)) {
	closed := false
	tool := &mcp.Tool{Name: spec.Name, Title: spec.Title, Description: spec.Description,
		Annotations: &mcp.ToolAnnotations{Title: spec.Title, ReadOnlyHint: spec.Scope == scopeRead, IdempotentHint: true, OpenWorldHint: &closed}}
	mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		ctx, span := s.tracer.Start(ctx, "mcp.tool", trace.WithAttributes(attribute.String("mcp.tool.name", spec.Name)))
		outcome := "ok" // or "refused", else the error code
		defer func() {
			span.SetAttributes(attribute.String("mcp.tool.outcome", outcome))
			span.End()
			s.logger.InfoContext(ctx, "mcp tool", "tool", spec.Name, "outcome", outcome)
		}()
		info := req.Extra.TokenInfo
		if info == nil || !slices.Contains(info.Scopes, spec.Scope) {
			outcome = outcomeRefused
			return nil, zero, refusalError("cette connexion n'a pas l'accès « " + spec.Scope + " » que demande l'outil")
		}
		out, err := run(mcpCall{s: s, t: &toolbox{s: s}, connection: connectionOf(info)}, ctx, in)
		if refusal, ok := errors.AsType[refusalError](err); ok {
			outcome = outcomeRefused
			return nil, zero, refusal
		}
		if err == nil {
			out, err = maskNumbers(out)
		}
		if err != nil {
			outcome = failureCode(ctx)
			telemetry.Fail(span, outcome)
			if ctx.Err() != nil {
				s.logger.WarnContext(ctx, "client left", "error", err)
			} else {
				s.logger.ErrorContext(ctx, "mcp tool failed", "tool", spec.Name, "error", err)
			}
			return nil, zero, errMCPInternal
		}
		return nil, out, nil
	})
}

// maskNumbers returns v with the phone numbers, IBANs and RIBs of its
// strings masked. Every imported text is free: a product, an outing title or
// a message may hold one.
func maskNumbers[T any](v T) (T, error) {
	var out T
	raw, err := json.Marshal(v)
	if err != nil {
		return out, fmt.Errorf("encode tool result: %w", err)
	}
	if raw, err = assistant.MaskJSONNumbers(raw); err != nil {
		return out, fmt.Errorf("mask tool result: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode tool result: %w", err)
	}
	return out, nil
}

// campaignHandle starts the handle of someone the membership campaign names
// and the members list does not: they have no address.
const campaignHandle = "campagne:"

// personHandle is how a client names p in its later calls: their address,
// or their name's keyed hash, which nobody can compute from a name.
func personHandle(p assistant.Person) string {
	if p.Email != "" {
		return p.Email
	}
	return campaignHandle + base64.RawURLEncoding.EncodeToString(p.NameHash)
}

// person reads a handle back. Nothing is kept between calls: the member or
// the campaign line is looked up again each time, so a homonym still reads
// nothing and a handle made up from a name opens nothing.
func (c mcpCall) person(ctx context.Context, handle string) (assistant.Person, error) {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return assistant.Person{}, refusalError("adherent attendu : la valeur « ref » d'un candidat de comite_find_member, ou l'adresse mail d'une demande")
	}
	if rest, ok := strings.CutPrefix(handle, campaignHandle); ok {
		return c.campaignPerson(ctx, rest)
	}
	email, err := secure.NormalizeEmail(handle)
	if err != nil {
		return assistant.Person{}, refusalError("adresse mail invalide : elle ne doit pas contenir d'espace")
	}
	matches, err := c.s.memberOf(ctx, email)
	if err != nil {
		return assistant.Person{}, err
	}
	if len(matches) == 0 {
		return assistant.Person{}, refusalError("aucun adhérent à cette adresse dans la liste des membres : cherche la personne par son nom avec comite_find_member")
	}
	m := matches[0]
	view := newProfileView(m.Profile)
	return assistant.Person{Name: strings.TrimSpace(m.FirstName + " " + m.LastName), Email: m.Email, NameHash: m.NameHash,
		Seasons: view.Seasons, Licence: view.Licence, Shared: m.Shared}, nil
}

// campaignPerson is the person of the campaign whose name hash is encoded.
func (c mcpCall) campaignPerson(ctx context.Context, encoded string) (assistant.Person, error) {
	unknown := refusalError("référence inconnue : cherche la personne avec comite_find_member")
	hash, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(hash) == 0 {
		return assistant.Person{}, unknown
	}
	n, err := c.s.members.NameCount(ctx, hash)
	if err != nil {
		return assistant.Person{}, err
	}
	if n > 0 {
		return assistant.Person{}, refusalError("cette personne est maintenant dans la liste des membres : cherche-la avec comite_find_member")
	}
	state, _, lines, err := c.s.campaign.NameLines(ctx, hash)
	if err != nil {
		return assistant.Person{}, err
	}
	p := assistant.Person{Name: "personne de la campagne d'adhésion", NameHash: hash}
	switch {
	case state == payments.BlockAmbiguous:
		p.Shared = 2 // a name the campaign holds twice: the tools read nothing
	case len(lines) == 0:
		return assistant.Person{}, unknown
	default:
		p.Name = strings.TrimSpace(lines[0].FirstName + " " + lines[0].LastName)
	}
	return p, nil
}
