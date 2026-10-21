// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package adminapi

// Port of test/admin.integration.js: the admin shell's JSON API
// (/api/v1/admin/*), the cookie+CSRF human surface. A REAL Ed25519
// elcano_auth cookie (test keypair) is minted so the SSO admin gate + CSRF
// are genuinely exercised end to end.
//
// Not covered here (server wiring, owned by internal/server): the /admin,
// /admin/:slug, /admin/templates and /admin/portals HTML shells and the
// /shell-assets/* files. The page-switcher check feeds the real API response
// to public/shell-assets/page-switcher.js under node (skipped without node).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/portals"
	"github.com/ElcanoTek/pages/internal/versions"
	"github.com/ElcanoTek/pages/internal/workspaces"
)

func expectAPIErr(t *testing.T, err error, status int, code, what string) {
	t.Helper()
	e, ok := apierror.As(err)
	if !ok || e.Status != status || e.Code != code {
		t.Fatalf("%s: got %v, want %d %s", what, err, status, code)
	}
}

func TestAdminIntegration(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	contentOrigin := env.ContentOrigin()

	// ── open page with a live v1 and a draft v2 ──
	e.createPage(versions.CreatePageInput{Slug: "adm", Title: "Admin Demo"})
	v1 := e.deploy("adm", html(1), true).ID.String()
	v2 := e.deploy("adm", html(2), false).ID.String()

	// 1. auth gate.
	expectCode(t, e.req("GET", "/api/v1/admin/pages/adm", opts{}), 401, "no_session", "no cookie")
	expectCode(t, e.req("GET", "/api/v1/admin/pages/adm", opts{cookie: e.outsideCk}), 403, "not_admin", "non-admin")
	// 1b. malformed cookie values must get the normal unauthenticated
	// behavior, never a 500 — for ANY cookie name, and the auth cookie.
	expect(t, e.req("GET", "/api/v1/admin/pages/adm", opts{cookie: "x=%"}), 401, "junk cookie")
	expect(t, e.req("GET", "/api/v1/admin/pages/adm", opts{cookie: "elcano_auth=%"}), 401, "malformed auth cookie")

	// 2. read: page + versions + pending + themes.
	g := e.get("/api/v1/admin/pages/adm")
	expect(t, g, 200, "detail")
	eq(t, g.at("page", "published_version_id"), v1, "v1 is live")
	eq(t, len(list(g.at("versions"))), 2, "two versions")
	if find(g.at("themes"), "name", "flag") == nil {
		t.Fatal("flag theme listed")
	}
	if _, ok := g.json.(map[string]any)["automatic_refresh"]; ok {
		t.Fatal("retired Pages-owned schedules are not exposed")
	}
	if g.at("csrfOk") != true || g.at("deck") != false {
		t.Fatalf("detail flags: %s", g.body)
	}

	// 3. CSRF gates on a mutation.
	expectCode(t, e.req("POST", "/api/v1/admin/pages/adm/publish", opts{cookie: e.adminCk, origin: origin, body: map[string]any{"version_id": v2}}), 403, "bad_csrf", "no CSRF token")
	expectCode(t, e.req("POST", "/api/v1/admin/pages/adm/publish", opts{cookie: e.adminCk, csrf: true, body: map[string]any{"version_id": v2}}), 403, "bad_origin", "no Origin")

	// 3b. The retired refresh route has no admin handler: it falls through to
	// the bearer API and cannot dispatch work.
	expect(t, e.post("/api/v1/admin/pages/adm/refresh/run", nil), 401, "retired admin refresh route")

	// 3c. Workspace CRUD + assignment.
	idx := e.get("/api/v1/admin/pages")
	if ws := list(idx.at("workspaces")); ws == nil || len(ws) != 0 {
		t.Fatalf("new installation starts with no workspaces: %s", idx.body)
	}
	admRow := find(idx.at("pages"), "slug", "adm")
	eq(t, admRow["workspace_id"], nil, "existing page is Ungrouped")
	eq(t, admRow["version_count"], 2, "admin index gets this page's version count")
	eq(t, admRow["published_version_number"], 1, "admin index labels the live pointer with a page-local ordinal")
	expect(t, e.req("GET", "/api/v1/admin/workspaces", opts{}), 401, "workspace list is admin-only")
	expect(t, e.req("POST", "/api/v1/admin/workspaces", opts{cookie: e.adminCk, origin: origin, body: map[string]any{"name": "No CSRF"}}), 403, "workspace creation requires CSRF")
	expect(t, e.post("/api/v1/admin/workspaces", map[string]any{"name": "  "}), 400, "blank name")
	expect(t, e.post("/api/v1/admin/workspaces", map[string]any{"name": strings.Repeat("x", 101)}), 400, "long name")
	alphaRes := e.post("/api/v1/admin/workspaces", map[string]any{"name": "Alpha"})
	expect(t, alphaRes, 201, "create Alpha")
	alpha := alphaRes.str("workspace", "id")
	expect(t, e.post("/api/v1/admin/workspaces", map[string]any{"name": "aLPHa"}), 409, "case-only duplicate")
	beta := e.post("/api/v1/admin/workspaces", map[string]any{"name": "Beta"}).str("workspace", "id")
	expectCode(t, e.post("/api/v1/admin/pages/adm/workspace", map[string]any{}), 400, "workspace_id_required", "assignment requires an explicit destination")
	expect(t, e.post("/api/v1/admin/pages/adm/workspace", map[string]any{"workspace_id": 999999}), 404, "unknown workspace")
	assigned := e.post("/api/v1/admin/pages/adm/workspace", map[string]any{"workspace_id": alpha})
	expect(t, assigned, 200, "assign")
	eq(t, assigned.str("page", "workspace_name"), "Alpha", "assigned name")
	detail := e.get("/api/v1/admin/pages/adm")
	eq(t, detail.str("page", "workspace_id"), alpha, "membership persisted")
	eq(t, detail.str("page", "workspace_name"), "Alpha", "membership name")
	grouped := e.get("/api/v1/admin/pages")
	eq(t, find(grouped.at("pages"), "slug", "adm")["workspace_name"], "Alpha", "index membership")
	eq(t, find(grouped.at("workspaces"), "id", alpha)["page_count"], 1, "workspace count")
	renamed := e.post("/api/v1/admin/workspaces/"+alpha+"/rename", map[string]any{"name": "Client Alpha"})
	eq(t, renamed.str("workspace", "name"), "Client Alpha", "rename")
	eq(t, e.get("/api/v1/admin/pages/adm").str("page", "workspace_name"), "Client Alpha", "renamed joins through page reads")
	groupedPage := e.post("/api/v1/admin/pages", map[string]any{"slug": "admgrouped", "title": "Grouped from creation", "workspace_id": beta})
	expect(t, groupedPage, 201, "create in workspace")
	eq(t, groupedPage.str("page", "workspace_id"), beta, "created directly in a workspace")
	agentGrouped := e.createPage(versions.CreatePageInput{Slug: "agent-grouped", Title: "Agent grouped", WorkspaceID: alpha})
	eq(t, agentGrouped.WorkspaceID.String(), alpha, "agents may choose workspace metadata")
	eq(t, *agentGrouped.WorkspaceName, "Client Alpha", "agent-grouped name")
	_, err := workspaces.Remove(ctx, e.d, alpha, seed)
	expectAPIErr(t, err, 403, "admin_only", "bulk workspace removal remains human-admin-only")

	e.post("/api/v1/admin/pages/adm/workspace", map[string]any{"workspace_id": beta})
	grouped = e.get("/api/v1/admin/pages")
	eq(t, find(grouped.at("workspaces"), "id", alpha)["page_count"], 1, "moving decrements old workspace")
	eq(t, find(grouped.at("workspaces"), "id", beta)["page_count"], 2, "moving increments destination")
	for _, w := range list(grouped.at("workspaces")) {
		n := 0
		for _, p := range list(grouped.at("pages")) {
			if at(p, "workspace_id") == at(w, "id") {
				n++
			}
		}
		eq(t, at(w, "page_count"), n, "count derives from the returned snapshot")
	}
	beforeRemoval := e.get("/api/v1/admin/pages/adm")
	removed := e.post("/api/v1/admin/workspaces/"+beta+"/delete", nil)
	expect(t, removed, 200, "delete workspace")
	eq(t, removed.at("workspace", "ungrouped_page_count"), 2, "delete reports detached active pages")
	expect(t, e.post("/api/v1/admin/workspaces/"+beta+"/delete", nil), 404, "deleted workspace is gone")
	detail = e.get("/api/v1/admin/pages/adm")
	expect(t, detail, 200, "member page survives")
	eq(t, detail.at("page", "workspace_id"), nil, "member moves to Ungrouped")
	for _, k := range []string{"slug", "published_version_id", "require_approval"} {
		eq(t, detail.at("page", k), beforeRemoval.at("page", k), k+" unchanged")
	}
	eq(t, pluck(detail.at("versions"), "id"), pluck(beforeRemoval.at("versions"), "id"), "version history unchanged")
	eq(t, e.get("/api/v1/admin/pages/admgrouped").at("page", "workspace_id"), nil, "every member detached")
	e.post("/api/v1/admin/workspaces/"+alpha+"/delete", nil)
	eq(t, len(list(e.get("/api/v1/admin/workspaces").at("workspaces"))), 0, "workspace cleanup complete")
	for _, action := range []string{"create_workspace", "rename_workspace", "delete_workspace", "set_workspace"} {
		var n int
		if err := e.d.QueryRow(ctx, `SELECT count(*)::int FROM audit_log WHERE actor = $1 AND action = $2`, admin, action).Scan(&n); err != nil || n == 0 {
			t.Fatalf("%s is audit logged (%d, %v)", action, n, err)
		}
	}
	t.Log("✓ workspace CRUD + assignment + safe audited removal")

	// 3d. Partner portals.
	e.createPage(versions.CreatePageInput{Slug: "pt-overview", Title: "NWM Client Overview"})
	e.deploy("pt-overview", html("pt-1"), true)
	e.createPage(versions.CreatePageInput{Slug: "pt-campaign", Title: "NWM Contoso Fabrikam"})
	e.deploy("pt-campaign", html("pt-2"), true)
	must[*versions.PasswordResult](t)(versions.SetPassword(ctx, e.d, "pt-campaign", "existing-client-password", seed))

	expect(t, e.req("GET", "/api/v1/admin/portals", opts{}), 401, "portal reads are admin-only")
	expect(t, e.req("GET", "/api/v1/admin/portals", opts{cookie: e.outsideCk}), 403, "outsider")
	expect(t, e.req("POST", "/api/v1/admin/portals", opts{cookie: e.adminCk, origin: origin, body: map[string]any{"slug": "nocsrf", "name": "No CSRF"}}), 403, "portal creation requires CSRF")
	// The same guard at the library seam, with a real agent actor.
	for verb, call := range map[string]func() error{
		"create": func() error {
			_, err := portals.Create(ctx, e.d, portals.CreateInput{Slug: "agent-made", Name: "Agent Made"}, seed)
			return err
		},
		"addPage": func() error {
			_, err := portals.AddPage(ctx, e.d, portals.AddPageInput{ID: 1, Slug: "pt-overview"}, seed)
			return err
		},
		"setPassword": func() error {
			_, err := portals.SetPassword(ctx, e.d, portals.SetPasswordInput{ID: 1}, seed)
			return err
		},
		"remove": func() error { _, err := portals.Remove(ctx, e.d, 1, seed); return err },
	} {
		expectAPIErr(t, call(), 403, "portal_admin_only", "an agent may never "+verb+" a portal")
	}
	expect(t, e.post("/api/v1/admin/portals", map[string]any{"slug": "nwm/nested", "name": "Nested"}), 400, "portal slug is one segment")
	expect(t, e.post("/api/v1/admin/portals", map[string]any{"slug": "nwm", "name": "  "}), 400, "blank name")
	expect(t, e.post("/api/v1/admin/portals", map[string]any{"slug": "nwm", "name": "NWM", "password": "short-one"}), 400, "weak password")
	nwmCreated := e.post("/api/v1/admin/portals", map[string]any{"slug": "nwm", "name": "Northwind Media Group"})
	expect(t, nwmCreated, 201, "create portal")
	nwm := nwmCreated.str("portal", "id")
	nwmPassword := nwmCreated.str("password")
	eq(t, nwmCreated.at("password_generated"), true, "generated by default")
	eq(t, nwmCreated.str("portal", "url"), contentOrigin+"/portal/nwm", "derived partner url")
	if !regexp.MustCompile(`^[a-hjkmnp-z2-9]{4}(?:-[a-hjkmnp-z2-9]{4}){3}$`).MatchString(nwmPassword) {
		t.Fatalf("transcribable credential: %q", nwmPassword)
	}
	if strings.Contains(nwmCreated.body, "scrypt$") {
		t.Fatal("no portal response carries the stored hash")
	}
	expectCode(t, e.post("/api/v1/admin/portals", map[string]any{"slug": "nwm", "name": "Other"}), 409, "portal_exists", "duplicate slug")
	expectCode(t, e.post("/api/v1/admin/portals", map[string]any{"slug": "nwm-2", "name": "northwind media group"}), 409, "portal_name_exists", "duplicate name")

	pp := "/api/v1/admin/portals/" + nwm
	addOverview := e.post(pp+"/pages", map[string]any{"slug": "pt-overview", "label": "Portfolio overview", "sort_order": 0})
	expect(t, addOverview, 201, "add overview")
	eq(t, addOverview.at("reclassifies_staff_only"), true, "staff-only page reclassified at add time")
	addCampaign := e.post(pp+"/pages", map[string]any{"slug": "pt-campaign", "label": "Contoso — Fabrikam", "sort_order": 1})
	expect(t, addCampaign, 201, "add campaign")
	eq(t, addCampaign.at("reclassifies_staff_only"), false, "passworded page not reclassified")
	eq(t, addCampaign.str("member", "display_title"), "Contoso — Fabrikam", "curated label")
	expect(t, e.post(pp+"/pages", map[string]any{"slug": "pt-campaign"}), 409, "already a member")
	expect(t, e.post(pp+"/pages", map[string]any{"slug": "pt-nonexistent"}), 404, "unknown page")
	expect(t, e.post("/api/v1/admin/portals/999999/pages", map[string]any{"slug": "pt-campaign"}), 404, "unknown portal")
	expectCode(t, e.post("/api/v1/admin/portals/abc/pages", map[string]any{"slug": "pt-campaign"}), 400, "bad_id", "non-numeric portal id")

	portalList := e.get("/api/v1/admin/portals")
	eq(t, len(list(portalList.at("portals"))), 1, "one live portal")
	eq(t, portalList.at("portals", 0, "page_count"), 2, "membership count")
	eq(t, portalList.at("portals", 0, "url"), contentOrigin+"/portal/nwm", "list url")
	pd := e.get(pp)
	eq(t, pluck(pd.at("members"), "slug"), []string{"pt-overview", "pt-campaign"}, "curated order wins")
	eq(t, pd.at("members", 0, "display_title"), "Portfolio overview", "curated label is what a partner reads")
	eq(t, pd.at("members", 0, "has_password"), false, "overview has no password")
	eq(t, pd.at("members", 1, "has_password"), true, "campaign has a password")
	eq(t, pd.at("portal", "url"), contentOrigin+"/portal/nwm", "detail url")
	for _, m := range list(pd.at("members")) {
		eq(t, at(m, "shows_switcher"), true, "a themed dashboard gets a Page menu")
		eq(t, at(m, "switcher_is_own"), false, "neither design renders its own yet")
	}
	e.deploy("pt-overview", `<!doctype html><html><head><title>Overview</title></head><body><h1>Overview</h1><script>
          const nav = document.getElementById("pages-nav");
          if (nav) draw(JSON.parse(nav.textContent));
        </script></body></html>`, true)
	aware := e.get(pp).at("members")
	eq(t, find(aware, "slug", "pt-overview")["switcher_is_own"], true, "a design that reads the block owns its menu")
	eq(t, find(aware, "slug", "pt-campaign")["switcher_is_own"], false, "one that does not gets the built-in control")
	// The production trap: boilerplate that MENTIONS #pages-nav in a comment.
	e.deploy("pt-campaign", `<!doctype html><html><head><title>Contoso</title><style>/* Page switcher (Pages injects #pages-nav on a portal-authorised render; absent otherwise, and this whole control is then never built). */</style></head><body><h1>v3</h1></body></html>`, true)
	mention := find(e.get(pp).at("members"), "slug", "pt-campaign")
	eq(t, mention["switcher_is_own"], false, "a comment mention is not ownership")
	eq(t, mention["shows_switcher"], true, "the row still reports a menu")

	expectCode(t, e.post(pp+"/home", map[string]any{}), 400, "portal_home_slug_required", "an explicit slug is required")
	expect(t, e.post(pp+"/home", map[string]any{"slug": "adm"}), 400, "home outside the portal")
	eq(t, e.post(pp+"/home", map[string]any{"slug": "pt-overview"}).at("portal", "home_page_slug"), "pt-overview", "home set")

	// Link audit: live pages the home page links to that are not members.
	e.createPage(versions.CreatePageInput{Slug: "pt-woodgrove", Title: "Woodgrove campaign"})
	e.deploy("pt-woodgrove", "<!doctype html><html><head><title>Woodgrove</title></head><body>woodgrove</body></html>", true)
	e.deploy("pt-overview", `<!doctype html><html><head><title>Overview</title></head><body>`+
		`<a href="/pt-woodgrove">Woodgrove</a>`+
		`<a href="/pt-campaign">Contoso</a>`+
		`<a href="/pt-overview">self</a>`+
		`<a href="https://elsewhere.example.com/pt-woodgrove">external, ignored</a>`+
		`</body></html>`, true)
	audited := e.get(pp).at("link_audit")
	eq(t, at(audited, "scanned"), true, "with a published home page the audit runs")
	eq(t, pluck(at(audited, "missing"), "slug"), []string{"pt-woodgrove"}, "exactly the linked live non-member")
	expect(t, e.post(pp+"/pages", map[string]any{"slug": "pt-woodgrove"}), 201, "add woodgrove")
	eq(t, len(list(e.get(pp).at("link_audit", "missing"))), 0, "adding the member clears the audit")
	expect(t, e.post(pp+"/pages/remove", map[string]any{"slug": "pt-woodgrove"}), 200, "remove woodgrove")

	relabelled := e.post(pp+"/pages/update", map[string]any{"slug": "pt-campaign", "label": "Contoso — Fabrikam (Q3)"})
	eq(t, relabelled.str("member", "display_title"), "Contoso — Fabrikam (Q3)", "relabel")
	eq(t, relabelled.at("member", "sort_order"), 1, "an absent field is left alone")
	expect(t, e.post(pp+"/pages/update", map[string]any{"slug": "pt-campaign"}), 400, "nothing to update")
	cleared := e.post(pp+"/pages/update", map[string]any{"slug": "pt-campaign", "label": nil})
	eq(t, cleared.at("member", "label"), nil, "label cleared")
	eq(t, cleared.str("member", "display_title"), "NWM Contoso Fabrikam", "falls back to the page title")

	// Fan-out cap.
	for i := 2; i <= portals.MaxPortalsPerPage; i++ {
		extra := e.post("/api/v1/admin/portals", map[string]any{"slug": fmt.Sprintf("fanout-%d", i), "name": fmt.Sprintf("Fan-out %d", i)}).str("portal", "id")
		expect(t, e.post("/api/v1/admin/portals/"+extra+"/pages", map[string]any{"slug": "pt-campaign"}), 201, "fan-out add")
	}
	overCap := e.post("/api/v1/admin/portals", map[string]any{"slug": "fanout-over", "name": "Fan-out over"}).str("portal", "id")
	expectCode(t, e.post("/api/v1/admin/portals/"+overCap+"/pages", map[string]any{"slug": "pt-campaign"}), 409, "portal_fanout_exceeded", "portals-per-page cap")
	wantPortals := []string{"nwm"}
	for i := 2; i <= portals.MaxPortalsPerPage; i++ {
		wantPortals = append(wantPortals, fmt.Sprintf("fanout-%d", i))
	}
	slices.Sort(wantPortals)
	got := pluck(e.get("/api/v1/admin/pages/pt-campaign").at("portals"), "slug")
	slices.Sort(got)
	eq(t, got, wantPortals, "page detail answers 'who can see this page'")

	// What membership does on the content host.
	staffOnly := e.req("GET", "/pt-overview", opts{host: contentHost})
	expect(t, staffOnly, 401, "reclassified page prompts for a credential")
	if !strings.Contains(staffOnly.body, "Portal password") || strings.Contains(staffOnly.body, "Staff-only page") || strings.Contains(staffOnly.body, "Northwind Media Group") {
		t.Fatalf("portal gate body: %.300s", staffOnly.body)
	}
	pwMember := e.req("GET", "/pt-campaign", opts{host: contentHost})
	expect(t, pwMember, 401, "a member with its own password still gets a form")
	if pwMember.header.Get("Set-Cookie") != "" {
		t.Fatal("membership itself mints no session")
	}

	removedHome := e.post(pp+"/pages/remove", map[string]any{"slug": "pt-overview"})
	eq(t, removedHome.at("removed"), true, "removed")
	eq(t, removedHome.at("home_cleared"), true, "removing the home page clears it")
	eq(t, e.get(pp).at("portal", "home_page_id"), nil, "stored home pointer cleared")
	expect(t, e.post(pp+"/pages/remove", map[string]any{"slug": "pt-overview"}), 404, "not a member any more")

	// A soft-deleted member stays visible and removable by page id.
	e.createPage(versions.CreatePageInput{Slug: "pt-gone", Title: "Retired dashboard"})
	eq(t, e.post(pp+"/pages", map[string]any{"slug": "pt-gone"}).str("member", "display_title"), "Retired dashboard", "no label ⇒ page title")
	e.post("/api/v1/admin/pages/pt-gone/delete", nil)
	orphan := find(e.get(pp).at("members"), "slug", "pt-gone")
	eq(t, orphan["page_deleted"], true, "a deleted page's membership stays visible")
	expect(t, e.post(pp+"/pages/remove", map[string]any{"slug": "pt-gone"}), 404, "its slug no longer resolves")
	eq(t, e.post(pp+"/pages/remove", map[string]any{"page_id": orphan["page_id"]}).at("removed"), true, "removable by page id")

	eq(t, e.post(pp+"/rename", map[string]any{"name": "NWM"}).str("portal", "name"), "NWM", "rename portal")
	rotated := e.post(pp+"/password", nil)
	expect(t, rotated, 200, "rotate")
	if rotated.str("password") == nwmPassword || rotated.str("password") == "" {
		t.Fatal("rotation mints a new credential")
	}
	retired := e.post(pp+"/delete", nil)
	eq(t, retired.at("portal", "deleted"), true, "retired")
	eq(t, retired.at("portal", "member_count"), 1, "retiring reports what it takes out of service")
	expect(t, e.get(pp), 404, "a retired portal is gone from every read")
	var members int
	var deletedAt *string
	if err := e.d.QueryRow(ctx, `SELECT deleted_at::text,
	        (SELECT count(*)::int FROM page_portal_members m WHERE m.portal_id = page_portals.id)
	   FROM page_portals WHERE id = $1`, nwm).Scan(&deletedAt, &members); err != nil {
		t.Fatal(err)
	}
	if deletedAt == nil || members != 1 {
		t.Fatalf("retiring is a soft delete that keeps membership: %v %d", deletedAt, members)
	}
	expect(t, e.post(pp+"/pages", map[string]any{"slug": "pt-overview"}), 404, "a retired portal accepts no further mutation")
	expect(t, e.post("/api/v1/admin/portals", map[string]any{"slug": "nwm", "name": "Northwind Media Group"}), 201, "retiring frees the slug and name")
	got = pluck(e.get("/api/v1/admin/pages/pt-campaign").at("portals"), "slug")
	slices.Sort(got)
	eq(t, got, slices.DeleteFunc(slices.Clone(wantPortals), func(s string) bool { return s == "nwm" }), "a retired portal exposes nothing, and reusing its slug inherits nothing")

	// The non-empty CHECK is load-bearing.
	if _, err := e.d.Exec(ctx, `INSERT INTO page_portals (slug, name, password_hash) VALUES ('empty', 'Empty', '')`); err == nil || !strings.Contains(err.Error(), "violates check constraint") {
		t.Fatalf("a portal can never hold an empty credential: %v", err)
	}
	var actions []string
	rows, err := e.d.Query(ctx, `SELECT DISTINCT action FROM audit_log WHERE actor = $1 AND action LIKE '%portal%' ORDER BY action`, admin)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	eq(t, actions, []string{"add_portal_page", "create_portal", "delete_portal", "remove_portal_page",
		"rename_portal", "set_portal_home", "set_portal_password", "update_portal_page"}, "every portal mutation audited")
	var reclassified bool
	if err := e.d.QueryRow(ctx, `SELECT (metadata->>'reclassifies_staff_only')::boolean FROM audit_log
	    WHERE action = 'add_portal_page' AND metadata->>'page_slug' = 'pt-overview' LIMIT 1`).Scan(&reclassified); err != nil || !reclassified {
		t.Fatalf("reclassification stamped into audit metadata: %v %v", reclassified, err)
	}
	var leaked int
	_ = e.d.QueryRow(ctx, `SELECT count(*)::int FROM audit_log WHERE metadata::text LIKE $1`, "%"+nwmPassword+"%").Scan(&leaked)
	eq(t, leaked, 0, "a portal credential never reaches the audit log")
	t.Log("✓ partner portals")

	// 4. publish the draft, then rollback.
	expect(t, e.post("/api/v1/admin/pages/adm/publish", map[string]any{"version_id": v2, "expected_version": v1}), 200, "publish")
	eq(t, e.get("/api/v1/admin/pages/adm").at("page", "published_version_id"), v2, "v2 live")
	rb := e.post("/api/v1/admin/pages/adm/rollback", map[string]any{"version_id": v1, "expected_version": v2})
	expect(t, rb, 200, "rollback")
	if _, ok := rb.json.(map[string]any)["published"]; ok {
		t.Fatalf("the admin pointer moves answer {version} only: %s", rb.body)
	}
	eq(t, e.get("/api/v1/admin/pages/adm").at("page", "published_version_id"), v1, "rolled back")
	expectCode(t, e.post("/api/v1/admin/pages/adm/publish", map[string]any{}), 400, "bad_id", "publish requires a version id")

	// 5. optimistic concurrency.
	expect(t, e.post("/api/v1/admin/pages/adm/rollback", map[string]any{"version_id": v2, "expected_version": 999999}), 409, "stale expected_version")

	// 6. approval gate + approve/reject queue.
	e.createPage(versions.CreatePageInput{Slug: "admg", Title: "Gated", RequireApproval: true})
	p1 := e.deploy("admg", html(1), true)
	p2 := e.deploy("admg", html(2), false)
	eq(t, p1.Status, "pending", "agent deploy on a gated page is pending")
	eq(t, len(list(e.get("/api/v1/admin/pages/admg").at("pending"))), 2, "two pending")
	expect(t, e.post(fmt.Sprintf("/api/v1/admin/pages/admg/versions/%s/approve", p1.ID), nil), 200, "approve")
	eq(t, e.get("/api/v1/admin/pages/admg").at("page", "published_version_id"), p1.ID.String(), "approve published p1")
	expect(t, e.post(fmt.Sprintf("/api/v1/admin/pages/admg/versions/%s/reject", p2.ID), map[string]any{"note": "no"}), 200, "reject")
	eq(t, find(e.get("/api/v1/admin/pages/admg").at("versions"), "id", p2.ID.String())["status"], "rejected", "p2 rejected")

	// 6b. one version's full row, html included; declared before the greedy
	// detail read.
	one := e.get("/api/v1/admin/pages/admg/versions/" + p1.ID.String())
	expect(t, one, 200, "one version")
	eq(t, one.str("version", "id"), p1.ID.String(), "id")
	eq(t, one.str("version", "html"), html(1), "exact html")
	for _, v := range list(e.get("/api/v1/admin/pages/admg").at("versions")) {
		if _, ok := v.(map[string]any)["html"]; ok {
			t.Fatal("the detail read does not carry html")
		}
	}
	foreign := e.get("/api/v1/admin/pages/adm/versions/" + p1.ID.String())
	expect(t, foreign, 404, "another page's version")
	if !strings.Contains(foreign.str("error"), "not found") {
		t.Fatalf("foreign: %s", foreign.body)
	}
	expect(t, e.get("/api/v1/admin/pages/admg/versions/999999"), 404, "unknown version")
	expectCode(t, e.get("/api/v1/admin/pages/admg/versions/abc"), 400, "bad_id", "non-numeric id")
	expect(t, e.get("/api/v1/admin/pages/admg/versions/0"), 400, "zero id")
	expect(t, e.req("GET", "/api/v1/admin/pages/admg/versions/"+p1.ID.String(), opts{}), 401, "no cookie")
	expect(t, e.req("GET", "/api/v1/admin/pages/admg/versions/"+p1.ID.String(), opts{cookie: e.outsideCk}), 403, "non-admin")

	// 7. approval toggle + disable/enable + theme.
	eq(t, e.post("/api/v1/admin/pages/adm/approval", map[string]any{"require_approval": true}).at("require_approval"), true, "approval on")
	eq(t, e.post("/api/v1/admin/pages/adm/approval", map[string]any{"require_approval": false}).at("require_approval"), false, "approval off")
	eq(t, e.post("/api/v1/admin/pages/adm/disable", nil).at("disabled"), true, "disable")
	eq(t, e.get("/api/v1/admin/pages/adm").at("page", "disabled"), true, "page disabled")
	eq(t, e.post("/api/v1/admin/pages/adm/enable", nil).at("disabled"), false, "enable")
	eq(t, e.post("/api/v1/admin/pages/adm/theme", map[string]any{"theme": nil}).at("theme"), "flag", "falsy theme ⇒ flag")
	expectCode(t, e.post("/api/v1/admin/pages/adm/theme", map[string]any{"theme": 5}), 400, "unknown_theme", "non-string theme is String()ed")

	// 8. preview-token → a content-host /raw URL for the chosen version.
	pv := e.post("/api/v1/admin/pages/adm/preview-token", map[string]any{"version_id": v2})
	expect(t, pv, 200, "preview-token")
	if !regexp.MustCompile(`^` + regexp.QuoteMeta(contentOrigin) + `/raw/adm\?t=`).MatchString(pv.str("url")) {
		t.Fatalf("preview url: %s", pv.str("url"))
	}
	expect(t, e.req("GET", contentPath(t, pv.str("url")), opts{host: contentHost}), 200, "preview renders on the content host")
	expectCode(t, e.post("/api/v1/admin/pages/adm/preview-token", map[string]any{"version_id": p1.ID.String()}), 404, "version_not_found", "preview of another page's version")
	expectCode(t, e.post("/api/v1/admin/pages/adm/edit-token", nil), 400, "not_a_bento_deck", "edit-token is for decks only")

	// 8b. source editor: deploy-source.
	expectCode(t, e.req("POST", "/api/v1/admin/pages/adm/deploy-source", opts{cookie: e.adminCk, csrf: true, origin: origin, body: map[string]any{}}), 400, "html_required", "empty body")
	expect(t, e.req("POST", "/api/v1/admin/pages/adm/deploy-source", opts{cookie: e.adminCk, csrf: true, body: map[string]any{"html": html(3)}}), 403, "no Origin")
	edited := e.post("/api/v1/admin/pages/adm/deploy-source", map[string]any{"html": html(3), "note": "typo fix"})
	expect(t, edited, 200, "deploy-source")
	eq(t, edited.str("version", "status"), "draft", "saved as draft")
	eq(t, edited.str("version", "source"), "admin", "source tagged admin")
	eq(t, edited.at("published"), false, "not published")
	if keys := keysOf(edited.body); !slices.Equal(keys, []string{"version", "deduped", "published", "gated"}) {
		t.Fatalf("deploy-source keys: %v", keys)
	}
	eq(t, e.get("/api/v1/admin/pages/adm").at("page", "published_version_id"), v1, "live pointer unchanged by edit")
	dup := e.post("/api/v1/admin/pages/adm/deploy-source", map[string]any{"html": html(3)})
	eq(t, dup.at("deduped"), true, "identical re-save deduped")
	eq(t, dup.str("version", "id"), edited.str("version", "id"), "same version id")
	expect(t, e.post("/api/v1/admin/pages/adm/publish", map[string]any{"version_id": edited.str("version", "id"), "expected_version": v1}), 200, "edited draft publishable")
	eq(t, e.get("/api/v1/admin/pages/adm").at("page", "published_version_id"), edited.str("version", "id"), "edited version live")
	gatedEdit := e.post("/api/v1/admin/pages/admg/deploy-source", map[string]any{"html": html(9)})
	eq(t, gatedEdit.str("version", "status"), "pending", "gated page edit → pending")
	eq(t, gatedEdit.at("gated"), true, "gated flag")

	// 8c. admin create + rename + delete/restore (incl. a GATED page).
	created := e.post("/api/v1/admin/pages", map[string]any{"slug": "admnew", "title": "Admin New"})
	expect(t, created, 201, "admin create")
	eq(t, created.str("page", "slug"), "admnew", "slug")
	eq(t, e.post("/api/v1/admin/pages/admnew/title", map[string]any{"title": "Renamed"}).at("title"), "Renamed", "rename")
	eq(t, e.post("/api/v1/admin/pages/admg/delete", nil).at("deleted"), true, "admin deletes a gated page")
	expect(t, e.get("/api/v1/admin/pages/admg"), 404, "deleted page")
	eq(t, e.post("/api/v1/admin/pages/admg/restore", nil).at("restored"), true, "restore")
	expect(t, e.get("/api/v1/admin/pages/admg"), 200, "restored page visible")
	if _, err := e.d.Exec(ctx, `INSERT INTO pages (slug, title, deleted_at) VALUES ('portal/legacy', 'Predates the reservation', now())`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, e.post("/api/v1/admin/pages/portal/legacy/restore", nil), 400, "reserved_slug", "restore refuses a reserved slug")
	expect(t, e.post("/api/v1/admin/pages", map[string]any{"slug": "portals", "title": "Shadow"}), 400, "'portals' is a reserved slug")

	// 10. nested slugs: admin API and preview-token resolve end to end.
	e.createPage(versions.CreatePageInput{Slug: "adm/nest", Title: "Nested Admin"})
	nv1 := e.deploy("adm/nest", html(5), true).ID.String()
	nget := e.get("/api/v1/admin/pages/adm/nest")
	expect(t, nget, 200, "nested read")
	eq(t, nget.at("page", "published_version_id"), nv1, "nested pointer")
	eq(t, e.get("/api/v1/admin/pages/adm%2Fnest").at("page", "slug"), "adm/nest", "an encoded %2F decodes into the slug")
	nws := e.post("/api/v1/admin/workspaces", map[string]any{"name": "Nested reports"}).str("workspace", "id")
	expect(t, e.post("/api/v1/admin/pages/adm/nest/workspace", map[string]any{"workspace_id": nws}), 200, "nested workspace mutation")
	eq(t, e.get("/api/v1/admin/pages/adm/nest").at("page", "workspace_name"), "Nested reports", "nested workspace")
	expect(t, e.post("/api/v1/admin/pages/adm/nest/deploy-source", map[string]any{"html": html(6)}), 200, "nested mutation")
	nver := e.get("/api/v1/admin/pages/adm/nest/versions/" + nv1)
	expect(t, nver, 200, "nested version read not swallowed by the detail route")
	eq(t, nver.str("version", "html"), html(5), "nested html")
	expect(t, e.get("/api/v1/admin/pages/adm/versions/"+nv1), 404, "nested page's version not readable on its parent")
	npv := e.post("/api/v1/admin/pages/adm/nest/preview-token", map[string]any{"version_id": nv1})
	expect(t, npv, 200, "nested preview")
	if !strings.Contains(npv.str("url"), "/raw/adm/nest?t=") {
		t.Fatalf("nested preview url: %s", npv.str("url"))
	}
	expect(t, e.req("GET", contentPath(t, npv.str("url")), opts{host: contentHost}), 200, "nested preview renders")
	e.post("/api/v1/admin/workspaces/"+nws+"/delete", nil)

	// 11. Cross-page switcher over a 35-page list.
	for i := 1; i <= 35; i++ {
		e.createPage(versions.CreatePageInput{Slug: fmt.Sprintf("switch-%02d", i), Title: fmt.Sprintf("Client %02d", i)})
	}
	picker := e.get("/api/v1/admin/pages")
	expect(t, picker, 200, "picker list")
	checkPageSwitcher(t, picker.body)

	// ── template library ──
	templateHTML := must[[]byte](t)(os.ReadFile(filepath.Join("..", "..", "templates", "nwm-campaign-dashboard", "template.html")))
	expect(t, e.req("GET", "/api/v1/admin/templates", opts{}), 401, "templates no cookie")
	expect(t, e.req("POST", "/api/v1/admin/templates/validate", opts{cookie: e.adminCk, origin: origin, body: map[string]any{"html": "x"}}), 403, "validate requires CSRF")
	countTemplates := func() int { return len(list(e.get("/api/v1/admin/templates").at("templates"))) }
	before := countTemplates()
	bad := e.post("/api/v1/admin/templates/validate", map[string]any{"html": "<!doctype html><html><body><p>not a template</p></body></html>", "name": "Not A Name!"})
	expect(t, bad, 200, "a dry run reports")
	eq(t, bad.at("contract_ok"), false, "contract_ok")
	eq(t, bad.str("contract_error", "code"), "template_contract_invalid", "contract code")
	eq(t, bad.str("name_error", "code"), "bad_template_name", "name checked")
	eq(t, countTemplates(), before, "validate wrote nothing")
	good := e.post("/api/v1/admin/templates/validate", map[string]any{"html": string(templateHTML), "name": "admin-nwm"})
	eq(t, good.at("contract_ok"), true, "good contract")
	eq(t, good.str("name"), "admin-nwm", "name")
	eq(t, good.at("ships_empty"), true, "ships its empty state")
	eq(t, good.at("preflight", "ok"), true, "preflight ok")
	if good.at("config_schema") == nil || good.at("data_schema") == nil || good.at("reference_config") == nil {
		t.Fatal("schemas reported")
	}
	eq(t, countTemplates(), before, "still wrote nothing")
	rejected := e.post("/api/v1/admin/templates", map[string]any{"html": "<p>nope</p>", "name": "admin-broken"})
	expectCode(t, rejected, 422, "template_contract_invalid", "malformed template")
	if keys := keysOf(rejected.body); !slices.Equal(keys, []string{"error", "code"}) {
		t.Fatalf("error envelope carries only error+code: %s", rejected.body)
	}
	expectCode(t, e.post("/api/v1/admin/templates", map[string]any{"html": string(templateHTML)}), 400, "template_required", "a name is required")
	eq(t, countTemplates(), before, "nothing registered")
	registered := e.post("/api/v1/admin/templates", map[string]any{
		"html": string(templateHTML), "name": "admin-nwm", "title": "Admin Uploaded NWM", "description": "Registered through the library.",
	})
	expect(t, registered, 201, "register")
	eq(t, registered.str("template", "name"), "admin-nwm", "template name")
	eq(t, registered.str("revision", "source"), "admin", "source admin")
	eq(t, registered.at("revision", "revision"), 1, "revision 1")
	eq(t, registered.at("has_sample_data"), true, "ships example data")
	eq(t, registered.at("revision", "has_sample_data"), true, "revision has sample data")
	eq(t, registered.at("validation", "contract_ok"), true, "validation report attached")
	reRegistered := e.post("/api/v1/admin/templates", map[string]any{"html": string(templateHTML), "name": "admin-nwm"})
	expect(t, reRegistered, 200, "identical re-register is not a create")
	eq(t, reRegistered.at("deduped"), true, "deduped")

	tpl := e.get("/api/v1/admin/templates/admin-nwm")
	eq(t, tpl.at("template", "current_revision"), 1, "current revision")
	eq(t, len(list(tpl.at("revisions"))), 1, "revisions")
	eq(t, len(list(tpl.at("pages"))), 0, "pages")
	if m, _ := tpl.at("reference_config").(map[string]any); len(m) == 0 {
		t.Fatal("reference config present")
	}
	if _, ok := tpl.json.(map[string]any)["html"]; ok {
		t.Fatal("the library reads the contract, not the design bytes")
	}
	expect(t, e.get("/api/v1/admin/templates/admin-nwm?revision=1"), 200, "explicit revision")
	expectCode(t, e.get("/api/v1/admin/templates/admin-nwm?revision=abc"), 400, "bad_id", "bad revision")

	preview := e.post("/api/v1/admin/templates/admin-nwm/preview-token", map[string]any{"render_mode": "themed"})
	expect(t, preview, 200, "template preview-token")
	if keys := keysOf(preview.body); !slices.Equal(keys, []string{"template", "revision", "render_mode", "content_sha256", "has_sample_data", "url", "expires_in_seconds"}) {
		t.Fatalf("preview keys: %v", keys)
	}
	purl := preview.str("url")
	if !strings.HasPrefix(purl, contentOrigin) || !regexp.MustCompile(`/raw-template/\d+\?t=`).MatchString(purl) {
		t.Fatalf("preview must be on the content host: %s", purl)
	}
	eq(t, preview.at("revision"), 1, "preview revision")
	eq(t, preview.at("has_sample_data"), true, "token says what the preview will show")
	ppath := contentPath(t, purl)
	rendered := e.req("GET", ppath, opts{host: contentHost})
	expect(t, rendered, 200, "preview renders")
	if !strings.Contains(rendered.header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("the preview must be sandboxed")
	}
	if !strings.Contains(rendered.body, "pages-config-schema") || strings.Contains(rendered.body, "pages-data-example") {
		t.Fatal("the design is served materialized")
	}
	m := regexp.MustCompile(`(?s)<script[^>]*id="pages-data"[^>]*>(.*?)</script>`).FindStringSubmatch(rendered.body)
	if m == nil {
		t.Fatal("no #pages-data in the preview")
	}
	var envelope struct {
		Data struct {
			Rows []map[string]any `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(m[1]), &envelope); err != nil || len(envelope.Data.Rows) < 30 {
		t.Fatalf("the preview must render populated: %d rows, %v", len(envelope.Data.Rows), err)
	}
	for _, row := range envelope.Data.Rows {
		if _, ok := row["dealId"].(string); !ok {
			t.Fatal("example rows are the design's own shape")
		}
	}
	tok := strings.SplitN(ppath, "?t=", 2)[1]
	expect(t, e.req("GET", "/raw-template/999999?t="+tok, opts{host: contentHost}), 403, "a token for one revision must not render another")
	expect(t, e.req("GET", "/raw/adm?t="+tok, opts{host: contentHost}), 403, "a template token is not a page view")
	expect(t, e.req("GET", regexp.MustCompile(`t=.*`).ReplaceAllString(ppath, "t=forged"), opts{host: contentHost}), 403, "forged token")
	expectCode(t, e.post("/api/v1/admin/templates/admin-nwm/preview-token", map[string]any{"revision": nil}), 400, "bad_id", "null revision")

	// Retire: refused while pages exist unless forced; here none do.
	del := e.req("DELETE", "/api/v1/admin/templates/admin-nwm", opts{cookie: e.adminCk, csrf: true, origin: origin})
	expect(t, del, 200, "delete template")
	eq(t, del.at("deleted"), true, "deleted")
	expect(t, e.get("/api/v1/admin/templates/admin-nwm"), 404, "retired template is gone")

	// Admin source drafts index their embedded managed data without
	// acquiring a template binding.
	e.post("/api/v1/admin/pages", map[string]any{"slug": "admin-source-metadata", "title": "Northwind metadata"})
	envJSON, _ := json.Marshal(map[string]any{"contract_version": 1, "refreshed_at": "2026-08-01T01:00:00Z", "source_as_of": "2026-08-01T00:00:00Z", "data": map[string]any{"count": 7}})
	metaHTML := `<!doctype html><html><body><h1>Northwind</h1>` +
		`<script id="pages-data-schema" type="application/schema+json">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}</script>` +
		`<script id="pages-data" type="application/json">` + string(envJSON) + `</script></body></html>`
	draft := e.post("/api/v1/admin/pages/admin-source-metadata/deploy-source", map[string]any{"html": metaHTML})
	expect(t, draft, 200, "metadata draft")
	var dataSHA, tplSHA, refreshed, asOf string
	var tplVersion *int64
	if err := e.d.QueryRow(ctx, `SELECT data_sha256, data_template_sha256,
	        to_char(refreshed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
	        to_char(source_as_of AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'), template_version_id
	   FROM page_versions WHERE id = $1`, draft.str("version", "id")).Scan(&dataSHA, &tplSHA, &refreshed, &asOf, &tplVersion); err != nil {
		t.Fatal(err)
	}
	parsed := must[*pagedata.Managed](t)(pagedata.ParseManagedHTML(metaHTML))
	eq(t, dataSHA, parsed.DataSHA256, "data_sha256")
	eq(t, tplSHA, parsed.TemplateSHA256, "data_template_sha256")
	eq(t, refreshed, "2026-08-01T01:00:00.000Z", "refreshed_at")
	eq(t, asOf, "2026-08-01T00:00:00.000Z", "source_as_of")
	if tplVersion != nil {
		t.Fatal("no template binding")
	}
	eq(t, draft.at("published"), false, "not published")
}

// keysOf is a JSON object's top-level keys in wire order.
func keysOf(body string) []string {
	dec := json.NewDecoder(strings.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return keys
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return keys
		}
	}
	return keys
}
