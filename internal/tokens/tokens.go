// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package tokens holds the agent API bearer tokens (PLAN.md §6c, §5
// api_tokens).
//
// One token per agent so they revoke independently. The raw token is shown
// ONCE at creation; we store only HMAC-SHA256(token, pepper) and a short
// display prefix. Verification hashes the presented token and looks it up —
// constant-time is provided by the hash + unique-ish lookup; we never compare
// raw secrets directly.
//
//	token  = "pgs_" + base64url(24 random bytes)
//	stored = { prefix: first 12 chars, token_hash: hex HMAC-SHA256(token, pepper) }
//
// Scopes:
//
//	deploy (default) — agent: create drafts/pending, and on OPEN pages
//	                   publish/rollback. Never approve/reject/disable.
//	data_update      — optional caller-owned automation: only the managed-data
//	                   MCP tools, and only for exact slugs explicitly granted
//	                   by an operator.
//	admin            — reserved; admin actions still go through the
//	                   cookie+CSRF path, not bearer (PLAN.md §6, §10).
//
// The hash is the one lib/tokens.js stored, so every existing token verifies.
package tokens

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
)

// Scopes.
const (
	ScopeDeploy     = "deploy"
	ScopeDataUpdate = "data_update"
	ScopeAdmin      = "admin"
)

// Scopes is every valid scope.
var Scopes = []string{ScopeDeploy, ScopeDataUpdate, ScopeAdmin}

func validScope(s string) bool { return slices.Contains(Scopes, s) }

func legacyBroad(s string) bool { return s == ScopeDeploy || s == ScopeAdmin }

// DataUpdateTools is the whole reach of a data_update token. The narrowest
// scope that runs a daily refresh. record_refresh_check belongs here because
// the refresh it runs most often is the one that publishes NOTHING — source
// unchanged, tool unavailable, gate hit — and that is exactly the outcome
// nothing recorded. It cannot create or move a version, cannot touch data or
// any hash, and is still slug-gated like the other two, so it adds no
// authoring reach.
var DataUpdateTools = []string{"get_page_data", "update_page_data", "record_refresh_check"}

func pepper() string { return env.String("API_TOKEN_PEPPER", "") }

// Validate fails CLOSED in production: without the pepper an attacker with a
// DB dump can brute/rainbow the token hashes offline. Dev/test may run
// without it (with a warning). The server and `pages token` call this first.
func Validate() error {
	if pepper() != "" {
		return nil
	}
	if env.Production() {
		return errors.New("API_TOKEN_PEPPER is required in production (an empty pepper removes the offline-DB-dump defense for API tokens)")
	}
	slog.Warn("API_TOKEN_PEPPER is unset — API tokens are hashed with an empty pepper. " +
		"Fine for local dev; set it in production (bootstrap generates one).")
	return nil
}

// HashToken is hex(HMAC-SHA256(key=API_TOKEN_PEPPER, rawToken)), the stored
// token_hash.
func HashToken(rawToken string) string {
	m := hmac.New(sha256.New, []byte(pepper()))
	m.Write([]byte(rawToken))
	return hex.EncodeToString(m.Sum(nil))
}

