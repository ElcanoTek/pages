// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package templates

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/versions"
)

// Ported from test/template-{create,version,metadata,migration}.integration.js.

var (
	agent = versions.Actor{Actor: "template-create-test", ActorType: "agent"}
	staff = versions.Actor{Actor: "migration-reviewer", ActorType: "user"}
)

func setup(t *testing.T) (*db.DB, context.Context) {
	t.Helper()
	t.Setenv("PAGE_COOKIE_SECRET", "templates-test-secret-0123456789abcdef")
	t.Setenv("PG_LOCK_TIMEOUT_MS", "5000")
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

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if errCode(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func obj(t *testing.T, js string) *jsonv.Object {
	t.Helper()
	v, err := jsonv.ParseString(js)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*jsonv.Object)
}

func schema(props string, required ...string) string {
	return `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["` +
		strings.Join(required, `","`) + `"],"properties":` + props + `}`
}

func block(id, value, typ string) string {
	if typ == "" {
		typ = "application/json"
	}
	return `<script id="` + id + `" type="` + typ + `">` + value + `</script>`
}

var (
	campaignConfigSchema = block("pages-config-schema", schema(`{"campaign":{"type":"string"}}`, "campaign"), "application/schema+json")
	campaignConfig       = block("pages-config", `{"campaign":"Northwind Spring"}`, "")
	countDataSchema      = block("pages-data-schema", schema(`{"count":{"type":"number"}}`, "count"), "application/schema+json")
	fixtureHTML          = "<!doctype html><html><head><title>Northwind</title></head><body><h1>Campaign</h1>" +
		campaignConfigSchema + campaignConfig + countDataSchema +
		block("pages-data", `{"contract_version":1,"refreshed_at":"2026-08-01T00:00:00.000Z","source_as_of":"2026-08-01T00:00:00.000Z","data":{"count":0}}`, "") +
		"</body></html>"
)

func register(t *testing.T, ctx context.Context, d *db.DB, name, html string) *RegisterResult {
	t.Helper()
	return must(Register(ctx, d, RegisterInput{Name: name, HTML: html}, agent))(t)
}

type pageState struct {
	ID        jsx.ID
	Published jsx.ID
	Versions  int
	Audits    int
}

func stateOf(t *testing.T, d *db.DB, slug string) pageState {
	t.Helper()
	var s pageState
	err := d.QueryRow(context.Background(),
		`SELECT p.id, p.published_version_id,
		        (SELECT count(*)::integer FROM page_versions WHERE page_id = p.id),
		        (SELECT count(*)::integer FROM audit_log WHERE page_id = p.id)
		   FROM pages p WHERE p.slug = $1 AND p.deleted_at IS NULL`, slug).Scan(&s.ID, &s.Published, &s.Versions, &s.Audits)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ── statement hooks (the JavaScript tests wrapped db.withTransaction) ──────

type hookedTx struct {
	pgx.Tx
	queryRow func(ctx context.Context, sql string, args []any) (pgx.Row, bool)
	exec     func(sql string) error
}

func (h *hookedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if h.queryRow != nil {
		if row, ok := h.queryRow(ctx, sql, args); ok {
			return row
		}
	}
	return h.Tx.QueryRow(ctx, sql, args...)
}

func (h *hookedTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if h.exec != nil {
		if err := h.exec(sql); err != nil {
			return pgconn.CommandTag{}, err
		}
	}
	return h.Tx.Exec(ctx, sql, args...)
}

func hook(t *testing.T, wrap func(tx pgx.Tx) db.Querier) {
	t.Helper()
	saved := wrapTx
	wrapTx = wrap
	t.Cleanup(func() { wrapTx = saved })
}

type noRow struct{}

func (noRow) Scan(...any) error { return pgx.ErrNoRows }

var pageLockSQL = regexp.MustCompile(`FROM pages WHERE slug = \$1 AND deleted_at IS NULL FOR UPDATE`)

// raceMissingSlug drives the actual transaction/unique-index race behind
// template creation: both transactions must read an absent slug before either
// may insert it. The barrier delays real query results; it never invents
// database state.
func raceMissingSlug(t *testing.T, slug string, calls ...func() error) []error {
	t.Helper()
	var mu sync.Mutex
	arrivals := 0
	ready := make(chan struct{})
	hook(t, func(tx pgx.Tx) db.Querier {
		arrived := false
		return &hookedTx{Tx: tx, queryRow: func(ctx context.Context, sql string, args []any) (pgx.Row, bool) {
			if arrived || len(args) == 0 || args[0] != slug || !pageLockSQL.MatchString(sql) {
				return nil, false
			}
			arrived = true
			var id jsx.ID
			err := tx.QueryRow(ctx, `SELECT id FROM pages WHERE slug = $1 AND deleted_at IS NULL FOR UPDATE`, slug).Scan(&id)
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("the real database must report an absent slug: %v", err)
				return nil, false
			}
			mu.Lock()
			arrivals++
			if arrivals == 2 {
				close(ready)
			}
			mu.Unlock()
			select {
			case <-ready:
			case <-time.After(4 * time.Second):
				t.Error("both creates did not reach the missing-slug barrier")
			}
			return noRow{}, true
		}}
	})
	errs := make([]error, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = call() }()
	}
	wg.Wait()
	if arrivals != 2 {
		t.Fatalf("two independent transactions must reach the barrier, got %d", arrivals)
	}
	return errs
}

