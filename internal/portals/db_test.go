// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package portals

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/versions"
)

var (
	admin = versions.Actor{Actor: "admin@example.test", ActorType: "user", IP: "127.0.0.1"}
	agent = versions.Actor{Actor: "test-agent", ActorType: "agent", IP: "127.0.0.1"}
)

func setup(t *testing.T) (*db.DB, context.Context) {
	t.Helper()
	t.Setenv("PAGE_COOKIE_SECRET", "portals-test-secret-0123456789abcdef")
	t.Setenv("PG_LOCK_TIMEOUT_MS", "1000")
	return testdb.New(t), context.Background()
}

func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return v
	}
}

func doc(title, body string) string {
	return `<!doctype html><html><head><title>` + title + `</title></head><body>` + body + `</body></html>`
}

func publishPage(t *testing.T, d *db.DB, slug, title, html string) {
	t.Helper()
	ctx := context.Background()
	if title != "" {
		must(versions.CreatePage(ctx, d, versions.CreatePageInput{Slug: slug, Title: title}, admin))(t)
	}
	must(versions.Deploy(ctx, d, versions.DeployInput{Slug: slug, HTML: html, Publish: true}, admin))(t)
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func member(t *testing.T, d *Detail, slug string) Member {
	t.Helper()
	for _, m := range d.Members {
		if m.Slug == slug {
			return m
		}
	}
	t.Fatalf("no member %s", slug)
	return Member{}
}

func portalSlugsFor(t *testing.T, d *db.DB, slug string) []string {
	t.Helper()
	var id jsx.ID
	if err := d.QueryRow(context.Background(), `SELECT id FROM pages WHERE slug = $1 AND deleted_at IS NULL`, slug).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range must(ListForPage(context.Background(), d, id))(t) {
		out = append(out, p.Slug)
	}
	sort.Strings(out)
	return out
}

// The partner-portal section of test/admin.integration.js, at the package
// layer (the HTTP-level 401/403/CSRF assertions belong to the admin API).
func TestPortalLifecycle(t *testing.T) {
	d, ctx := setup(t)
	publishPage(t, d, "pt-overview", "NWM Client Overview", doc("Overview", "pt-1"))
	publishPage(t, d, "pt-campaign", "NWM Contoso Fabrikam", doc("Campaign", "pt-2"))
	must(versions.SetPassword(ctx, d, "pt-campaign", "existing-client-password", admin))(t)
	must(versions.CreatePage(ctx, d, versions.CreatePageInput{Slug: "adm", Title: "Not a member"}, admin))(t)

	// The same guard at the real seam, with a real agent actor and a live DB.
	for _, call := range mutationCalls {
		wantCode(t, call(ctx, d, agent), 403, "portal_admin_only")
	}

	_, err := Create(ctx, d, CreateInput{Slug: "nwm/nested", Name: "Nested"}, admin)
	wantCode(t, err, 400, "bad_portal_slug")
	_, err = Create(ctx, d, CreateInput{Slug: "nwm", Name: "  "}, admin)
	wantCode(t, err, 400, "portal_name_required")
	_, err = Create(ctx, d, CreateInput{Slug: "nwm", Name: "NWM", Password: "short-one"}, admin)
	wantCode(t, err, 400, "portal_password_too_weak")

	created := must(Create(ctx, d, CreateInput{Slug: "nwm", Name: "Northwind Media Group"}, admin))(t)
	nwm := created.Portal
	if !created.PasswordGenerated || !generatedRE.MatchString(created.Password) {
		t.Fatalf("Pages generates a transcribable credential by default: %+v", created)
	}
	body := marshal(t, created)
	if strings.Contains(body, "scrypt$") {
		t.Fatal("no portal response ever carries the stored hash")
	}
	wantKeys := `{"portal":{"id":"` + nwm.ID.String() + `","slug":"nwm","name":"Northwind Media Group","home_page_id":null,"created_at":`
	if !strings.HasPrefix(body, wantKeys) || !strings.Contains(body, `,"home_page_slug":null,"page_count":0},"password":"`) {
		t.Fatalf("create result shape: %s", body)
	}
	nwmPassword := created.Password

	// Both live-unique indexes land on 23505; the codes must say WHICH collided.
	_, err = Create(ctx, d, CreateInput{Slug: "nwm", Name: "Other"}, admin)
	wantCode(t, err, 409, "portal_exists")
	_, err = Create(ctx, d, CreateInput{Slug: "nwm-2", Name: "northwind media group"}, admin)
	wantCode(t, err, 409, "portal_name_exists")

	addOverview := must(AddPage(ctx, d, AddPageInput{ID: nwm.ID.String(), Slug: "pt-overview", Label: "Portfolio overview", SortOrder: 0.0}, admin))(t)
	if !addOverview.ReclassifiesStaffOnly {
		t.Fatal("a staff-only page joining a portal becomes client-readable — reported at the moment of adding")
	}
	// Labelled so that curated order and alphabetical order DISAGREE.
	addCampaign := must(AddPage(ctx, d, AddPageInput{ID: nwm.ID.String(), Slug: "pt-campaign", Label: "Contoso — Fabrikam", SortOrder: 1.0}, admin))(t)
	if addCampaign.ReclassifiesStaffOnly || addCampaign.Member.DisplayTitle != "Contoso — Fabrikam" {
		t.Fatalf("%+v", addCampaign)
	}
	if got := marshal(t, addCampaign); !strings.HasPrefix(got, `{"member":{"page_id":"`) ||
		!strings.Contains(got, `"label":"Contoso — Fabrikam","sort_order":1,"added_at":"`) ||
		!strings.Contains(got, `"slug":"pt-campaign","title":"NWM Contoso Fabrikam","display_title":"Contoso — Fabrikam"},"portal_id":"`+nwm.ID.String()+`","reclassifies_staff_only":false}`) {
		t.Fatalf("add result shape: %s", got)
	}
	_, err = AddPage(ctx, d, AddPageInput{ID: nwm.ID.String(), Slug: "pt-campaign"}, admin)
	wantCode(t, err, 409, "portal_page_exists")
	_, err = AddPage(ctx, d, AddPageInput{ID: nwm.ID.String(), Slug: "pt-nonexistent"}, admin)
	wantCode(t, err, 404, "page_not_found")
	_, err = AddPage(ctx, d, AddPageInput{ID: "999999", Slug: "pt-campaign"}, admin)
	wantCode(t, err, 404, "portal_not_found")

	list := must(List(ctx, d))(t)
	if len(list) != 1 || list[0].PageCount != 2 {
		t.Fatalf("one live portal with its membership count: %+v", list)
	}
	detail := must(Get(ctx, d, nwm.ID.String()))(t)
	var slugs []string
	for _, m := range detail.Members {
		slugs = append(slugs, m.Slug)
	}
	if !slices.Equal(slugs, []string{"pt-overview", "pt-campaign"}) {
		t.Fatalf("curated order wins: %v", slugs)
	}
	if detail.Members[0].DisplayTitle != "Portfolio overview" || detail.Members[0].HasPassword || !detail.Members[1].HasPassword {
		t.Fatalf("%+v", detail.Members)
	}
	for _, m := range detail.Members {
		if !m.ShowsSwitcher || m.SwitcherIsOwn == nil || *m.SwitcherIsOwn {
			t.Fatalf("a themed dashboard shows a Page menu it does not own yet: %+v", m)
		}
	}
	if got := marshal(t, detail.LinkAudit); got != `{"scanned":false,"missing":[]}` {
		t.Fatalf("no home page ⇒ not scanned: %s", got)
	}

	publishPage(t, d, "pt-overview", "", `<!doctype html><html><head><title>Overview</title></head><body><h1>Overview</h1><script>
          const nav = document.getElementById("pages-nav");
          if (nav) draw(JSON.parse(nav.textContent));
        </script></body></html>`)
	aware := must(Get(ctx, d, nwm.ID))(t)
	if own := member(t, aware, "pt-overview").SwitcherIsOwn; own == nil || !*own {
		t.Fatal("a design that reads the block owns its menu")
	}
	if own := member(t, aware, "pt-campaign").SwitcherIsOwn; own == nil || *own {
		t.Fatal("…and one that does not gets the built-in control")
	}
	// Authoring boilerplate that MENTIONS #pages-nav in a CSS comment is not
	// ownership.
	publishPage(t, d, "pt-campaign", "", `<!doctype html><html><head><title>Contoso</title><style>/* Page switcher (Pages injects #pages-nav on a portal-authorised render; absent otherwise, and this whole control is then never built). */</style></head><body><h1>v3</h1></body></html>`)
	mention := member(t, must(Get(ctx, d, nwm.ID))(t), "pt-campaign")
	if mention.SwitcherIsOwn == nil || *mention.SwitcherIsOwn || !mention.ShowsSwitcher {
		t.Fatalf("a comment mention is not ownership: %+v", mention)
	}

	_, err = SetHome(ctx, d, SetHomeInput{ID: nwm.ID, Slug: "adm"}, admin)
	wantCode(t, err, 400, "portal_page_not_found")
	home := must(SetHome(ctx, d, SetHomeInput{ID: nwm.ID, Slug: "pt-overview"}, admin))(t)
	if home.Portal.HomePageSlug == nil || *home.Portal.HomePageSlug != "pt-overview" {
		t.Fatalf("%+v", home)
	}
	if got := marshal(t, home); !strings.Contains(got, `"updated_at":"`) || !strings.HasSuffix(got, `,"home_page_slug":"pt-overview"}}`) {
		t.Fatalf("set home shape: %s", got)
	}
	again := must(SetHome(ctx, d, SetHomeInput{ID: nwm.ID, Slug: "pt-overview"}, admin))(t)
	if *again.Portal.HomePageSlug != "pt-overview" {
		t.Fatal("an unchanged home is a no-op that still names it")
	}

	// Link audit: live pages the home page links to that are not members.
	publishPage(t, d, "pt-woodgrove", "Woodgrove campaign", doc("Woodgrove", "woodgrove"))
	publishPage(t, d, "pt-overview", "", doc("Overview",
		`<a href="/pt-woodgrove">Woodgrove</a>`+
			`<a href="/pt-campaign">Contoso</a>`+
			`<a href="/pt-overview">self</a>`+
			`<a href="https://elsewhere.example.com/pt-woodgrove">external, ignored</a>`))
	audited := must(Get(ctx, d, nwm.ID))(t).LinkAudit
	if !audited.Scanned || len(audited.Missing) != 1 || audited.Missing[0].Slug != "pt-woodgrove" || audited.Missing[0].Title != "Woodgrove campaign" {
		t.Fatalf("exactly the linked live non-member: %+v", audited)
	}
	must(AddPage(ctx, d, AddPageInput{ID: nwm.ID, Slug: "pt-woodgrove"}, admin))(t)
	if got := marshal(t, must(Get(ctx, d, nwm.ID))(t).LinkAudit); got != `{"scanned":true,"missing":[]}` {
		t.Fatalf("adding the member clears the audit: %s", got)
	}
	must(RemovePage(ctx, d, RemovePageInput{ID: nwm.ID, Slug: "pt-woodgrove"}, admin))(t)

	relabelled := must(UpdatePage(ctx, d, UpdatePageInput{ID: nwm.ID, Slug: "pt-campaign", Label: "Contoso — Fabrikam (Q3)"}, admin))(t)
	if relabelled.Member.DisplayTitle != "Contoso — Fabrikam (Q3)" || relabelled.Member.SortOrder != 1 {
		t.Fatalf("an absent field is left alone, not reset: %+v", relabelled)
	}
	_, err = UpdatePage(ctx, d, UpdatePageInput{ID: nwm.ID, Slug: "pt-campaign"}, admin)
	wantCode(t, err, 400, "portal_member_nothing_to_update")
	cleared := must(UpdatePage(ctx, d, UpdatePageInput{ID: nwm.ID, Slug: "pt-campaign", Label: versions.Null}, admin))(t)
	if cleared.Member.Label != nil || cleared.Member.DisplayTitle != "NWM Contoso Fabrikam" {
		t.Fatalf("clearing a label falls back to the page title: %+v", cleared)
	}
	reordered := must(UpdatePage(ctx, d, UpdatePageInput{ID: nwm.ID, Slug: "pt-campaign", SortOrder: "5"}, admin))(t)
	if reordered.Member.SortOrder != 5 || reordered.Member.Label != nil {
		t.Fatalf("%+v", reordered)
	}
	_, err = UpdatePage(ctx, d, UpdatePageInput{ID: nwm.ID, Slug: "adm", Label: "x"}, admin)
	wantCode(t, err, 404, "portal_page_not_found")

	// Fan-out cap.
	for i := 2; i <= MaxPortalsPerPage; i++ {
		n := string(rune('0' + i))
		extra := must(Create(ctx, d, CreateInput{Slug: "fanout-" + n, Name: "Fan-out " + n}, admin))(t)
		must(AddPage(ctx, d, AddPageInput{ID: extra.Portal.ID, Slug: "pt-campaign"}, admin))(t)
	}
	over := must(Create(ctx, d, CreateInput{Slug: "fanout-over", Name: "Fan-out over"}, admin))(t)
	_, err = AddPage(ctx, d, AddPageInput{ID: over.Portal.ID, Slug: "pt-campaign"}, admin)
	wantCode(t, err, 409, "portal_fanout_exceeded")
	if got := portalSlugsFor(t, d, "pt-campaign"); !slices.Equal(got, []string{"fanout-2", "fanout-3", "fanout-4", "nwm"}) {
		t.Fatalf("who can see this page: %v", got)
	}

	removedHome := must(RemovePage(ctx, d, RemovePageInput{ID: nwm.ID, Slug: "pt-overview"}, admin))(t)
	if !removedHome.Removed || !removedHome.HomeCleared {
		t.Fatalf("%+v", removedHome)
	}
	if must(Get(ctx, d, nwm.ID))(t).Portal.HomePageID != 0 {
		t.Fatal("the stored home pointer is cleared, not just reported as cleared")
	}
	_, err = RemovePage(ctx, d, RemovePageInput{ID: nwm.ID, Slug: "pt-overview"}, admin)
	wantCode(t, err, 404, "portal_page_not_found")

	// A soft-deleted member stays visible, and removable by page id.
	must(versions.CreatePage(ctx, d, versions.CreatePageInput{Slug: "pt-gone", Title: "Retired dashboard"}, admin))(t)
	addGone := must(AddPage(ctx, d, AddPageInput{ID: nwm.ID, Slug: "pt-gone"}, admin))(t)
	if addGone.Member.DisplayTitle != "Retired dashboard" {
		t.Fatal("no label ⇒ fall back to the page title")
	}
	must(versions.DeletePage(ctx, d, "pt-gone", admin))(t)
	orphanDetail := must(Get(ctx, d, nwm.ID))(t)
	orphan := member(t, orphanDetail, "pt-gone")
	if !orphan.PageDeleted || orphanDetail.Portal.PageCount != 1 {
		t.Fatalf("a deleted member stays visible but does not count: %+v", orphanDetail)
	}
	_, err = RemovePage(ctx, d, RemovePageInput{ID: nwm.ID, Slug: "pt-gone"}, admin)
	wantCode(t, err, 404, "page_not_found")
	_, err = RemovePage(ctx, d, RemovePageInput{ID: nwm.ID, PageID: "999999"}, admin)
	wantCode(t, err, 404, "page_not_found")
	if r := must(RemovePage(ctx, d, RemovePageInput{ID: nwm.ID, PageID: orphan.PageID.String()}, admin))(t); !r.Removed || r.PageSlug != "pt-gone" {
		t.Fatalf("%+v", r)
	}

	if r := must(Rename(ctx, d, RenameInput{ID: nwm.ID, Name: "NWM"}, admin))(t); r.Portal.Name != "NWM" {
		t.Fatal(r)
	}
	rotated := must(SetPassword(ctx, d, SetPasswordInput{ID: nwm.ID}, admin))(t)
	if rotated.Password == nwmPassword || !rotated.PasswordGenerated {
		t.Fatal("rotation mints a new credential")
	}
	retired := must(Remove(ctx, d, nwm.ID, admin))(t)
	if !retired.Deleted || retired.MemberCount != 1 {
		t.Fatalf("%+v", retired)
	}
	_, err = Get(ctx, d, nwm.ID)
	wantCode(t, err, 404, "portal_not_found")
	var members int
	if err := d.QueryRow(ctx, `SELECT (SELECT count(*)::int FROM page_portal_members m WHERE m.portal_id = page_portals.id)
	   FROM page_portals WHERE id = $1 AND deleted_at IS NOT NULL`, nwm.ID).Scan(&members); err != nil || members != 1 {
		t.Fatalf("a soft delete keeps the row and its membership: %v %d", err, members)
	}
	_, err = AddPage(ctx, d, AddPageInput{ID: nwm.ID, Slug: "pt-overview"}, admin)
	wantCode(t, err, 404, "portal_not_found")
	must(Create(ctx, d, CreateInput{Slug: "nwm", Name: "Northwind Media Group"}, admin))(t)
	if got := portalSlugsFor(t, d, "pt-campaign"); !slices.Equal(got, []string{"fanout-2", "fanout-3", "fanout-4"}) {
		t.Fatalf("a retired portal exposes nothing, and reusing its slug inherits no membership: %v", got)
	}

	// The non-empty CHECK is load-bearing (credential digest collision).
	if _, err := d.Exec(ctx, `INSERT INTO page_portals (slug, name, password_hash) VALUES ('empty', 'Empty', '')`); err == nil ||
		!strings.Contains(err.Error(), "violates check constraint") {
		t.Fatalf("a portal can never hold an empty credential: %v", err)
	}

	rows := must(d.Query(ctx, `SELECT DISTINCT action FROM audit_log WHERE actor = $1 AND action LIKE '%portal%' ORDER BY action`, admin.Actor))(t)
	var actions []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, a)
	}
	rows.Close()
	if !slices.Equal(actions, []string{"add_portal_page", "create_portal", "delete_portal", "remove_portal_page",
		"rename_portal", "set_portal_home", "set_portal_password", "update_portal_page"}) {
		t.Fatalf("every portal mutation is audited under its own action name: %v", actions)
	}
	var reclassified bool
	var portalIDType string
	if err := d.QueryRow(ctx, `SELECT (metadata->>'reclassifies_staff_only')::boolean, jsonb_typeof(metadata->'portal_id') FROM audit_log
	   WHERE action = 'add_portal_page' AND metadata->>'page_slug' = 'pt-overview' LIMIT 1`).Scan(&reclassified, &portalIDType); err != nil || !reclassified {
		t.Fatalf("the reclassification is stamped into the audit metadata: %v", err)
	}
	if portalIDType != "string" {
		t.Fatalf("portal_id in audit metadata is a string, as node-postgres wrote it: %s", portalIDType)
	}
	var leaked int
	if err := d.QueryRow(ctx, `SELECT count(*)::int FROM audit_log WHERE metadata::text LIKE $1`, "%"+nwmPassword+"%").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatal("a portal credential never reaches the audit log")
	}
}

