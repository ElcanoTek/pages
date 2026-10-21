// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/tokens"
)

var (
	agent = Actor{Actor: "test-agent", ActorType: "agent", IP: "127.0.0.1"}
	admin = Actor{Actor: "admin@example.test", ActorType: "user", IP: "127.0.0.1"}
)

func setup(t *testing.T) (*db.DB, context.Context) {
	t.Helper()
	t.Setenv("PAGE_COOKIE_SECRET", "versions-test-secret-0123456789abcdef")
	t.Setenv("PG_LOCK_TIMEOUT_MS", "1000")
	return testdb.New(t), context.Background()
}

// must(f())(t) fails the test on f's error and returns its value.
func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return v
	}
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func pageHTML(n int) string {
	return fmt.Sprintf(`<!doctype html><html><head><title>Northwind</title></head><body><h1>Report %d</h1>`+
		`<canvas id=c></canvas><script>chart(%d)</script></body></html>`, n, n)
}

func counts(t *testing.T, d *db.DB) [3]int {
	t.Helper()
	var c [3]int
	err := d.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM pages), (SELECT count(*) FROM page_versions), (SELECT count(*) FROM audit_log)`).
		Scan(&c[0], &c[1], &c[2])
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func getPage(t *testing.T, d *db.DB, slug string) *PageView {
	t.Helper()
	return must(GetPage(context.Background(), d, slug))(t)
}

func deploy(t *testing.T, d *db.DB, a Actor, in DeployInput) *DeployResult {
	t.Helper()
	return must(Deploy(context.Background(), d, in, a))(t)
}

// The REST agent loop (test/api.integration.js), at the state-machine layer.
func TestAgentLoop(t *testing.T) {
	d, ctx := setup(t)

	created := must(CreatePage(ctx, d, CreatePageInput{Slug: "acme", Title: "Acme"}, agent))(t)
	if created.Slug != "acme" || created.PublishedVersionID != 0 || created.WorkspaceName != nil {
		t.Fatalf("%+v", created)
	}
	_, err := CreatePage(ctx, d, CreatePageInput{Slug: "ACME"}, agent)
	wantCode(t, err, 409, "slug_exists")

	d1 := deploy(t, d, agent, DeployInput{Slug: "acme", HTML: pageHTML(1), Publish: true})
	if !d1.Published || d1.Version.Status != "approved" || d1.Deduped || !d1.Live || d1.Gated {
		t.Fatalf("%+v", d1)
	}
	v1 := d1.Version.ID
	g := getPage(t, d, "ACME") // slug normalization
	if g.Page.PublishedVersionID != v1 || g.Published == nil || g.Published.ID != v1 || g.Published.HTML != pageHTML(1) {
		t.Fatalf("pointer is v1: %+v", g)
	}

	d2 := deploy(t, d, agent, DeployInput{Slug: "acme", HTML: pageHTML(2), Publish: true})
	v2 := d2.Version.ID
	if v2 == v1 || getPage(t, d, "acme").Page.PublishedVersionID != v2 {
		t.Fatal("v2 moves the pointer")
	}
	hist := must(ListVersions(ctx, d, "acme", ListVersionsOptions{}))(t)
	if len(hist) != 2 || hist[0].ID != v2 || !hist[0].IsPublished || !hist[0].IsLive || hist[1].IsPublished {
		t.Fatalf("history newest-first: %+v", hist)
	}

	rb := must(Rollback(ctx, d, RollbackInput{Slug: "acme", VersionID: v1}, agent))(t)
	if rb.ID != v1 || rb.Status != "approved" || getPage(t, d, "acme").Page.PublishedVersionID != v1 {
		t.Fatal("rollback moves the pointer back to v1")
	}

	dup := deploy(t, d, agent, DeployInput{Slug: "acme", HTML: pageHTML(1)})
	if !dup.Deduped || dup.Version.ID != v1 || !dup.Live || dup.Published {
		t.Fatalf("dedupe returns the live row: %+v", dup)
	}

	_, err = Rollback(ctx, d, RollbackInput{Slug: "acme", VersionID: v2, ExpectedVersion: 999999.0}, agent)
	wantCode(t, err, 409, "stale_version")

	// Approval gate: an agent's deploy+publish lands pending; publish is forbidden.
	must(CreatePage(ctx, d, CreatePageInput{Slug: "gated", Title: "Gated", RequireApproval: true}, agent))(t)
	gd := deploy(t, d, agent, DeployInput{Slug: "gated", HTML: pageHTML(9), Publish: true})
	if !gd.Gated || gd.Published || gd.Version.Status != "pending" || gd.Live {
		t.Fatalf("%+v", gd)
	}
	_, err = Publish(ctx, d, PublishInput{Slug: "gated", VersionID: gd.Version.ID}, agent)
	wantCode(t, err, 403, "approval_required")
	if getPage(t, d, "gated").Page.PublishedVersionID != 0 {
		t.Fatal("gated page stays unpublished")
	}

	// rename + soft delete + slug reuse
	if r := must(SetTitle(ctx, d, "acme", "  Acme Renamed  ", agent))(t); r.Title != "Acme Renamed" {
		t.Fatal(r)
	}
	if r := must(DeletePage(ctx, d, "acme", agent))(t); !r.Deleted {
		t.Fatal(r)
	}
	_, err = GetPage(ctx, d, "acme")
	wantCode(t, err, 404, "page_not_found")
	must(CreatePage(ctx, d, CreatePageInput{Slug: "acme", Title: "Acme 2"}, agent))(t)

	// password: agents set; clearing is admin-only
	if r := must(SetPassword(ctx, d, "acme", "letmein", agent))(t); !r.HasPassword {
		t.Fatal(r)
	}
	if !getPage(t, d, "acme").Page.HasPassword {
		t.Fatal("has_password")
	}
	_, err = SetPassword(ctx, d, "acme", "", agent)
	wantCode(t, err, 403, "admin_only")

	_, err = DeletePage(ctx, d, "gated", agent)
	wantCode(t, err, 403, "approval_required")
	must(d.Exec(ctx, "UPDATE pages SET disabled = true WHERE slug = 'acme'"))(t)
	_, err = DeletePage(ctx, d, "acme", agent)
	wantCode(t, err, 403, "disabled_takedown")

	// nested slugs are first-class
	must(CreatePage(ctx, d, CreatePageInput{Slug: "nested/q2", Title: "Nested"}, agent))(t)
	nd := deploy(t, d, agent, DeployInput{Slug: "nested/q2", HTML: pageHTML(7), Publish: true})
	if must(GetVersion(ctx, d, "nested/q2", nd.Version.ID))(t).ID != nd.Version.ID {
		t.Fatal("nested get version")
	}
	// A real version id asked for on the wrong page is a 404, never another page's html.
	_, err = GetVersion(ctx, d, "gated", nd.Version.ID)
	wantCode(t, err, 404, "version_not_found")
	if e, _ := apierror.As(err); e.Message != fmt.Sprintf("version %d not found on page gated", nd.Version.ID) {
		t.Fatal(e.Message)
	}

	// reserved slugs are rejected at creation and restoration
	_, err = CreatePage(ctx, d, CreatePageInput{Slug: "northwind/raw-template/report"}, agent)
	wantCode(t, err, 400, "reserved_slug")
	_, err = RestorePage(ctx, d, "northwind/preflight/report", admin)
	wantCode(t, err, 400, "reserved_slug")
}

// Boolean intent must survive JSON transport; rejected inputs create nothing.
func TestBooleanIntentHasNoSideEffects(t *testing.T) {
	d, ctx := setup(t)
	before := counts(t, d)
	for _, bad := range []any{"false", "true", 0.0, 1.0, Null, map[string]any{}, []any{}} {
		_, err := CreatePage(ctx, d, CreatePageInput{Slug: "northwind/bool-invalid", RequireApproval: bad}, agent)
		wantCode(t, err, 400, "bad_boolean")
		_, err = Deploy(ctx, d, DeployInput{Slug: "northwind/bool-invalid", HTML: pageHTML(81), Publish: bad}, agent)
		wantCode(t, err, 400, "bad_boolean")
	}
	if counts(t, d) != before {
		t.Fatal("invalid booleans have no mutation side effects")
	}
	p := must(CreatePage(ctx, d, CreatePageInput{Slug: "northwind/bools", RequireApproval: false}, agent))(t)
	if p.RequireApproval {
		t.Fatal("require_approval")
	}
	for i, publish := range []any{nil, false} {
		r := deploy(t, d, agent, DeployInput{Slug: "northwind/bools", HTML: pageHTML(82 + i), Publish: publish})
		if r.Published || r.Version.Status != "draft" {
			t.Fatalf("%+v", r)
		}
	}
	if getPage(t, d, "northwind/bools").Page.PublishedVersionID != 0 {
		t.Fatal("omitted publish defaults to false")
	}
}

// Dedupe is scoped: same bytes + mode dedupes; a mode change is a new row;
// rejected content re-enters as a fresh publishable row.
func TestDedupeScoping(t *testing.T) {
	d, ctx := setup(t)
	must(CreatePage(ctx, d, CreatePageInput{Slug: "dedup"}, agent))(t)
	w1 := deploy(t, d, agent, DeployInput{Slug: "dedup", HTML: pageHTML(42)})
	w2 := deploy(t, d, agent, DeployInput{Slug: "dedup", HTML: pageHTML(42)})
	if !w2.Deduped || w2.Version.ID != w1.Version.ID {
		t.Fatal("identical bytes + mode dedupe")
	}
	w3 := deploy(t, d, agent, DeployInput{Slug: "dedup", HTML: pageHTML(42), RenderMode: "raw"})
	if w3.Deduped || w3.Version.RenderMode != "raw" || w3.Version.ID == w1.Version.ID {
		t.Fatal("render mode change → new version")
	}
	rejected := must(Reject(ctx, d, ReviewInput{Slug: "dedup", VersionID: w1.Version.ID, Note: "no"}, admin))(t)
	if rejected.Status != "rejected" || *rejected.ReviewedBy != admin.Actor {
		t.Fatal(rejected)
	}
	w4 := deploy(t, d, agent, DeployInput{Slug: "dedup", HTML: pageHTML(42), Publish: true})
	if w4.Deduped || w4.Version.ID == w1.Version.ID || !w4.Published || !w4.Live {
		t.Fatalf("rejected content re-enters: %+v", w4)
	}
	_, err := Publish(ctx, d, PublishInput{Slug: "dedup", VersionID: w1.Version.ID}, admin)
	wantCode(t, err, 409, "not_draft")
	_, err = Reject(ctx, d, ReviewInput{Slug: "dedup", VersionID: w4.Version.ID}, admin)
	wantCode(t, err, 409, "not_rejectable")
}

// expected_version must hold on EVERY deploy, not only pointer moves.
func TestExpectedVersionOnEveryDeploy(t *testing.T) {
	d, ctx := setup(t)
	must(CreatePage(ctx, d, CreatePageInput{Slug: "stalegate"}, agent))(t)
	stale := deploy(t, d, agent, DeployInput{Slug: "stalegate", HTML: "<h1>one</h1>", Publish: true}).Version.ID
	deploy(t, d, agent, DeployInput{Slug: "stalegate", HTML: "<h1>two</h1>", Publish: true})

	_, err := Deploy(ctx, d, DeployInput{Slug: "stalegate", HTML: "<h1>three</h1>", Publish: false, ExpectedVersion: stale.String()}, agent)
	wantCode(t, err, 409, "stale_version")
	must(SetApproval(ctx, d, "stalegate", true, admin))(t)
	_, err = Deploy(ctx, d, DeployInput{Slug: "stalegate", HTML: "<h1>four</h1>", Publish: true, ExpectedVersion: float64(stale)}, agent)
	wantCode(t, err, 409, "stale_version")
	if e, _ := apierror.As(err); !strings.Contains(e.Message, "retry once with expected_version") {
		t.Fatal(e.Message)
	}
	live := getPage(t, d, "stalegate").Page.PublishedVersionID
	fresh := deploy(t, d, agent, DeployInput{Slug: "stalegate", HTML: "<h1>five</h1>", ExpectedVersion: live.String()})
	if fresh.Version.Status != "pending" {
		t.Fatal("a current expected_version is accepted on a gated page")
	}
	// A stale expected_version on a disabled page answers stale_version, not takedown.
	must(SetApproval(ctx, d, "stalegate", false, admin))(t)
	must(SetDisabled(ctx, d, "stalegate", true, admin))(t)
	_, err = Deploy(ctx, d, DeployInput{Slug: "stalegate", HTML: "<h1>six</h1>", Publish: true, ExpectedVersion: stale.String()}, agent)
	wantCode(t, err, 409, "stale_version")
}

// Bounded lock waits: a wedged transaction makes mutations on THAT page fail
// fast with a clean 503 while other pages keep working.
func TestLockTimeoutIs503(t *testing.T) {
	d, ctx := setup(t)
	must(CreatePage(ctx, d, CreatePageInput{Slug: "wedged"}, agent))(t)
	must(CreatePage(ctx, d, CreatePageInput{Slug: "other"}, agent))(t)
	wedge, err := d.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wedge.Rollback(ctx) }()
	must(wedge.Exec(ctx, "SELECT id FROM pages WHERE slug = 'wedged' FOR UPDATE"))(t)
	start := time.Now()
	_, err = SetTitle(ctx, d, "wedged", "Blocked", agent)
	waited := time.Since(start)
	mapped := apierror.FromDB(err)
	if mapped == nil || mapped.Status != 503 || mapped.Code != "db_lock_timeout" {
		t.Fatalf("lock wait is bounded → 503, got %v", err)
	}
	if waited < 900*time.Millisecond || waited > 10*time.Second {
		t.Fatalf("waited %v", waited)
	}
	must(SetTitle(ctx, d, "other", "Unaffected", agent))(t)
}

// Two writers racing from the same expected version: the page lock
// serializes them and exactly one moves the pointer.
func TestConcurrentPublishRace(t *testing.T) {
	d, ctx := setup(t)
	must(CreatePage(ctx, d, CreatePageInput{Slug: "race"}, agent))(t)
	base := deploy(t, d, agent, DeployInput{Slug: "race", HTML: pageHTML(1), Publish: true}).Version.ID
	a := deploy(t, d, agent, DeployInput{Slug: "race", HTML: pageHTML(2)}).Version.ID
	b := deploy(t, d, agent, DeployInput{Slug: "race", HTML: pageHTML(3)}).Version.ID
	for round := 0; round < 2; round++ {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		var targets []jsx.ID
		if round == 0 {
			targets = []jsx.ID{a, b}
		}
		if round == 1 {
			// Deploy+publish racing (the update_page fast path).
			targets = nil
		}
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if targets != nil {
					_, errs[i] = Publish(ctx, d, PublishInput{Slug: "race", VersionID: targets[i], ExpectedVersion: base.String()}, agent)
				} else {
					_, errs[i] = Deploy(ctx, d, DeployInput{Slug: "race", HTML: pageHTML(10 + i), Publish: true, ExpectedVersion: base.String()}, agent)
				}
			}(i)
		}
		wg.Wait()
		ok, stale := 0, 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else if e, isAPI := apierror.As(err); isAPI && e.Code == "stale_version" {
				stale++
			} else {
				t.Fatalf("unexpected: %v", err)
			}
		}
		if ok != 1 || stale != 1 {
			t.Fatalf("round %d: ok=%d stale=%d", round, ok, stale)
		}
		base = getPage(t, d, "race").Page.PublishedVersionID
	}
}

// create-or-deploy is atomic and converges under a slug race.
func TestCreateAndDeploy(t *testing.T) {
	d, ctx := setup(t)
	before := counts(t, d)
	_, err := CreateAndDeploy(ctx, d, CreateAndDeployInput{
		DeployInput: DeployInput{Slug: "atomic-fail", HTML: pageHTML(40), Source: SourceMCP, Publish: true, ExpectedVersion: "1"},
		Title:       "Must Roll Back",
	}, agent)
	wantCode(t, err, 409, "stale_version")
	if counts(t, d) != before {
		t.Fatal("a failed create-and-deploy leaves no page, version or audit row")
	}
	_, err = CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "portal/x", HTML: pageHTML(1)}}, agent)
	wantCode(t, err, 400, "reserved_slug")

	var wg sync.WaitGroup
	results := make([]*CreateDeployResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = CreateAndDeploy(ctx, d, CreateAndDeployInput{
				DeployInput: DeployInput{Slug: "atomic-race", HTML: pageHTML(41 + i), Source: SourceMCP, Publish: true},
				Title:       fmt.Sprintf("Race %d", i),
			}, agent)
		}(i)
	}
	wg.Wait()
	createdCount := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i].Created {
			createdCount++
		}
		if !results[i].Published {
			t.Fatal("both racers complete normally")
		}
	}
	if createdCount != 1 {
		t.Fatalf("one racer creates the page, got %d", createdCount)
	}
	b, _ := json.Marshal(results[0])
	if !strings.HasPrefix(string(b), `{"created":`) || !strings.Contains(string(b), `"deduped":false,"published":true,"gated":false,"live":true}`) {
		t.Fatal(string(b))
	}

	// AssertExistingPage runs only for an existing page, with it locked.
	called := false
	_, err = CreateAndDeployWithTxForTest(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "atomic-race", HTML: pageHTML(50)}},
		func(_ context.Context, p *LockedPage) error {
			called = true
			return apierror.Conflict("page exists", "page_exists", nil)
		})
	wantCode(t, err, 409, "page_exists")
	if !called {
		t.Fatal("assertExistingPage ran")
	}
	r := must(CreateAndDeployWithTxForTest(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "brand-new", HTML: pageHTML(51)}, RequireApproval: true},
		func(context.Context, *LockedPage) error { return fmt.Errorf("must not run for a created page") }))(t)
	if !r.Created || !r.Gated || r.Version.Status != "pending" {
		t.Fatalf("%+v", r)
	}
}

func CreateAndDeployWithTxForTest(ctx context.Context, d *db.DB, in CreateAndDeployInput, assert func(context.Context, *LockedPage) error) (*CreateDeployResult, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := CreateAndDeployWithTx(ctx, tx, in, agent, CreateAndDeployOptions{AssertExistingPage: assert})
	if err != nil {
		return nil, err
	}
	return r, tx.Commit(ctx)
}

// A gate added after a live release; then removed again.
func TestApprovalGateTransitions(t *testing.T) {
	d, ctx := setup(t)
	must(CreatePage(ctx, d, CreatePageInput{Slug: "mcpgated"}, agent))(t)
	prior := deploy(t, d, agent, DeployInput{Slug: "mcpgated", HTML: pageHTML(10), Publish: true})
	must(d.Exec(ctx, "UPDATE pages SET require_approval = true WHERE slug = 'mcpgated'"))(t)
	gated := deploy(t, d, agent, DeployInput{Slug: "mcpgated", HTML: pageHTML(11), Publish: true})
	if gated.Version.Status != "pending" || gated.Published || gated.Live {
		t.Fatalf("%+v", gated)
	}
	if getPage(t, d, "mcpgated").Page.PublishedVersionID != prior.Version.ID {
		t.Fatal("prior stays live")
	}
	must(d.Exec(ctx, "UPDATE pages SET require_approval = false WHERE slug = 'mcpgated'"))(t)
	draft := deploy(t, d, agent, DeployInput{Slug: "mcpgated", HTML: pageHTML(11)})
	if draft.Deduped || draft.Version.Status != "draft" {
		t.Fatal("open page skips an unusable gated-era pending row")
	}
	must(Publish(ctx, d, PublishInput{Slug: "mcpgated", VersionID: draft.Version.ID, ExpectedVersion: prior.Version.ID.String()}, agent))(t)
	// …and symmetrically a gated page skips open-era drafts.
	open := deploy(t, d, agent, DeployInput{Slug: "mcpgated", HTML: pageHTML(12)})
	must(SetApproval(ctx, d, "mcpgated", true, admin))(t)
	again := deploy(t, d, agent, DeployInput{Slug: "mcpgated", HTML: pageHTML(12)})
	if again.Deduped || again.Version.ID == open.Version.ID || again.Version.Status != "pending" {
		t.Fatalf("%+v", again)
	}
}

func TestTakedownAndDedupeOntoOlderApproved(t *testing.T) {
	d, ctx := setup(t)
	must(CreatePage(ctx, d, CreatePageInput{Slug: "auto"}, agent))(t)
	deploy(t, d, agent, DeployInput{Slug: "auto", HTML: pageHTML(20), Publish: true})
	must(SetDisabled(ctx, d, "auto", true, admin))(t)
	_, err := DeletePage(ctx, d, "auto", agent)
	wantCode(t, err, 403, "disabled_takedown")
	_, err = Deploy(ctx, d, DeployInput{Slug: "auto", HTML: pageHTML(22), Publish: true}, agent)
	wantCode(t, err, 403, "disabled_takedown")
	saved := deploy(t, d, agent, DeployInput{Slug: "auto", HTML: pageHTML(23)})
	if saved.Live || saved.Version.Status != "draft" {
		t.Fatal("a draft may still be saved on a disabled page")
	}
	hist := must(ListVersions(ctx, d, "auto", ListVersionsOptions{}))(t)
	if !hist[1].IsPublished || hist[1].IsLive {
		t.Fatal("published means pointer equality; live also requires not disabled")
	}
	_, err = Rollback(ctx, d, RollbackInput{Slug: "auto"}, agent)
	wantCode(t, err, 403, "disabled_takedown")
	must(SetDisabled(ctx, d, "auto", false, admin))(t)

	must(CreatePage(ctx, d, CreatePageInput{Slug: "dedupe"}, agent))(t)
	v1 := deploy(t, d, agent, DeployInput{Slug: "dedupe", HTML: pageHTML(30), Publish: true})
	if live := deploy(t, d, agent, DeployInput{Slug: "dedupe", HTML: pageHTML(30)}); !live.Live {
		t.Fatal("live duplicate")
	}
	deploy(t, d, agent, DeployInput{Slug: "dedupe", HTML: pageHTML(31), Publish: true})
	older := deploy(t, d, agent, DeployInput{Slug: "dedupe", HTML: pageHTML(30)})
	if older.Version.ID != v1.Version.ID || older.Live || older.Version.Status != "approved" {
		t.Fatalf("%+v", older)
	}
	// rollback with no version: the most recent approved version that isn't live
	rb := must(Rollback(ctx, d, RollbackInput{Slug: "dedupe", Note: "revert the chart change"}, agent))(t)
	if rb.ID != v1.Version.ID {
		t.Fatal("default rollback target")
	}
	var meta map[string]any
	noErr(t, d.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE action = 'rollback' ORDER BY id DESC LIMIT 1`).Scan(&meta))
	if meta["note"] != "revert the chart change" || meta["to"] != v1.Version.ID.String() || meta["from"] == nil {
		t.Fatalf("%v", meta)
	}
	must(CreatePage(ctx, d, CreatePageInput{Slug: "single"}, agent))(t)
	deploy(t, d, agent, DeployInput{Slug: "single", HTML: pageHTML(1), Publish: true})
	_, err = Rollback(ctx, d, RollbackInput{Slug: "single"}, agent)
	wantCode(t, err, 409, "no_rollback_target")
}

