// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package tokens

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/testdb"
)

func TestHashTokenMatchesNode(t *testing.T) {
	t.Setenv("API_TOKEN_PEPPER", "pep")
	// crypto.createHmac("sha256", "pep").update("pgs_abc").digest("hex")
	if got := HashToken("pgs_abc"); got != "9816e2e3c173bbc011e68396f466cc19cae8ae2b5309cc5e626141a762ef9f85" {
		t.Fatalf("hash %s", got)
	}
}

func TestValidate(t *testing.T) {
	t.Setenv("API_TOKEN_PEPPER", "")
	t.Setenv("PAGES_ENV", "")
	t.Setenv("NODE_ENV", "")
	if Validate() != nil {
		t.Fatal("dev tolerates an empty pepper")
	}
	t.Setenv("NODE_ENV", "production")
	if err := Validate(); err == nil || !strings.Contains(err.Error(), "API_TOKEN_PEPPER is required in production") {
		t.Fatalf("production must fail closed: %v", err)
	}
	t.Setenv("API_TOKEN_PEPPER", "p")
	if Validate() != nil {
		t.Fatal("configured")
	}
}

func TestNormalizeGrantSlug(t *testing.T) {
	bom, nbsp := string(rune(0xFEFF)), string(rune(0xA0))
	for in, want := range map[string]string{"\tNorthwind/Q2": "northwind/q2", bom + "a-b_c" + nbsp: "a-b_c", "x": "x"} {
		if got, err := NormalizeGrantSlug(in); err != nil || got != want {
			t.Errorf("%q → %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "a//b", "-a", "a-", "a b", "a/", "../x", "é"} {
		if _, err := NormalizeGrantSlug(bad); err == nil || err.Error() != "bad allowed slug: "+bad {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// A data_update token must never gain authoring reach: its allowlist is
// exactly the data tools.
func TestScopeToolMatrix(t *testing.T) {
	dataReach := map[string]bool{"get_page_data": true, "update_page_data": true, "record_refresh_check": true}
	for _, name := range []string{"get_page_data", "update_page_data", "record_refresh_check", "deploy_page", "publish_version",
		"list_templates", "get_template", "register_template_upload", "create_page_from_template", "delete_page"} {
		if got := IsMcpToolAllowed(&Agent{Scope: ScopeDataUpdate}, name); got != dataReach[name] {
			t.Errorf("data_update %s = %v", name, got)
		}
		if !IsMcpToolAllowed(&Agent{Scope: ScopeDeploy}, name) || !IsMcpToolAllowed(&Agent{Scope: ScopeAdmin}, name) {
			t.Errorf("broad scopes keep %s", name)
		}
		if IsMcpToolAllowed(nil, name) || IsMcpToolAllowed(&Agent{Scope: "root"}, name) {
			t.Errorf("unknown scope allowed %s", name)
		}
	}
}

func code(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	e, ok := apierror.As(err)
	if !ok || e.Status != 403 {
		t.Fatalf("want 403 apierror, got %v", err)
	}
	return e.Code
}

func TestAuthorizeMcpTool(t *testing.T) {
	scoped := &Agent{Scope: ScopeDataUpdate, AllowedSlugs: []string{"mcpdata"}}
	if c := code(t, AuthorizeMcpTool(scoped, "get_page_data", map[string]any{"slug": "MCPData "})); c != "" {
		t.Fatalf("granted slug denied: %s", c)
	}
	if c := code(t, AuthorizeMcpTool(scoped, "get_page_data", map[string]any{"slug": "mcpgateddata"})); c != "slug_not_allowed" {
		t.Fatal(c)
	}
	err := AuthorizeMcpTool(scoped, "get_page_data", map[string]any{"slug": "mcpgateddata"})
	if err.Error() != "this token is not authorized for page slug mcpgateddata" {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{nil, {}, {"slug": 7}, {"slug": "../x"}} {
		err := AuthorizeMcpTool(scoped, "update_page_data", args)
		if code(t, err) != "slug_not_allowed" || err.Error() != "data_update tools require an exactly granted page slug" {
			t.Fatalf("%v: %v", args, err)
		}
	}
	err = AuthorizeMcpTool(scoped, "deploy_page", nil)
	if code(t, err) != "tool_not_allowed" || err.Error() != "token scope data_update cannot call deploy_page" {
		t.Fatal(err)
	}
	if err := AuthorizeMcpTool(nil, "x", nil); err.Error() != "token scope unknown cannot call x" {
		t.Fatal(err)
	}
	if err := AuthorizeMcpTool(&Agent{Scope: ScopeDeploy}, "deploy_page", nil); err != nil {
		t.Fatal(err)
	}
}

func TestMintValidation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		p    MintParams
		want string
	}{
		{MintParams{}, "label is required"},
		{MintParams{Label: "x", Scope: "root"}, "bad scope: root"},
		{MintParams{Label: "x", AllowedSlugs: []string{"a"}}, "exact slug grants are only valid for data_update tokens"},
		{MintParams{Label: "x", Scope: ScopeDataUpdate, AllowedSlugs: []string{"a b"}}, "bad allowed slug: a b"},
	} {
		// Validation runs before any database access.
		if _, err := Mint(ctx, nil, tc.p); err == nil || err.Error() != tc.want {
			t.Errorf("%+v: %v", tc.p, err)
		}
	}
}

func insertPage(t *testing.T, d *db.DB, slug string) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRow(context.Background(), `INSERT INTO pages (slug) VALUES ($1) RETURNING id`, slug).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func softDelete(t *testing.T, d *db.DB, id int64) {
	t.Helper()
	if _, err := d.Exec(context.Background(), `UPDATE pages SET deleted_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestMintVerifyListRevoke(t *testing.T) {
	t.Setenv("API_TOKEN_PEPPER", "integration-pepper")
	d := testdb.New(t)
	ctx := context.Background()

	m, err := Mint(ctx, d, MintParams{Label: "northwind-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^pgs_[A-Za-z0-9_-]{32}$`).MatchString(m.Token) || m.Prefix != m.Token[:12] || m.Scope != ScopeDeploy ||
		m.ID == 0 || len(m.AllowedSlugs) != 0 {
		t.Fatalf("minted %+v", m)
	}
	var stored string
	if err := d.QueryRow(ctx, `SELECT token_hash FROM api_tokens WHERE id = $1`, m.ID).Scan(&stored); err != nil || stored != HashToken(m.Token) {
		t.Fatalf("stored hash %q %v", stored, err)
	}

	got, err := Verify(ctx, d, m.Token)
	if err != nil || got == nil || got.ID != m.ID || got.Label != "northwind-agent" || got.Scope != ScopeDeploy || len(got.AllowedSlugs) != 0 {
		t.Fatalf("verify %+v %v", got, err)
	}
	for _, bad := range []string{"", "nope", "pgs_unknown", m.Token + "x"} {
		if v, err := Verify(ctx, d, bad); v != nil || err != nil {
			t.Errorf("%q verified: %+v %v", bad, v, err)
		}
	}
	if v, _ := VerifyID(ctx, d, m.ID.String()); v == nil || v.ID != m.ID {
		t.Fatal("verifyId")
	}
	for _, bad := range []string{"", "0", "01", "-1", "1.5", "abc"} {
		if v, err := VerifyID(ctx, d, bad); v != nil || err != nil {
			t.Errorf("VerifyID(%q) = %+v %v", bad, v, err)
		}
	}

	scoped, err := Mint(ctx, d, MintParams{Label: "daily-mcpdata", Scope: ScopeDataUpdate, AllowedSlugs: []string{"mcpdata", " McpData", "alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scoped.AllowedSlugs, []string{"alpha", "mcpdata"}) {
		t.Fatalf("normalized, deduplicated, sorted: %v", scoped.AllowedSlugs)
	}
	rows, err := List(ctx, d)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list %v %v", rows, err)
	}
	var found bool
	for _, r := range rows {
		if r.ID == scoped.ID {
			found = reflect.DeepEqual(r.AllowedSlugs, []string{"alpha", "mcpdata"}) && r.RevokedAt.IsZero() && !r.CreatedAt.IsZero()
		}
	}
	if !found {
		t.Fatalf("scoped row %+v", rows)
	}

	ok, err := Revoke(ctx, d, int64(scoped.ID))
	if err != nil || !ok {
		t.Fatal("revoke")
	}
	if ok, _ := Revoke(ctx, d, int64(scoped.ID)); ok {
		t.Fatal("revoking twice reports nothing revoked")
	}
	if v, _ := Verify(ctx, d, scoped.Token); v != nil {
		t.Fatal("revocation is immediate")
	}
	if v, _ := VerifyID(ctx, d, scoped.ID.String()); v != nil {
		t.Fatal("revocation ends minted credentials too")
	}
}

// Grants bind to page IDENTITY, not slug text (issue #50). A grant minted
// before its page exists binds lazily to the first live holder of the slug;
// after delete→recreate (a NEW page row) the stale grant must NOT follow the
// slug; delete→restore (the SAME row) keeps working.
func TestGrantsBindToPageIdentity(t *testing.T) {
	t.Setenv("API_TOKEN_PEPPER", "integration-pepper")
	d := testdb.New(t)
	ctx := context.Background()
	allowed := func(tok string) []string {
		t.Helper()
		v, err := Verify(ctx, d, tok)
		if err != nil || v == nil {
			t.Fatalf("verify: %v", err)
		}
		return v.AllowedSlugs
	}

	rebind, err := Mint(ctx, d, MintParams{Label: "rebind-watch", Scope: ScopeDataUpdate, AllowedSlugs: []string{"grantrebind"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(allowed(rebind.Token), []string{"grantrebind"}) {
		t.Fatal("unbound grant stays allowed")
	}
	first := insertPage(t, d, "grantrebind")
	if !reflect.DeepEqual(allowed(rebind.Token), []string{"grantrebind"}) {
		t.Fatal("grant lazily binds to the first live page")
	}
	var bound *int64
	if err := d.QueryRow(ctx, `SELECT page_id FROM api_token_page_grants WHERE token_id = $1`, rebind.ID).Scan(&bound); err != nil ||
		bound == nil || *bound != first {
		t.Fatalf("lazy bind persisted: %v %v", bound, err)
	}
	softDelete(t, d, first)
	if !reflect.DeepEqual(allowed(rebind.Token), []string{"grantrebind"}) {
		t.Fatal("deleted page with no live holder stays allowed (handler says page_not_found)")
	}
	insertPage(t, d, "grantrebind")
	if got := allowed(rebind.Token); len(got) != 0 {
		t.Fatalf("delete→recreate must NOT re-arm the stale grant: %v", got)
	}

	keepPage := insertPage(t, d, "grantkeep")
	keep, err := Mint(ctx, d, MintParams{Label: "keep-watch", Scope: ScopeDataUpdate, AllowedSlugs: []string{"grantkeep"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(ctx, `SELECT page_id FROM api_token_page_grants WHERE token_id = $1`, keep.ID).Scan(&bound); err != nil ||
		bound == nil || *bound != keepPage {
		t.Fatal("mint binds to the live holder")
	}
	softDelete(t, d, keepPage)
	if _, err := d.Exec(ctx, `UPDATE pages SET deleted_at = NULL WHERE id = $1`, keepPage); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(allowed(keep.Token), []string{"grantkeep"}) {
		t.Fatal("delete→restore keeps the same page row, so the bound grant keeps working")
	}
}

func TestMiddleware(t *testing.T) {
	t.Setenv("API_TOKEN_PEPPER", "integration-pepper")
	d := testdb.New(t)
	ctx := context.Background()
	deploy, err := Mint(ctx, d, MintParams{Label: "northwind-deploy"})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := Mint(ctx, d, MintParams{Label: "northwind-data", Scope: ScopeDataUpdate, AllowedSlugs: []string{"mcpdata"}})
	if err != nil {
		t.Fatal(err)
	}
	onError := func(w http.ResponseWriter, _ *http.Request, err error) {
		e, ok := apierror.As(err)
		if !ok {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(e.Status)
		_, _ = w.Write([]byte(e.Code))
	}
	var seen *Agent
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = AgentFrom(r.Context())
		_, _ = w.Write([]byte("ok"))
	})
	bearerOnly := RequireBearer(d, onError)(final)
	rest := RequireBearer(d, onError)(RequireRestAccess(onError)(final))
	do := func(h http.Handler, auth string) (int, string) {
		req := httptest.NewRequest("GET", "/api/v1/pages", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, tc := range []struct {
		h    http.Handler
		auth string
		code int
		body string
	}{
		{bearerOnly, "", 401, "no_token"},
		{bearerOnly, "Basic abc", 401, "no_token"},
		{bearerOnly, "Bearer", 401, "no_token"},
		{bearerOnly, "Bearer pgs_nope", 401, "bad_token"},
		{bearerOnly, "bearer   " + deploy.Token, 200, "ok"},
		{bearerOnly, "Bearer " + scoped.Token, 200, "ok"},
		{rest, "Bearer " + deploy.Token, 200, "ok"},
		{rest, "Bearer " + scoped.Token, 403, "token_scope_denied"},
	} {
		seen = nil
		if code, body := do(tc.h, tc.auth); code != tc.code || body != tc.body {
			t.Errorf("%q: %d %s, want %d %s", tc.auth, code, body, tc.code, tc.body)
		}
	}
	do(bearerOnly, "Bearer "+scoped.Token)
	if seen == nil || seen.Actor != "northwind-data" || seen.ActorType != "agent" || seen.Scope != ScopeDataUpdate ||
		seen.TokenID != scoped.ID || !reflect.DeepEqual(seen.AllowedSlugs, []string{"mcpdata"}) {
		t.Fatalf("agent on context %+v", seen)
	}
	if code, body := do(RequireRestAccess(onError)(final), ""); code != 403 || body != "token_scope_denied" {
		t.Fatal("no agent → denied")
	}
}

// The dashboard verifies a bearer token before choosing body limits and
// passes the result down (WithVerified); RequireBearer must reuse it rather
// than look the token up — and touch last_used_at — a second time, and must
// reuse it only for that exact token.
func TestRequireBearerReusesAnEarlierVerification(t *testing.T) {
	var got *Agent
	var failed error
	h := RequireBearer(nil, func(_ http.ResponseWriter, _ *http.Request, err error) { failed = err })(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, _ = AgentFrom(r.Context()) }))
	tok := &Token{ID: 7, Label: "northwind-agent", Scope: ScopeDeploy}

	// A nil database: any lookup would panic, so passing proves reuse.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/pages", nil)
	r.Header.Set("Authorization", "Bearer pgs_verified")
	h.ServeHTTP(httptest.NewRecorder(), r.WithContext(WithVerified(r.Context(), "pgs_verified", tok, nil)))
	if failed != nil || got == nil || got.Actor != "northwind-agent" {
		t.Fatalf("verified token not reused: agent=%+v err=%v", got, failed)
	}

	// A different token in the header is verified on its own (here: refused
	// without a lookup, as it lacks the pgs_ prefix).
	got, failed = nil, nil
	r = httptest.NewRequest(http.MethodPost, "/api/v1/pages", nil)
	r.Header.Set("Authorization", "Bearer someone-else")
	h.ServeHTTP(httptest.NewRecorder(), r.WithContext(WithVerified(r.Context(), "pgs_verified", tok, nil)))
	if got != nil {
		t.Fatal("an earlier verification authorised a different token")
	}
	if e, ok := apierror.As(failed); !ok || e.Code != "bad_token" {
		t.Fatalf("want bad_token, got %v", failed)
	}

	// A refusal is reused too: an unknown or revoked pgs_ token is not
	// looked up again (the nil database would panic), nor is a lookup error.
	for _, c := range []struct {
		err  error
		code string
	}{{nil, "bad_token"}, {apierror.Unauthorized("lookup failed", "northwind_lookup", nil), "northwind_lookup"}} {
		got, failed = nil, nil
		r = httptest.NewRequest(http.MethodPost, "/api/v1/pages", nil)
		r.Header.Set("Authorization", "Bearer pgs_revoked")
		h.ServeHTTP(httptest.NewRecorder(), r.WithContext(WithVerified(r.Context(), "pgs_revoked", nil, c.err)))
		if e, ok := apierror.As(failed); got != nil || !ok || e.Code != c.code {
			t.Fatalf("cached refusal: agent=%v err=%v, want %s", got, failed, c.code)
		}
	}
}