var grantSlugRe = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*(?:/[a-z0-9]+(?:[-_][a-z0-9]+)*)*$`)

// NormalizeGrantSlug trims and lowercases slug and checks it is a valid page
// slug.
func NormalizeGrantSlug(slug string) (string, error) {
	normalized := strings.ToLower(jsstr.Trim(slug))
	if !grantSlugRe.MatchString(normalized) {
		return "", fmt.Errorf("bad allowed slug: %s", slug)
	}
	return normalized, nil
}

func normalizeAllowedSlugs(scope string, allowed []string) ([]string, error) {
	out := []string{}
	for _, s := range allowed {
		n, err := NormalizeGrantSlug(s)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	if scope != ScopeDataUpdate && len(out) > 0 {
		return nil, errors.New("exact slug grants are only valid for data_update tokens")
	}
	return out, nil
}

// MintParams describes a token to create.
type MintParams struct {
	Label string
	// Scope defaults to ScopeDeploy when empty.
	Scope        string
	AllowedSlugs []string
}

// Minted is a new token's metadata plus the one-time secret.
type Minted struct {
	ID           jsx.ID   `json:"id"`
	Label        string   `json:"label"`
	Scope        string   `json:"scope"`
	Prefix       string   `json:"prefix"`
	AllowedSlugs []string `json:"allowed_slugs"`
	// Token is the raw secret, returned only here and never stored.
	Token string `json:"token"`
}

// Mint creates a token row. Exact-slug grants bind to the page currently
// holding each slug when one exists; grants for a not-yet-created slug stay
// unbound and bind lazily at first use (Verify), and once bound they never
// re-bind (migrations/011).
func Mint(ctx context.Context, d *db.DB, p MintParams) (*Minted, error) {
	if p.Label == "" {
		return nil, errors.New("label is required")
	}
	scope := p.Scope
	if scope == "" {
		scope = ScopeDeploy
	}
	if !validScope(scope) {
		return nil, fmt.Errorf("bad scope: %s", scope)
	}
	slugs, err := normalizeAllowedSlugs(scope, p.AllowedSlugs)
	if err != nil {
		return nil, err
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	raw := "pgs_" + base64.RawURLEncoding.EncodeToString(b[:])
	m := &Minted{Label: p.Label, Scope: scope, Prefix: raw[:12], AllowedSlugs: slugs, Token: raw}
	err = d.WithTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO api_tokens (label, prefix, token_hash, scope)
			 VALUES ($1, $2, $3, $4) RETURNING id`,
			p.Label, m.Prefix, HashToken(raw), scope).Scan(&m.ID); err != nil {
			return err
		}
		if len(slugs) == 0 {
			return nil
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO api_token_page_grants (token_id, slug, page_id)
			 SELECT $1, s.slug, p.id
			   FROM unnest($2::text[]) AS s(slug)
			   LEFT JOIN pages p ON p.slug = s.slug AND p.deleted_at IS NULL`,
			m.ID, slugs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Token is a live token as Verify sees it.
type Token struct {
	ID    jsx.ID `json:"id"`
	Label string `json:"label"`
	Scope string `json:"scope"`
	// AllowedSlugs carries the grants that currently authorize a page.
	AllowedSlugs []string `json:"allowed_slugs"`
}

// Verify looks up a presented bearer token. It returns nil (no error) for a
// revoked or unknown token, and touches last_used_at as a side effect on
// success.
//
// AllowedSlugs carries the grants that currently authorize a page:
//   - bound grant (page_id set) → denied ONLY when a different live page now
//     holds the slug (delete→recreate rebinds nothing: the recreated page is a
//     new row). A deleted page with no live holder stays allowed and surfaces
//     page_not_found from the handler.
//   - unbound grant (page_id NULL — minted before the page existed) → stays
//     allowed and binds lazily to the first live page holding the slug, then
//     behaves as bound (never re-binds).
func Verify(ctx context.Context, d *db.DB, rawToken string) (*Token, error) {
	if !strings.HasPrefix(rawToken, "pgs_") {
		return nil, nil
	}
	t, err := loadActiveToken(ctx, d, "t.token_hash = $1", HashToken(rawToken))
	if err != nil || t == nil {
		return nil, err
	}
	// Best-effort and throttled: authentication is a hot path, so do not
	// create a row update/WAL write for every MCP call in a multi-step agent
	// run, and do not make the request wait for it.
	go func(id jsx.ID) {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = d.Exec(bg,
			`UPDATE api_tokens SET last_used_at = now()
			  WHERE id = $1
			    AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes')`, id)
	}(t.ID)
	return t, nil
}

var positiveIDRe = regexp.MustCompile(`^[1-9][0-9]*$`)