// test/admin.integration.js state-machine parts.
func TestAdminFlow(t *testing.T) {
	d, ctx := setup(t)
	must(CreatePage(ctx, d, CreatePageInput{Slug: "adm"}, admin))(t)
	v1 := deploy(t, d, admin, DeployInput{Slug: "adm", HTML: pageHTML(1), Publish: true}).Version.ID
	v2 := deploy(t, d, admin, DeployInput{Slug: "adm", HTML: pageHTML(2)}).Version.ID
	must(Publish(ctx, d, PublishInput{Slug: "adm", VersionID: v2, ExpectedVersion: v1}, admin))(t)
	must(Rollback(ctx, d, RollbackInput{Slug: "adm", VersionID: v1, ExpectedVersion: v2}, admin))(t)
	if getPage(t, d, "adm").Page.PublishedVersionID != v1 {
		t.Fatal("rolled back")
	}
	_, err := Rollback(ctx, d, RollbackInput{Slug: "adm", VersionID: v2, ExpectedVersion: 999999}, admin)
	wantCode(t, err, 409, "stale_version")

	must(CreatePage(ctx, d, CreatePageInput{Slug: "admg", RequireApproval: true}, admin))(t)
	p1 := deploy(t, d, agent, DeployInput{Slug: "admg", HTML: pageHTML(1), Publish: true}).Version
	p2 := deploy(t, d, agent, DeployInput{Slug: "admg", HTML: pageHTML(2)}).Version
	if p1.Status != "pending" || p2.Status != "pending" {
		t.Fatal("pending")
	}
	_, err = Approve(ctx, d, ReviewInput{Slug: "admg", VersionID: p1.ID}, agent)
	wantCode(t, err, 403, "admin_only")
	ap := must(Approve(ctx, d, ReviewInput{Slug: "admg", VersionID: p1.ID}, admin))(t)
	if ap.Status != "approved" || *ap.ReviewedBy != admin.Actor || getPage(t, d, "admg").Page.PublishedVersionID != p1.ID {
		t.Fatal("approve publishes")
	}
	_, err = Approve(ctx, d, ReviewInput{Slug: "admg", VersionID: p1.ID}, admin)
	wantCode(t, err, 409, "not_pending")
	must(Reject(ctx, d, ReviewInput{Slug: "admg", VersionID: p2.ID, Note: "no"}, admin))(t)
	_, err = Approve(ctx, d, ReviewInput{Slug: "admg", VersionID: 999999}, admin)
	wantCode(t, err, 404, "version_not_found")
	// An admin may publish/delete a gated page.
	if r := must(DeletePage(ctx, d, "admg", admin))(t); !r.Deleted {
		t.Fatal(r)
	}
	if r := must(RestorePage(ctx, d, "admg", admin))(t); !r.Restored {
		t.Fatal(r)
	}
	if getPage(t, d, "admg").Page.PublishedVersionID != 0 {
		t.Fatal("a restored page has no live pointer")
	}
	_, err = RestorePage(ctx, d, "admg", admin)
	wantCode(t, err, 409, "slug_taken")
	_, err = RestorePage(ctx, d, "never-existed", admin)
	wantCode(t, err, 404, "page_not_found")
	_, err = RestorePage(ctx, d, "admg", agent)
	wantCode(t, err, 403, "admin_only")
	must(d.Exec(ctx, `INSERT INTO pages (slug, title, deleted_at) VALUES ('portal/legacy', 'Predates the reservation', now())`))(t)
	_, err = RestorePage(ctx, d, "portal/legacy", admin)
	wantCode(t, err, 400, "reserved_slug")

	if r := must(SetApproval(ctx, d, "adm", true, admin))(t); !r.RequireApproval {
		t.Fatal(r)
	}
	_, err = SetApproval(ctx, d, "adm", false, agent)
	wantCode(t, err, 403, "admin_only")
	_, err = SetApproval(ctx, d, "adm", Null, admin)
	wantCode(t, err, 400, "bad_boolean")
	if r := must(SetDisabled(ctx, d, "adm", true, admin))(t); !r.Disabled || !getPage(t, d, "adm").Page.Disabled {
		t.Fatal(r)
	}
	must(SetDisabled(ctx, d, "adm", false, admin))(t)

	// themes
	themes := must(ListThemes(ctx, d))(t)
	if len(themes) == 0 || themes[0].Name != "flag" {
		t.Fatalf("%+v", themes)
	}
	_, err = SetTheme(ctx, d, "adm", "no-such-theme", admin)
	wantCode(t, err, 400, "unknown_theme")
	if r := must(SetTheme(ctx, d, "adm", "", admin))(t); r.Theme != "flag" {
		t.Fatal(r)
	}
	if getPage(t, d, "adm").Page.ThemeName != "flag" {
		t.Fatal("theme name")
	}
	_, err = SetTheme(ctx, d, "adm", "flag", agent)
	wantCode(t, err, 403, "admin_only")

	// Every mutation above left exactly the audit trail it claims.
	var actions []string
	rows, _ := d.Query(ctx, `SELECT DISTINCT action FROM audit_log ORDER BY action`)
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	want := "approve,create_page,delete_page,deploy,disable,enable,publish,reject,restore_page,rollback,set_approval,set_theme"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit actions %v", actions)
	}
}