// The portal section of test/password-reveal.integration.js.
func TestRevealPortalPassword(t *testing.T) {
	d, ctx := setup(t)
	auditCount := func() int {
		var n int
		if err := d.QueryRow(ctx, `SELECT count(*)::int FROM audit_log WHERE action = 'reveal_portal_password'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	created := must(Create(ctx, d, CreateInput{Slug: "rv-northwind", Name: "Northwind Reveal"}, admin))(t)
	id := created.Portal.ID
	shown := must(RevealPassword(ctx, d, id.String(), admin))(t)
	if got, want := marshal(t, shown), `{"id":"`+id.String()+`","available":true,"password":"`+created.Password+`"}`; got != want {
		t.Fatalf("%s != %s", got, want)
	}
	if auditCount() != 1 {
		t.Fatal("portal reveals are audited")
	}
	rotated := must(SetPassword(ctx, d, SetPasswordInput{ID: id, Password: "a-chosen-portal-passphrase"}, admin))(t)
	if rotated.PasswordGenerated || rotated.Password != "a-chosen-portal-passphrase" {
		t.Fatalf("%+v", rotated)
	}
	if p := must(RevealPassword(ctx, d, id, admin))(t).Password; p == nil || *p != rotated.Password {
		t.Fatal("a reveal follows rotation")
	}
	if _, err := d.Exec(ctx, `UPDATE page_portals SET password_ciphertext = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if got := marshal(t, must(RevealPassword(ctx, d, id, admin))(t)); got != `{"id":"`+id.String()+`","available":false,"password":null}` {
		t.Fatal(got)
	}
	_, err := RevealPassword(ctx, d, id, agent)
	wantCode(t, err, 403, "portal_admin_only")
	_, err = RevealPassword(ctx, d, "999999", admin)
	wantCode(t, err, 404, "portal_not_found")
	if auditCount() != 3 {
		t.Fatal("every successful reveal is audited, and only those")
	}
	detail := marshal(t, must(Get(ctx, d, id))(t))
	if strings.Contains(detail, rotated.Password) || strings.Contains(detail, "password_ciphertext") || strings.Contains(detail, "scrypt$") {
		t.Fatal("portal reads carry no password")
	}
}