// VerifyID is the same live lookup by row id, for a credential Pages minted
// on a token's behalf (data exports). Revoking the token, or a grant no longer
// holding the page, must end what it minted exactly as it ends the token. It
// does not touch last_used_at: nothing presented the token itself.
func VerifyID(ctx context.Context, d *db.DB, tokenID string) (*Token, error) {
	if !positiveIDRe.MatchString(tokenID) {
		return nil, nil
	}
	return loadActiveToken(ctx, d, "t.id = $1::bigint", tokenID)
}

// predicate is one of the two literals above, never caller input.
func loadActiveToken(ctx context.Context, d *db.DB, predicate string, value string) (*Token, error) {
	var t Token
	var bindable []string
	err := d.QueryRow(ctx,
		`SELECT t.id, t.label, t.scope,
		        COALESCE(array_agg(g.slug ORDER BY g.slug) FILTER (
		          WHERE g.slug IS NOT NULL
		            AND (g.page_id IS NULL OR live.id IS NULL OR g.page_id = live.id)
		        ), ARRAY[]::text[]) AS allowed_slugs,
		        COALESCE(array_agg(g.slug ORDER BY g.slug) FILTER (
		          WHERE g.page_id IS NULL AND live.id IS NOT NULL
		        ), ARRAY[]::text[]) AS bindable_slugs
		   FROM api_tokens t
		   LEFT JOIN api_token_page_grants g ON g.token_id = t.id
		   LEFT JOIN pages live ON live.slug = g.slug AND live.deleted_at IS NULL
		  WHERE `+predicate+` AND t.revoked_at IS NULL
		  GROUP BY t.id, t.label, t.scope`, value).Scan(&t.ID, &t.Label, &t.Scope, &t.AllowedSlugs, &bindable)
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Persist lazy binds (guarded, idempotent) so the next request reads the
	// bound state directly. Never touches already-bound grants.
	if len(bindable) > 0 {
		if _, err := d.Exec(ctx,
			`UPDATE api_token_page_grants g SET page_id = live.id
			   FROM pages live
			  WHERE g.token_id = $1 AND g.slug = ANY($2::text[]) AND g.page_id IS NULL
			    AND live.slug = g.slug AND live.deleted_at IS NULL`, t.ID, bindable); err != nil {
			return nil, err
		}
	}
	return &t, nil
}

// Row is one token as List reports it.
type Row struct {
	ID           jsx.ID   `json:"id"`
	Label        string   `json:"label"`
	Prefix       string   `json:"prefix"`
	Scope        string   `json:"scope"`
	LastUsedAt   jsx.Time `json:"last_used_at"`
	CreatedAt    jsx.Time `json:"created_at"`
	RevokedAt    jsx.Time `json:"revoked_at"`
	AllowedSlugs []string `json:"allowed_slugs"`
}

// List returns every token, newest first.
func List(ctx context.Context, d *db.DB) ([]Row, error) {
	rows, err := d.Query(ctx,
		`SELECT t.id, t.label, t.prefix, t.scope, t.last_used_at, t.created_at, t.revoked_at,
		        COALESCE(array_agg(g.slug ORDER BY g.slug) FILTER (WHERE g.slug IS NOT NULL), ARRAY[]::text[]) AS allowed_slugs
		   FROM api_tokens t
		   LEFT JOIN api_token_page_grants g ON g.token_id = t.id
		  GROUP BY t.id, t.label, t.prefix, t.scope, t.last_used_at, t.created_at, t.revoked_at
		  ORDER BY t.created_at DESC`)
	if err != nil {
		return nil, err
	}
	out := []Row{}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Label, &r.Prefix, &r.Scope, &r.LastUsedAt, &r.CreatedAt, &r.RevokedAt, &r.AllowedSlugs); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Revoke revokes an active token and reports whether one was revoked.