// List pages and history: bounded keyset pagination, filters, admin ordinals.
func TestListPagination(t *testing.T) {
	d, ctx := setup(t)
	for i := 0; i < 3; i++ {
		must(CreatePage(ctx, d, CreatePageInput{Slug: fmt.Sprintf("northwind/p%d", i), Title: "Northwind", ClientID: "northwind"}, agent))(t)
	}
	must(CreatePage(ctx, d, CreatePageInput{Slug: "contoso", Title: "Contoso"}, agent))(t)
	deploy(t, d, agent, DeployInput{Slug: "contoso", HTML: pageHTML(1), Publish: true})
	deploy(t, d, agent, DeployInput{Slug: "contoso", HTML: pageHTML(2)})
	all := must(ListPages(ctx, d, ListPagesOptions{}))(t)
	if len(all) != 4 || all[0].Slug != "contoso" || !all[0].IsLive {
		t.Fatalf("%+v", all)
	}
	two := 2
	first := must(ListPages(ctx, d, ListPagesOptions{Limit: &two}))(t)
	last := first[len(first)-1]
	next := must(ListPages(ctx, d, ListPagesOptions{Limit: &two, After: &After{CreatedAt: last.CreatedAt.Time, ID: last.ID.String()}}))(t)
	if len(first) != 2 || len(next) != 2 || next[0].ID == last.ID || next[1].Slug != "northwind/p0" {
		t.Fatal("keyset pagination")
	}
	q := "NORTH"
	if got := must(ListPages(ctx, d, ListPagesOptions{Query: &q}))(t); len(got) != 3 {
		t.Fatal("query")
	}
	cid := "northwind"
	if got := must(ListPages(ctx, d, ListPagesOptions{ClientIDSet: true, ClientID: &cid}))(t); len(got) != 3 {
		t.Fatal("client")
	}
	live := true
	if got := must(ListPages(ctx, d, ListPagesOptions{IsLive: &live}))(t); len(got) != 1 {
		t.Fatal("is_live")
	}
	if got := must(ListPages(ctx, d, ListPagesOptions{WorkspaceFilterSet: true, WorkspaceID: "ungrouped"}))(t); len(got) != 4 {
		t.Fatal("ungrouped")
	}
	numbered := must(ListPages(ctx, d, ListPagesOptions{IncludeVersionNumbers: true}))(t)
	if numbered[0].VersionCount != 2 || *numbered[0].PublishedVersionNumber != 1 ||
		numbered[1].PublishedVersionNumber != nil {
		t.Fatalf("%+v %+v", numbered[0].VersionNumbers, numbered[1].VersionNumbers)
	}
	one := 1
	h1 := must(ListVersions(ctx, d, "contoso", ListVersionsOptions{Limit: &one}))(t)
	h2 := must(ListVersions(ctx, d, "contoso", ListVersionsOptions{Limit: &one, After: &After{CreatedAt: h1[0].CreatedAt.Time, ID: h1[0].ID}}))(t)
	if len(h2) != 1 || h2[0].ID == h1[0].ID {
		t.Fatal("version pagination")
	}
	status := "approved"
	if got := must(ListVersions(ctx, d, "contoso", ListVersionsOptions{Status: &status}))(t); len(got) != 1 || !got[0].IsLive {
		t.Fatal("status filter")
	}
	bad := "live"
	_, err := ListVersions(ctx, d, "contoso", ListVersionsOptions{Status: &bad})
	wantCode(t, err, 400, "bad_version_status")
}