func TestCreatePageRaces(t *testing.T) {
	d, ctx := setup(t)
	register(t, ctx, d, "create-race-template", fixtureHTML)
	config := obj(t, `{"campaign":"Northwind Spring"}`)

	for _, mode := range []string{"published", "draft", "gated"} {
		in := CreatePageInput{Template: "create-race-template", Slug: "template-create-conflict-" + mode, Config: config,
			Publish: mode != "draft", RequireApproval: mode == "gated"}
		results := make([]*CreatePageResult, 2)
		other := in
		other.Config = obj(t, `{"campaign":"Contoso Summer"}`)
		errs := raceMissingSlug(t, in.Slug.(string),
			func() (err error) { results[0], err = CreatePage(ctx, d, in, agent); return },
			func() (err error) { results[1], err = CreatePage(ctx, d, other, agent); return })
		var winner *CreatePageResult
		failures := 0
		for i, err := range errs {
			if err == nil {
				winner = results[i]
				continue
			}
			failures++
			wantCode(t, err, "page_exists")
		}
		if winner == nil || failures != 1 || !winner.Created {
			t.Fatalf("%s: only one different build may succeed: %v", mode, errs)
		}
		st := stateOf(t, d, in.Slug.(string))
		wantAudits := 2
		var wantPublished jsx.ID
		if mode == "published" {
			wantAudits, wantPublished = 3, winner.Version.ID
		}
		if st.Versions != 1 || st.Audits != wantAudits || st.Published != wantPublished {
			t.Fatalf("%s: %+v", mode, st)
		}

		retry := in
		retry.Slug = "template-create-retry-" + mode
		a, b := retry, retry
		a.Now = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		b.Now = time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
		pair := make([]*CreatePageResult, 2)
		errs = raceMissingSlug(t, retry.Slug.(string),
			func() (err error) { pair[0], err = CreatePage(ctx, d, a, agent); return },
			func() (err error) { pair[1], err = CreatePage(ctx, d, b, agent); return })
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("%s: identical retries must both succeed: %v", mode, errs)
		}
		if pair[0].Version.ID != pair[1].Version.ID {
			t.Fatal("timestamps do not turn an exact retry into a new build")
		}
		if (pair[0].Created == pair[1].Created) || (pair[0].Deduped == pair[1].Deduped) {
			t.Fatalf("one create and one dedupe: %+v %+v", pair[0].DeployResult, pair[1].DeployResult)
		}
		rs := stateOf(t, d, retry.Slug.(string))
		if mode == "published" {
			wantPublished = pair[0].Version.ID
		}
		if rs.Versions != 1 || rs.Audits != wantAudits || rs.Published != wantPublished {
			t.Fatalf("%s retry: %+v", mode, rs)
		}
		if mode == "published" {
			hook(t, func(tx pgx.Tx) db.Querier { return tx })
			refreshed := must(versions.UpdatePageData(ctx, d, versions.DataUpdateInput{Slug: retry.Slug.(string), Data: obj(t, `{"count":42}`),
				SourceAsOf: "2026-08-03T00:00:00Z", ExpectedVersion: pair[0].Version.ID}, agent))(t)
			before := stateOf(t, d, retry.Slug.(string))
			_, err := CreatePage(ctx, d, retry, agent)
			wantCode(t, err, "page_exists")
			if stateOf(t, d, retry.Slug.(string)) != before || before.Published != refreshed.Version.ID {
				t.Fatal("a historical retry cannot undo a refresh or write an audit row")
			}
		}
	}

	hook(t, func(tx pgx.Tx) db.Querier { return tx })
	// The already-existing path enforces the same identity, including fields
	// that do not necessarily change the materialized config/data hashes.
	identity := CreatePageInput{Template: "create-race-template", Slug: "template-create-identity", Config: config, RenderMode: "raw",
		Data: obj(t, `{"count":10}`), SourceAsOf: "2026-08-01T00:00:00Z"}
	must(CreatePage(ctx, d, identity, agent))(t)
	before := stateOf(t, d, "template-create-identity")
	register(t, ctx, d, "create-race-other-template", fixtureHTML)
	for i, diff := range []func(*CreatePageInput){
		func(c *CreatePageInput) { c.Config = obj(t, `{"campaign":"Contoso Summer"}`) },
		func(c *CreatePageInput) { c.Data = obj(t, `{"count":11}`) },
		func(c *CreatePageInput) { c.SourceAsOf = "2026-08-02T00:00:00Z" },
		func(c *CreatePageInput) { c.RenderMode = "themed" },
		func(c *CreatePageInput) { c.Template = "create-race-other-template" },
	} {
		c := identity
		diff(&c)
		_, err := CreatePage(ctx, d, c, agent)
		if errCode(err) != "page_exists" || stateOf(t, d, "template-create-identity") != before {
			t.Fatalf("difference %d: %v", i, err)
		}
	}
	// Preview-only sample data changes the template revision while leaving the
	// materialized page bytes unchanged. It is still a different build.
	register(t, ctx, d, "create-race-template", strings.Replace(fixtureHTML, "</body>", block("pages-data-example", `{"count":7}`, "")+"</body>", 1))
	_, err := CreatePage(ctx, d, identity, agent)
	wantCode(t, err, "page_exists")
	pinned := identity
	pinned.Revision = intPtr(1)
	if r := must(CreatePage(ctx, d, pinned, agent))(t); !r.Deduped {
		t.Fatal("an explicit retry of the original revision remains safe")
	}
	if stateOf(t, d, "template-create-identity") != before {
		t.Fatal("no mutation")
	}

	// Ordinary concurrent deploys retain their create-or-update contract.
	upsert := func(n string) func() error {
		return func() error {
			_, err := inTx(ctx, d, func(q db.Querier) (*versions.CreateDeployResult, error) {
				return versions.CreateAndDeployWithTx(ctx, q, versions.CreateAndDeployInput{DeployInput: versions.DeployInput{
					Slug: "template-create-ordinary-upsert", HTML: "<!doctype html><html><body>Northwind " + n + "</body></html>", Publish: true,
				}}, agent, versions.CreateAndDeployOptions{})
			})
			return err
		}
	}
	errs := raceMissingSlug(t, "template-create-ordinary-upsert", upsert("1"), upsert("2"))
	if errs[0] != nil || errs[1] != nil {
		t.Fatal(errs)
	}
	if st := stateOf(t, d, "template-create-ordinary-upsert"); st.Versions != 2 {
		t.Fatalf("ordinary deploy_page remains create-or-update: %+v", st)
	}
}