func Revoke(ctx context.Context, d *db.DB, id int64) (bool, error) {
	tag, err := d.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ── HTTP ────────────────────────────────────────────────────────────────────

// Agent is the authenticated bearer identity a request carries.
type Agent struct {
	Actor        string // the token's label
	ActorType    string // always "agent"
	Scope        string
	TokenID      jsx.ID
	AllowedSlugs []string
}

// AgentFor builds the request identity for a verified token.
func AgentFor(t *Token) *Agent {
	slugs := t.AllowedSlugs
	if slugs == nil {
		slugs = []string{}
	}
	return &Agent{Actor: t.Label, ActorType: "agent", Scope: t.Scope, TokenID: t.ID, AllowedSlugs: slugs}
}

type agentKey struct{}

// WithAgent returns ctx carrying a.
func WithAgent(ctx context.Context, a *Agent) context.Context {
	return context.WithValue(ctx, agentKey{}, a)
}

// AgentFrom returns the agent RequireBearer put on the request context.
func AgentFrom(ctx context.Context) (*Agent, bool) {
	a, ok := ctx.Value(agentKey{}).(*Agent)
	return a, ok && a != nil
}

// ErrorHandler writes err (an *apierror.Error, or an unexpected failure) as
// the surface's own error envelope.
type ErrorHandler func(w http.ResponseWriter, r *http.Request, err error)

var bearerRe = regexp.MustCompile(`(?i)^Bearer\s+(.+)$`)

// RequireBearer validates `Authorization: Bearer <token>` and puts the Agent
// on the request context (AgentFrom). It answers 401 no_token / bad_token
// through onError otherwise. Bearer auth carries no ambient cookies, so it is
// CSRF-immune by construction (PLAN.md §7) — these routes never trust the SSO
// cookie.
func RequireBearer(d *db.DB, onError ErrorHandler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m := bearerRe.FindStringSubmatch(r.Header.Get("Authorization"))
			if m == nil {
				onError(w, r, apierror.Unauthorized("missing bearer token", "no_token", nil))
				return
			}
			t, err := Verify(r.Context(), d, m[1])
			if err != nil {
				onError(w, r, err)
				return
			}
			if t == nil {
				onError(w, r, apierror.Unauthorized("invalid or revoked token", "bad_token", nil))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithAgent(r.Context(), AgentFor(t))))
		})
	}
}

// RequireRestAccess admits only the legacy broad scopes (deploy, admin) to the
// REST API: scoped data_update tokens are intentionally MCP-only. Mount it
// after RequireBearer.
func RequireRestAccess(onError ErrorHandler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a, ok := AgentFrom(r.Context()); ok && legacyBroad(a.Scope) {
				next.ServeHTTP(w, r)
				return
			}
			onError(w, r, apierror.Forbidden("this bearer token may only use its scoped Pages MCP data tools", "token_scope_denied", nil))
		})
	}
}

// IsMcpToolAllowed reports whether agent's scope may call toolName at all.
func IsMcpToolAllowed(agent *Agent, toolName string) bool {
	if agent == nil || !validScope(agent.Scope) {
		return false
	}
	if legacyBroad(agent.Scope) {
		return true
	}
	return agent.Scope == ScopeDataUpdate && slices.Contains(DataUpdateTools, toolName)
}

// AuthorizeMcpTool returns a 403 *apierror.Error unless agent may call
// toolName with args. A data_update token additionally needs args["slug"] to
// be one of its exactly granted slugs.
func AuthorizeMcpTool(agent *Agent, toolName string, args map[string]any) error {
	if !IsMcpToolAllowed(agent, toolName) {
		scope := "unknown"
		if agent != nil && agent.Scope != "" {
			scope = agent.Scope
		}
		return apierror.Forbidden(fmt.Sprintf("token scope %s cannot call %s", scope, toolName), "tool_not_allowed", nil)
	}
	if agent.Scope != ScopeDataUpdate {
		return nil
	}
	raw, _ := args["slug"].(string)
	slug, err := NormalizeGrantSlug(raw)
	if err != nil {
		return apierror.Forbidden("data_update tools require an exactly granted page slug", "slug_not_allowed", nil)
	}
	if !slices.Contains(agent.AllowedSlugs, slug) {
		return apierror.Forbidden("this token is not authorized for page slug "+slug, "slug_not_allowed", nil)
	}
	return nil
}