// ── managed data ────────────────────────────────────────────────────────────

const dataSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,` +
	`"required":["count","label"],"properties":{"count":{"type":"integer","minimum":0},"label":{"type":"string"}}}`

func managedHTML(sourceAsOf, data string) string {
	refreshed := jsx.FormatTime(time.Now().Add(-5 * time.Hour))
	return `<!doctype html><html><head><title>Managed</title></head><body>` +
		`<main data-layout="preserve-me"><h1>Managed dashboard</h1></main>` +
		`<script id="pages-data-schema" type="application/schema+json">` + dataSchema + `</script>` +
		`<script id="pages-data" type="application/json">{"contract_version":1,"refreshed_at":"` + refreshed +
		`","source_as_of":"` + sourceAsOf + `","data":` + data + `}</script>` +
		`<footer>immutable layout tail</footer></body></html>`
}

func hoursAgo(h float64) string {
	return jsx.FormatTime(time.Now().Add(-time.Duration(h * float64(time.Hour))))
}

func jv(t *testing.T, s string) any {
	t.Helper()
	v, err := jsonv.ParseString(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestManagedData(t *testing.T) {
	d, ctx := setup(t)
	minted := must(tokens.Mint(ctx, d, tokens.MintParams{Label: "data-agent"}))(t)
	bot := Actor{Actor: "data-agent", ActorType: "agent", TokenID: minted.ID, IP: "127.0.0.1"}
	source0, source1, source2, source3 := hoursAgo(4), hoursAgo(3), hoursAgo(2), hoursAgo(1)
	initialHTML := managedHTML(source0, `{"count":1,"label":"initial"}`)
	base := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "mcpdata", HTML: initialHTML, Source: SourceMCP, Publish: true}, Title: "Managed Data"}, bot))(t)
	v0 := base.Version.ID
	read := must(GetPageData(ctx, d, "mcpdata"))(t)
	if read.Version.ID != v0 || jsonv.Stringify(read.Envelope.Value("data")) != `{"count":1,"label":"initial"}` || len(read.DataSHA256) != 64 {
		t.Fatalf("%+v", read)
	}
	if read.Freshness == nil || jsx.FormatTime(read.Freshness.SourceAsOf.Time) != source0 || read.Freshness.CheckedAt != read.Freshness.RefreshedAt {
		t.Fatalf("freshness %+v", read.Freshness)
	}
	b, _ := json.Marshal(read)
	if !strings.HasPrefix(string(b), `{"page":{"id":"`) || strings.Contains(string(b), `"html"`) {
		t.Fatal(string(b)[:200])
	}

	_, err := UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, `{"count":-1,"label":4}`), SourceAsOf: source1, ExpectedVersion: v0.String()}, bot)
	wantCode(t, err, 400, "data_validation_failed")
	_, err = UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, `{"count":2,"label":"x"}`), SourceAsOf: source1}, bot)
	wantCode(t, err, 400, "expected_version_required")

	injected := `{"count":2,"label":"</script><img src=x onerror=alert(1)>"}`
	canary := must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, injected), SourceAsOf: source1, ExpectedVersion: v0.String(), Publish: false, Note: "canary"}, bot))(t)
	if canary.Published || canary.Live || canary.Version.Status != "draft" || *canary.Version.Note != "canary" {
		t.Fatalf("%+v", canary)
	}
	stored := must(GetVersion(ctx, d, "mcpdata", canary.Version.ID))(t)
	pi := must(pagedata.ParseManagedHTML(initialHTML))(t)
	pc := must(pagedata.ParseManagedHTML(stored.HTML))(t)
	if stored.HTML[:pc.DataBlock.ContentStart] != initialHTML[:pi.DataBlock.ContentStart] ||
		!strings.HasSuffix(stored.HTML, initialHTML[pi.DataBlock.ContentEnd:]) {
		t.Fatal("bytes around the data block are preserved")
	}
	if strings.Contains(strings.ToLower(stored.HTML[pc.DataBlock.ContentStart:pc.DataBlock.ContentEnd]), "</script") {
		t.Fatal("data is escaped")
	}

	retry := must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, injected), SourceAsOf: source1, ExpectedVersion: v0.String(), Publish: false}, bot))(t)
	if !retry.Deduped || retry.Version.ID != canary.Version.ID || retry.Envelope.Value("refreshed_at") != canary.Envelope.Value("refreshed_at") {
		t.Fatal("exact retries dedupe and return the stored bytes")
	}
	for _, w := range retry.DataWarnings {
		if w.Code == "data_unchanged" || w.Code == "coverage_did_not_advance" {
			t.Fatal("a dedupe drops the stale-refresh pair")
		}
	}
	pub := must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, injected), SourceAsOf: source1, ExpectedVersion: v0.String()}, bot))(t)
	if !pub.Deduped || !pub.Published || pub.Version.ID != canary.Version.ID || !pub.Live {
		t.Fatalf("publishing the canary: %+v", pub)
	}
	adv := must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, injected), SourceAsOf: source2, ExpectedVersion: canary.Version.ID}, bot))(t)
	if adv.Deduped || adv.Version.ID == canary.Version.ID {
		t.Fatal("new coverage creates a version")
	}
	v2 := adv.Version.ID
	beforeAudit := counts(t, d)[2]
	exact := must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, injected), SourceAsOf: source2, ExpectedVersion: v2}, bot))(t)
	if !exact.Deduped || exact.Published || !exact.Live || counts(t, d)[2] != beforeAudit {
		t.Fatal("an exact live retry does not rewrite the pointer/audit row")
	}
	_, err = UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, `{"count":3,"label":"regress"}`), SourceAsOf: source1, ExpectedVersion: v2}, bot)
	wantCode(t, err, 409, "source_regression")
	if e, _ := apierror.As(err); jsonv.Stringify(e.Details) != `{"current_source_as_of":"`+source2+`"}` {
		t.Fatal(jsonv.Stringify(e.Details))
	}
	_, err = UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, `{"count":3,"label":"future"}`), SourceAsOf: jsx.FormatTime(time.Now().Add(time.Hour)), ExpectedVersion: v2}, bot)
	wantCode(t, err, 400, "source_in_future")
	_, err = UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, `{"count":3,"label":"stale"}`), SourceAsOf: source3, ExpectedVersion: canary.Version.ID}, bot)
	wantCode(t, err, 409, "stale_version")
	_, err = UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, `{"count":3,"label":"x"}`), SourceAsOf: source3, ExpectedVersion: v2, Expect: jv(t, `{"row_count":{"rows":5}}`)}, bot)
	if err == nil {
		t.Fatal("expect reconciliation refuses a mismatched payload")
	}

	// Overlapping writers serialize on the page lock; exactly one wins.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wins := make([]*DataUpdateResult, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wins[i], errs[i] = UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, fmt.Sprintf(`{"count":%d,"label":"overlap"}`, 4+i)), SourceAsOf: source3, ExpectedVersion: v2}, bot)
		}(i)
	}
	wg.Wait()
	var winner *DataUpdateResult
	stale := 0
	for i := range errs {
		if errs[i] == nil {
			winner = wins[i]
		} else if e, ok := apierror.As(errs[i]); ok && e.Code == "stale_version" {
			stale++
		} else {
			t.Fatal(errs[i])
		}
	}
	if winner == nil || stale != 1 || !winner.Live {
		t.Fatal("one overlapping writer wins")
	}
	var action string
	var tokenID jsx.ID
	var meta map[string]any
	noErr(t, d.QueryRow(ctx, `SELECT action, token_id, metadata FROM audit_log WHERE version_id = $1 AND action = 'data_update'`, winner.Version.ID).Scan(&action, &tokenID, &meta))
	if tokenID != minted.ID || meta["source_as_of"] != source3 || meta["deduped"] != false {
		t.Fatalf("audit attribution %v %v", tokenID, meta)
	}
	_, err = d.Exec(ctx, "UPDATE page_versions SET data_sha256 = repeat('0', 64) WHERE id = $1", winner.Version.ID)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatal("structured-data identity is immutable", err)
	}

	// Unmanaged pages fail with the bounded opt-in error.
	must(CreatePage(ctx, d, CreatePageInput{Slug: "plain"}, bot))(t)
	_, err = GetPageData(ctx, d, "plain")
	wantCode(t, err, 409, "page_not_data_managed")
	deploy(t, d, bot, DeployInput{Slug: "plain", HTML: pageHTML(1), Publish: true})
	_, err = GetPageData(ctx, d, "plain")
	wantCode(t, err, 409, "page_not_data_managed")

	// Gated and disabled pages keep their governance.
	gb := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "gateddata", HTML: managedHTML(source0, `{"count":1,"label":"a"}`), Publish: true}}, bot))(t)
	must(d.Exec(ctx, "UPDATE pages SET require_approval = true WHERE slug = 'gateddata'"))(t)
	gd := must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "gateddata", Data: jv(t, `{"count":2,"label":"pending"}`), SourceAsOf: source1, ExpectedVersion: gb.Version.ID}, bot))(t)
	if !gd.Gated || gd.Version.Status != "pending" || gd.Live {
		t.Fatal("gated data lands pending")
	}
	must(d.Exec(ctx, "UPDATE pages SET disabled = true WHERE slug = 'mcpdata'"))(t)
	_, err = UpdatePageData(ctx, d, DataUpdateInput{Slug: "mcpdata", Data: jv(t, `{"count":6,"label":"disabled"}`), SourceAsOf: hoursAgo(0.5), ExpectedVersion: winner.Version.ID}, bot)
	wantCode(t, err, 403, "disabled_takedown")
	disabledRead := must(GetPageData(ctx, d, "mcpdata"))(t)
	if disabledRead.Page.PublishedVersionID != winner.Version.ID || !disabledRead.Page.Disabled {
		t.Fatal("published pointer is retained during takedown")
	}
	must(d.Exec(ctx, "UPDATE pages SET disabled = false WHERE slug = 'mcpdata'"))(t)

	// A no-op refresh is recordable without a version.
	beforeCheck := must(GetPageData(ctx, d, "mcpdata"))(t)
	nv := counts(t, d)[1]
	rec := must(RecordRefreshCheck(ctx, d, RefreshCheckInput{Slug: "mcpdata", Outcome: "source_not_updated",
		Detail: "  upstream max date unchanged  ", SourceAsOfSeen: beforeCheck.Envelope.Value("source_as_of")}, bot))(t)
	if *rec.Freshness.LastCheckOutcome != "source_not_updated" || *rec.Freshness.DaysSinceCheck != 0 ||
		rec.Freshness.SourceAsOf != beforeCheck.Freshness.SourceAsOf || *rec.Freshness.LatestOutcome != "source_not_updated" {
		t.Fatalf("%+v", rec.Freshness)
	}
	after := must(GetPageData(ctx, d, "mcpdata"))(t)
	if after.DataSHA256 != beforeCheck.DataSHA256 || after.Page.PublishedVersionID != beforeCheck.Page.PublishedVersionID ||
		*after.Freshness.LastCheckDetail != "upstream max date unchanged" || after.Freshness.CheckedAt.Before(after.Freshness.RefreshedAt.Time) {
		t.Fatalf("%+v", after.Freshness)
	}
	if counts(t, d)[1] != nv {
		t.Fatal("recording a check creates no version")
	}
	listed := must(ListPages(ctx, d, ListPagesOptions{}))(t)
	for _, p := range listed {
		if p.Slug == "mcpdata" {
			lb, _ := json.Marshal(p.Freshness)
			ab, _ := json.Marshal(after.Freshness)
			if string(lb) != string(ab) {
				t.Fatalf("list and read agree:\n%s\n%s", lb, ab)
			}
		}
	}
	var checkMeta map[string]any
	noErr(t, d.QueryRow(ctx, `SELECT token_id, metadata FROM audit_log WHERE action = 'record_refresh_check'`).Scan(&tokenID, &checkMeta))
	if tokenID != minted.ID || checkMeta["outcome"] != "source_not_updated" || checkMeta["detail"] != "upstream max date unchanged" {
		t.Fatal(checkMeta)
	}
}

// test/source-metadata.integration.js
func TestSourceMetadataAndLegacyRows(t *testing.T) {
	d, ctx := setup(t)
	html := managedHTML("2026-08-01T00:00:00.000Z", `{"count":0,"label":"x"}`)
	indexed := func(id jsx.ID) {
		t.Helper()
		var h string
		var dataSHA, tplSHA *string
		var refreshed, source jsx.Time
		var tv jsx.ID
		noErr(t, d.QueryRow(ctx, `SELECT html, data_sha256, data_template_sha256, refreshed_at, source_as_of, template_version_id FROM page_versions WHERE id=$1`, id).
			Scan(&h, &dataSHA, &tplSHA, &refreshed, &source, &tv))
		m := must(pagedata.ParseManagedHTML(h))(t)
		if *dataSHA != m.DataSHA256 || *tplSHA != m.TemplateSHA256 || isoString(source.Time) != isoString(ParseTime(m.SourceAsOf())) ||
			isoString(refreshed.Time) != isoString(ParseTime(m.RefreshedAt())) || tv != 0 {
			t.Fatal("indexed metadata")
		}
	}
	inline := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "source-metadata-page", HTML: html, Publish: true}}, agent))(t)
	indexed(inline.Version.ID)
	_, err := UpdatePageData(ctx, d, DataUpdateInput{Slug: "source-metadata-page", Data: jv(t, `{"count":2,"label":"x"}`), SourceAsOf: "2025-08-02T00:00:00Z", ExpectedVersion: inline.Version.ID}, agent)
	wantCode(t, err, 409, "source_regression")

	// Full-source writes keep byte identity even when only a timestamp changes.
	changed := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "byte-dedupe", HTML: html, Publish: true}}, agent))(t)
	ref := ParseTime(must(pagedata.ParseManagedHTML(html))(t).RefreshedAt())
	alt := strings.Replace(html, jsx.FormatTime(ref), jsx.FormatTime(ref.Add(time.Hour)), 1)
	changedTime := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "byte-dedupe", HTML: alt, Publish: true}}, agent))(t)
	if changedTime.Version.ID == changed.Version.ID {
		t.Fatal("full-source writes retain byte identity")
	}
	retry := must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "byte-dedupe", Data: jv(t, `{"count":0,"label":"x"}`), SourceAsOf: "2026-08-01T00:00:00Z", ExpectedVersion: changedTime.Version.ID}, agent))(t)
	if retry.Version.ID != changedTime.Version.ID {
		t.Fatal("managed retries can see indexed full-source versions")
	}

	// Pre-index immutable rows are read without being rewritten.
	legacyPage := must(CreatePage(ctx, d, CreatePageInput{Slug: "legacy-source-metadata"}, agent))(t)
	var legacy jsx.ID
	noErr(t, d.QueryRow(ctx, `INSERT INTO page_versions(page_id,html,content_sha256,status,render_mode,author,source)
	   VALUES($1,$2,$3,'approved','themed','source-fixture','mcp') RETURNING id`, legacyPage.ID, html, SHA256(html)).Scan(&legacy))
	must(d.Exec(ctx, "UPDATE pages SET published_version_id=$1 WHERE id=$2", legacy, legacyPage.ID))(t)
	var listedFresh *Freshness
	for _, p := range must(ListPages(ctx, d, ListPagesOptions{}))(t) {
		if p.Slug == "legacy-source-metadata" {
			listedFresh = p.Freshness
		}
	}
	readFresh := must(GetPageData(ctx, d, "legacy-source-metadata"))(t).Freshness
	lb, _ := json.Marshal(listedFresh)
	rb, _ := json.Marshal(readFresh)
	if listedFresh == nil || string(lb) != string(rb) {
		t.Fatalf("legacy rows are readable:\n%s\n%s", lb, rb)
	}
	checked := must(RecordRefreshCheck(ctx, d, RefreshCheckInput{Slug: "legacy-source-metadata", Outcome: "source_not_updated", SourceAsOfSeen: "2026-08-01T00:00:00Z"}, agent))(t)
	if isoString(checked.Freshness.SourceAsOf.Time) != "2026-08-01T00:00:00.000Z" {
		t.Fatal("legacy coverage")
	}
	newer := strings.ReplaceAll(html, "2026-08-01", "2026-08-03")
	must(d.Exec(ctx, `INSERT INTO page_versions(page_id,html,content_sha256,status,render_mode,author,source)
	   VALUES($1,$2,$3,'draft','themed','source-fixture','mcp')`, legacyPage.ID, newer, SHA256(newer)))(t)
	_, err = UpdatePageData(ctx, d, DataUpdateInput{Slug: "legacy-source-metadata", Data: jv(t, `{"count":2,"label":"x"}`), SourceAsOf: "2026-08-02T00:00:00Z", ExpectedVersion: legacy}, agent)
	wantCode(t, err, 409, "source_regression")
	var nullSHA *string
	noErr(t, d.QueryRow(ctx, "SELECT data_sha256 FROM page_versions WHERE id=$1", legacy).Scan(&nullSHA))
	if nullSHA != nil {
		t.Fatal("legacy rows are not rewritten")
	}
	plain := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "plain-source-metadata", HTML: pageHTML(1), Publish: true}}, agent))(t)
	noErr(t, d.QueryRow(ctx, "SELECT data_sha256 FROM page_versions WHERE id=$1", plain.Version.ID).Scan(&nullSHA))
	if nullSHA != nil {
		t.Fatal("plain html has no data index")
	}
	for _, p := range must(ListPages(ctx, d, ListPagesOptions{}))(t) {
		if p.Slug == "plain-source-metadata" && p.Freshness != nil {
			t.Fatal("plain pages have no freshness")
		}
	}
}

func configHTML(campaign string) string {
	schema := func(props string, req string) string {
		return `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":[` + req + `],"properties":` + props + `}`
	}
	return `<!doctype html><html><head><title>Northwind</title></head><body><h1>Campaign</h1>` +
		`<script id="pages-config-schema" type="application/schema+json">` + schema(`{"campaign":{"type":"string"}}`, `"campaign"`) + `</script>` +
		`<script id="pages-config" type="application/json">{"campaign":"` + campaign + `"}</script>` +
		`<script id="pages-data-schema" type="application/schema+json">` + schema(`{"count":{"type":"number"}}`, `"count"`) + `</script>` +
		`<script id="pages-data" type="application/json">{"contract_version":1,"refreshed_at":"2026-08-01T00:00:00.000Z","source_as_of":"2026-08-01T00:00:00.000Z","data":{"count":0}}</script>` +
		`</body></html>`
}

func TestPageConfig(t *testing.T) {
	d, ctx := setup(t)
	base := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "cfg", HTML: configHTML("Northwind Spring"), Publish: true}}, agent))(t)
	cfg := must(GetPageConfig(ctx, d, "cfg"))(t)
	if jsonv.Stringify(cfg.Config) != `{"campaign":"Northwind Spring"}` || cfg.Version.ID != base.Version.ID {
		t.Fatal(jsonv.Stringify(cfg.Config))
	}
	before := must(GetPageData(ctx, d, "cfg"))(t)
	_, err := UpdatePageConfig(ctx, d, ConfigUpdateInput{Slug: "cfg", Config: jv(t, `{"campaign":"x"}`)}, agent)
	wantCode(t, err, 400, "expected_version_required")
	_, err = UpdatePageConfig(ctx, d, ConfigUpdateInput{Slug: "cfg", Config: jv(t, `{"campaign":5}`), ExpectedVersion: base.Version.ID}, agent)
	wantCode(t, err, 400, "config_validation_failed")
	up := must(UpdatePageConfig(ctx, d, ConfigUpdateInput{Slug: "cfg", Config: jv(t, `{"campaign":"Northwind Summer"}`), ExpectedVersion: base.Version.ID}, agent))(t)
	if up.Deduped || !up.Published || jsonv.Stringify(up.Config) != `{"campaign":"Northwind Summer"}` || up.DataSHA256 != before.DataSHA256 {
		t.Fatalf("%+v", up)
	}
	after := must(GetPageData(ctx, d, "cfg"))(t)
	if jsonv.Stringify(after.Envelope) != jsonv.Stringify(before.Envelope) || after.TemplateSHA256 == before.TemplateSHA256 {
		t.Fatal("config edits leave the data block byte-for-byte alone but move the template identity")
	}
	var action string
	noErr(t, d.QueryRow(ctx, `SELECT action FROM audit_log WHERE version_id = $1`, up.Version.ID).Scan(&action))
	if action != "config_update" {
		t.Fatal(action)
	}
	must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "nocfg", HTML: managedHTML(hoursAgo(1), `{"count":1,"label":"a"}`), Publish: true}}, agent))(t)
	_, err = GetPageConfig(ctx, d, "nocfg")
	wantCode(t, err, 409, "page_not_template_managed")
	must(CreatePage(ctx, d, CreatePageInput{Slug: "empty"}, agent))(t)
	_, err = GetPageConfig(ctx, d, "empty")
	wantCode(t, err, 409, "update_page_not_published")

	// A page that ships #pages-config without a schema gets one derived.
	noSchema := strings.Replace(configHTML("Fabrikam"), `<script id="pages-config-schema" type="application/schema+json">`, `<script id="ignored" type="application/schema+json">`, 1)
	gen := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{Slug: "derived", HTML: noSchema, Publish: true}}, agent))(t)
	if !gen.ConfigSchemaGenerated {
		t.Fatal("config_schema_generated")
	}
}

func TestSaveDeck(t *testing.T) {
	d, ctx := setup(t)
	deck := func(title string) string {
		return `<!doctype html><html><head><title>` + title + `</title></head><body><script type="application/bento+json" id="bento-doc">{}</script></body></html>`
	}
	must(CreatePage(ctx, d, CreatePageInput{Slug: "deck"}, admin))(t)
	first := deploy(t, d, admin, DeployInput{Slug: "deck", HTML: deck("one"), Publish: true})
	page := getPage(t, d, "deck").Page
	saved := must(SaveDeck(ctx, d, SaveDeckInput{Slug: "deck", PageID: page.ID, BaseVersion: first.Version.ID.String(), HTML: deck("two")}, admin))(t)
	if saved.Version.Status != "draft" || saved.Version.Source != "admin" || saved.Version.RenderMode != "raw" || saved.Published {
		t.Fatalf("%+v", saved)
	}
	// Saving the same snapshot again from the old base is the same save.
	again := must(SaveDeck(ctx, d, SaveDeckInput{Slug: "deck", PageID: page.ID, BaseVersion: first.Version.ID.String(), HTML: deck("two")}, admin))(t)
	if !again.Deduped || again.Version.ID != saved.Version.ID {
		t.Fatal("same snapshot")
	}
	_, err := SaveDeck(ctx, d, SaveDeckInput{Slug: "deck", PageID: page.ID, BaseVersion: first.Version.ID.String(), HTML: deck("three")}, admin)
	wantCode(t, err, 409, "stale_deck_version")
	_, err = SaveDeck(ctx, d, SaveDeckInput{Slug: "deck", PageID: page.ID + 1000, BaseVersion: saved.Version.ID.String(), HTML: deck("three")}, admin)
	wantCode(t, err, 403, "deck_session_unavailable")
	_, err = SaveDeck(ctx, d, SaveDeckInput{Slug: "deck", PageID: page.ID, BaseVersion: saved.Version.ID.String(), HTML: "<p>not a deck</p>"}, admin)
	wantCode(t, err, 403, "deck_session_unavailable")
	must(SaveDeck(ctx, d, SaveDeckInput{Slug: "deck", PageID: page.ID, BaseVersion: saved.Version.ID.String(), HTML: deck("three")}, admin))(t)
}

// test/password-reveal.integration.js state-machine parts.
func TestRevealPassword(t *testing.T) {
	d, ctx := setup(t)
	must(CreatePage(ctx, d, CreatePageInput{Slug: "rv-fabrikam", Title: "Fabrikam"}, agent))(t)
	must(SetPassword(ctx, d, "rv-fabrikam", "fabrikam-client-pass", agent))(t)
	shown := must(RevealPassword(ctx, d, "rv-fabrikam", admin))(t)
	b, _ := json.Marshal(shown)
	if string(b) != `{"slug":"rv-fabrikam","has_password":true,"available":true,"password":"fabrikam-client-pass"}` {
		t.Fatal(string(b))
	}
	var actor, actorType string
	var meta map[string]any
	noErr(t, d.QueryRow(ctx, `SELECT actor, actor_type, metadata FROM audit_log WHERE action = 'reveal_password' ORDER BY id DESC LIMIT 1`).Scan(&actor, &actorType, &meta))
	if actor != admin.Actor || actorType != "user" || meta["available"] != true {
		t.Fatal(actor, meta)
	}
	var leaked int
	noErr(t, d.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE metadata::text LIKE '%fabrikam-client-pass%'`).Scan(&leaked))
	if leaked != 0 {
		t.Fatal("the audit row never carries the password")
	}
	_, err := RevealPassword(ctx, d, "rv-fabrikam", agent)
	wantCode(t, err, 403, "admin_only")
	pb, _ := json.Marshal(getPage(t, d, "rv-fabrikam"))
	if strings.Contains(string(pb), "fabrikam-client-pass") || strings.Contains(string(pb), "ciphertext") {
		t.Fatal("page reads never include the password")
	}
	must(d.Exec(ctx, "UPDATE pages SET password_ciphertext = NULL WHERE slug = 'rv-fabrikam'"))(t)
	if r := must(RevealPassword(ctx, d, "rv-fabrikam", admin))(t); r.Available || r.Password != nil {
		t.Fatal("no copy yet is reported, not guessed")
	}
	must(SetPassword(ctx, d, "rv-fabrikam", nil, admin))(t)
	b, _ = json.Marshal(must(RevealPassword(ctx, d, "rv-fabrikam", admin))(t))
	if string(b) != `{"slug":"rv-fabrikam","has_password":false,"available":false,"password":null}` {
		t.Fatal(string(b))
	}
	var ct *string
	noErr(t, d.QueryRow(ctx, "SELECT password_ciphertext FROM pages WHERE slug = 'rv-fabrikam'").Scan(&ct))
	if ct != nil {
		t.Fatal("no copy outlives the password")
	}
	_, err = RevealPassword(ctx, d, "nope", admin)
	wantCode(t, err, 404, "page_not_found")
}

// Wire shapes the REST/MCP layers serialize.
func TestWireShapes(t *testing.T) {
	d, ctx := setup(t)
	p := must(CreatePage(ctx, d, CreatePageInput{Slug: "wire", Title: "Wire"}, agent))(t)
	b, _ := json.Marshal(p)
	want := fmt.Sprintf(`{"id":"%d","slug":"wire","title":"Wire","client_id":null,"workspace_id":null,"theme_id":null,"require_approval":false,"disabled":false,"published_version_id":null,"created_at":"%s","updated_at":"%s","workspace_name":null}`,
		p.ID, jsx.FormatTime(p.CreatedAt.Time), jsx.FormatTime(p.UpdatedAt.Time))
	if string(b) != want {
		t.Fatalf("\n%s\n%s", b, want)
	}
	r := deploy(t, d, agent, DeployInput{Slug: "wire", HTML: pageHTML(1), Publish: true})
	b, _ = json.Marshal(r)
	if !strings.HasPrefix(string(b), fmt.Sprintf(`{"version":{"id":"%d","page_id":"%d","content_sha256":"`, r.Version.ID, p.ID)) ||
		!strings.Contains(string(b), `"status":"approved","render_mode":"themed","author":"test-agent","source":"api","note":null,"reviewed_by":null,"reviewed_at":null,"created_at":"`) ||
		!strings.HasSuffix(string(b), `"deduped":false,"published":true,"gated":false,"live":true}`) {
		t.Fatal(string(b))
	}
	pv := getPage(t, d, "wire")
	b, _ = json.Marshal(pv)
	if !strings.Contains(string(b), `"theme_name":"flag"`) || !strings.Contains(string(b), `"published":{"id":"`) ||
		!strings.HasSuffix(string(b), `"lastCheck":{"last_check_at":null,"last_check_outcome":null,"last_check_detail":null,"last_check_source_as_of":null}}`) {
		t.Fatal(string(b))
	}
	lv := must(ListVersions(ctx, d, "wire", ListVersionsOptions{}))(t)
	b, _ = json.Marshal(lv[0])
	if !strings.HasSuffix(string(b), `"is_published":true,"is_live":true}`) {
		t.Fatal(string(b))
	}
}
