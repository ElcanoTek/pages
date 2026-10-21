// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/routens"
)

func wantCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	e, ok := apierror.As(err)
	if !ok {
		t.Fatalf("want apierror %d %s, got %v", status, code, err)
	}
	if e.Status != status || e.Code != code {
		t.Fatalf("want %d %s, got %d %s (%s)", status, code, e.Status, e.Code, e.Message)
	}
}

func TestSHA256(t *testing.T) {
	if SHA256("hello") != SHA256("hello") || SHA256("hello") == SHA256("hello!") {
		t.Fatal("sha256 must be stable and content-sensitive")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(SHA256("x")) {
		t.Fatal("hex digest")
	}
}

func TestNormalizeSlug(t *testing.T) {
	for in, want := range map[string]string{"Northwind": "northwind", "northwind/q2-report": "northwind/q2-report", " acme_co ": "acme_co"} {
		got, err := NormalizeSlug(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeSlug(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []any{"", "/leading", "trailing/", "has space", "bad//seg", "../etc", "İstanbul"} {
		_, err := NormalizeSlug(bad)
		wantCode(t, err, 400, "bad_slug")
	}
	_, err := NormalizeSlug(nil)
	wantCode(t, err, 400, "slug_required")
	_, err = NormalizeSlug(5.0)
	wantCode(t, err, 400, "slug_required")
}

func TestReservedSlugs(t *testing.T) {
	for _, segment := range []string{"raw-template", "preflight", "edit-token", "readyz"} {
		for _, slug := range []string{segment, segment + "/report", "northwind/" + segment + "/report"} {
			wantCode(t, routens.AssertSlugNotReserved(slug), 400, "reserved_slug")
		}
	}
	for _, ok := range []string{"northwind", "northwind/q2", "rawhide", "my-assets/q2", "welcome-page", "versionsx", "my-portal", "portal-nwm", "portals-x", "northwind/quarterly-report"} {
		if err := routens.AssertSlugNotReserved(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
}

func TestNormalizeWorkspaceID(t *testing.T) {
	id, err := NormalizeWorkspaceID("9007199254740992")
	if err != nil || id != 9007199254740992 {
		t.Fatal(id, err)
	}
	b, _ := json.Marshal(id)
	if string(b) != `"9007199254740992"` {
		t.Fatalf("unsafe ids stay strings: %s", b)
	}
	b, _ = json.Marshal(WorkspaceRef(42))
	if string(b) != "42" {
		t.Fatalf("safe ids are numbers: %s", b)
	}
	for _, bad := range []any{0.0, -1.0, 1.5, "nope", float64(maxSafeInteger + 1), "9223372036854775808", nil} {
		_, err := NormalizeWorkspaceID(bad)
		wantCode(t, err, 400, "bad_workspace_id")
	}
}

func ts(s string) time.Time { return ParseTime(s) }

func TestFreshnessReportsAgeWithoutVerdict(t *testing.T) {
	now := ts("2026-08-17T12:00:00.000Z")
	f := FreshnessOf(FreshnessInput{SourceAsOf: ts("2026-08-13T23:59:59.000Z"), RefreshedAt: ts("2026-08-14T16:42:41.000Z")}, now)
	if jsx.FormatTime(f.SourceAsOf.Time) != "2026-08-13T23:59:59.000Z" || *f.DaysSinceSource != 3 || *f.DaysSinceRefresh != 2 {
		t.Fatalf("%+v", f)
	}
	if jsx.FormatTime(f.CheckedAt.Time) != "2026-08-14T16:42:41.000Z" || *f.DaysSinceCheck != 2 || f.LastCheckOutcome != nil {
		t.Fatalf("with no recorded check, last looked is the last refresh: %+v", f)
	}
	b, _ := json.Marshal(f)
	if strings.Contains(string(b), "overdue") || strings.Contains(string(b), "is_stale") {
		t.Fatal("no verdict field")
	}
	want := `{"last_check_at":null,"latest_outcome":"updated","latest_detail":null,"latest_source_as_of":"2026-08-13T23:59:59.000Z",` +
		`"source_as_of":"2026-08-13T23:59:59.000Z","refreshed_at":"2026-08-14T16:42:41.000Z","checked_at":"2026-08-14T16:42:41.000Z",` +
		`"last_check_outcome":null,"last_check_detail":null,"last_check_source_as_of":null,"days_since_source":3,"days_since_refresh":2,"days_since_check":2}`
	if string(b) != want {
		t.Fatalf("wire shape:\n%s\n%s", b, want)
	}
	if FreshnessOf(FreshnessInput{}, now) != nil {
		t.Fatal("nothing to be stale about ⇒ nil")
	}
}

func TestFreshnessRecordedCheck(t *testing.T) {
	now := ts("2026-08-17T12:00:00.000Z")
	dead := FreshnessOf(FreshnessInput{
		SourceAsOf: ts("2026-07-02T00:00:00.000Z"), RefreshedAt: ts("2026-07-21T00:00:00.000Z"),
		LastCheckAt: ts("2026-08-17T06:00:00.000Z"), LastCheckOutcome: "source_not_updated",
		LastCheckDetail: "upstream max date still 2026-07-02", LastCheckSourceAsOf: ts("2026-07-02T00:00:00.000Z"),
	}, now)
	if *dead.DaysSinceSource != 46 || *dead.DaysSinceRefresh != 27 || *dead.DaysSinceCheck != 0 || *dead.LastCheckOutcome != "source_not_updated" {
		t.Fatalf("%+v", dead)
	}
	if *dead.LatestOutcome != "source_not_updated" || *dead.LatestDetail != "upstream max date still 2026-07-02" {
		t.Fatal("the explicit check is latest")
	}
	abandoned := FreshnessOf(FreshnessInput{SourceAsOf: ts("2026-07-02T00:00:00.000Z"), RefreshedAt: ts("2026-07-21T00:00:00.000Z")}, now)
	if *abandoned.DaysSinceCheck != 27 {
		t.Fatal("nobody has looked since the last write")
	}
	since := FreshnessOf(FreshnessInput{RefreshedAt: ts("2026-08-16T00:00:00.000Z"), LastCheckAt: ts("2026-08-10T00:00:00.000Z")}, now)
	if jsx.FormatTime(since.CheckedAt.Time) != "2026-08-16T00:00:00.000Z" {
		t.Fatal("an older check must not pull checked_at backwards")
	}
}

func TestFreshnessSkewAndJunk(t *testing.T) {
	now := ts("2026-08-17T12:00:00.000Z")
	if d := *FreshnessOf(FreshnessInput{SourceAsOf: ts("2026-08-17T12:00:30.000Z")}, now).DaysSinceSource; d != 0 {
		t.Fatal("clamped skew", d)
	}
	junk := FreshnessOf(FreshnessInput{SourceAsOf: ts("not-a-date"), RefreshedAt: ts("2026-08-16T00:00:00.000Z")}, now)
	if !junk.SourceAsOf.IsZero() || junk.DaysSinceSource != nil || *junk.DaysSinceRefresh != 1 {
		t.Fatalf("%+v", junk)
	}
}

func TestListPagesQuery(t *testing.T) {
	limit := 101
	q := "Client 100%_\\"
	f, tr := false, true
	sql, params, err := buildListPagesQuery(ListPagesOptions{
		Limit: &limit, After: &After{CreatedAt: "2026-07-16T12:34:56.789Z", ID: "42"},
		WorkspaceFilterSet: true, WorkspaceID: Null, Query: &q, ClientIDSet: true,
		IsLive: &f, RequireApproval: &tr, Disabled: &f,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{`p\.workspace_id IS NULL`, `p\.client_id IS NULL`, `LEFT JOIN page_versions pv ON pv\.id = p\.published_version_id`,
		`pv\.source_as_of, pv\.refreshed_at`, `LEFT JOIN themes`, `COALESCE\(t\.name, 'flag'\) AS theme_name`,
		`(?s)date_trunc\('milliseconds', p\.created_at\).*p\.id`, `LIMIT \$\d+$`} {
		if !regexp.MustCompile(re).MatchString(sql) {
			t.Fatalf("missing %s in %s", re, sql)
		}
	}
	if strings.Contains(sql, "published_version_number") {
		t.Fatal("public/MCP reads do not gain admin-only fields")
	}
	found := false
	for _, p := range params {
		found = found || p == `%Client 100\%\_\\%`
	}
	if !found || params[len(params)-1] != 101 {
		t.Fatalf("params %v", params)
	}
	over := 102
	_, _, err = buildListPagesQuery(ListPagesOptions{Limit: &over})
	wantCode(t, err, 400, "bad_limit")
	_, _, err = buildListPagesQuery(ListPagesOptions{After: &After{CreatedAt: "not-a-date", ID: 1}})
	wantCode(t, err, 400, "bad_cursor")
	_, _, err = buildListPagesQuery(ListPagesOptions{WorkspaceFilterSet: true})
	wantCode(t, err, 400, "bad_workspace_id")
	sql, _, _ = buildListPagesQuery(ListPagesOptions{IncludeVersionNumbers: true})
	for _, s := range []string{"AS version_count", "AS published_version_number", "versions.id <= p.published_version_id"} {
		if !strings.Contains(sql, s) {
			t.Fatal(s)
		}
	}
}

func TestPageSummaryVersionNumbersOptIn(t *testing.T) {
	b, _ := json.Marshal(PageSummary{Page: Page{ID: 8, Slug: "client/q2"}})
	if strings.Contains(string(b), "version_count") || !strings.HasSuffix(string(b), `"has_password":false,"is_live":false,"freshness":null}`) {
		t.Fatal(string(b))
	}
	b, _ = json.Marshal(PageSummary{Page: Page{ID: 8}, VersionNumbers: &VersionNumbers{VersionCount: 2}})
	if !strings.Contains(string(b), `"is_live":false,"version_count":2,"published_version_number":null,"freshness":null`) {
		t.Fatal(string(b))
	}
}

// The state layer bounds inputs before any database work (nil *db.DB).
func TestStateLayerBoundsBeforePersistence(t *testing.T) {
	ctx := context.Background()
	agent := Actor{Actor: "unit-agent", ActorType: "agent"}
	admin := Actor{Actor: "unit-admin", ActorType: "user"}
	_, err := Deploy(ctx, nil, DeployInput{Slug: "bounded", HTML: "<p>x</p>", Note: strings.Repeat("n", 501)}, agent)
	wantCode(t, err, 400, "note_too_long")
	_, err = Reject(ctx, nil, ReviewInput{Slug: "bounded", VersionID: 1, Note: strings.Repeat("n", 501)}, admin)
	wantCode(t, err, 400, "note_too_long")
	_, err = SetPassword(ctx, nil, "bounded", strings.Repeat("p", 513), agent)
	wantCode(t, err, 400, "password_too_long")
	_, err = SetPassword(ctx, nil, "bounded", "", agent)
	wantCode(t, err, 403, "admin_only")
	_, err = SetPassword(ctx, nil, "bounded", 5.0, agent)
	wantCode(t, err, 400, "bad_password")
	_, err = RevealPassword(ctx, nil, "bounded", agent)
	wantCode(t, err, 403, "admin_only")
	_, err = Approve(ctx, nil, ReviewInput{Slug: "bounded", VersionID: 1}, agent)
	wantCode(t, err, 403, "admin_only")
	_, err = Publish(ctx, nil, PublishInput{Slug: "bounded"}, agent)
	wantCode(t, err, 400, "version_id_required")
	_, err = SetTitle(ctx, nil, "bounded", "   ", agent)
	wantCode(t, err, 400, "title_required")
	_, err = SetDisabled(ctx, nil, "bounded", "true", admin)
	wantCode(t, err, 400, "bad_boolean")
	_, err = SaveDeck(ctx, nil, SaveDeckInput{Slug: "bounded", BaseVersion: "1"}, agent)
	wantCode(t, err, 403, "admin_only")
	_, err = SaveDeck(ctx, nil, SaveDeckInput{Slug: "bounded", BaseVersion: "9223372036854775808"}, admin)
	wantCode(t, err, 400, "deck_base_version_required")
	_, err = UpdatePageData(ctx, nil, DataUpdateInput{Slug: "bounded"}, agent)
	wantCode(t, err, 400, "expected_version_required")
	_, err = RecordRefreshCheck(ctx, nil, RefreshCheckInput{Slug: "bounded", Outcome: "probably_fine"}, agent)
	wantCode(t, err, 400, "refresh_check_outcome_invalid")
	_, err = RecordRefreshCheck(ctx, nil, RefreshCheckInput{Slug: "bounded", Outcome: "failed", Detail: strings.Repeat("d", 501)}, agent)
	wantCode(t, err, 400, "refresh_check_detail_invalid")
	_, err = RecordRefreshCheck(ctx, nil, RefreshCheckInput{Slug: "bounded", Outcome: "failed", SourceAsOfSeen: "yesterday"}, agent)
	wantCode(t, err, 400, "refresh_check_source_invalid")
	_, err = CreatePage(ctx, nil, CreatePageInput{Slug: "x", RequireApproval: Null}, agent)
	wantCode(t, err, 400, "bad_boolean")
	_, err = CreatePage(ctx, nil, CreatePageInput{Slug: "x", WorkspaceID: "1"}, Actor{ActorType: "service"})
	wantCode(t, err, 403, "organizer_only")
}

func TestPrepareDeploy(t *testing.T) {
	agent := Actor{Actor: "unit-agent", ActorType: "agent"}
	for _, bad := range []any{"false", "true", 0.0, 1.0, Null, map[string]any{}, []any{}} {
		_, err := PrepareDeploy(DeployInput{Slug: "x", HTML: "<p>x</p>", Publish: bad}, agent)
		wantCode(t, err, 400, "bad_boolean")
	}
	_, err := PrepareDeploy(DeployInput{Slug: "x"}, agent)
	wantCode(t, err, 400, "html_required")
	_, err = PrepareDeploy(DeployInput{Slug: "x", HTML: "<p>x</p>", RenderMode: 5.0}, agent)
	wantCode(t, err, 400, "bad_render_mode")
	if e, _ := apierror.As(err); e.Message != "bad render_mode: 5" {
		t.Fatal(e.Message)
	}
	_, err = PrepareDeploy(DeployInput{Slug: "x", HTML: "<p>x</p>", Source: "ftp"}, agent)
	wantCode(t, err, 400, "bad_source")
	_, err = PrepareDeploy(DeployInput{Slug: "x", HTML: "<p>x</p>", Note: 3.0}, agent)
	wantCode(t, err, 400, "bad_note")

	deck := `<!doctype html><html><body><script type="application/bento+json" id="bento-doc">{}</script></body></html>`
	p, err := PrepareDeploy(DeployInput{Slug: "Deck", HTML: deck}, agent)
	if err != nil || p.RenderMode != "raw" || p.Slug != "deck" || p.VersionAuthor != "unit-agent" || p.Source != "api" {
		t.Fatalf("%+v %v", p, err)
	}
	_, err = PrepareDeploy(DeployInput{Slug: "deck", HTML: deck, RenderMode: "themed"}, agent)
	wantCode(t, err, 400, "bad_render_mode")

	p, _ = PrepareDeploy(DeployInput{Slug: "x", HTML: "<p>x</p>", Author: "Contoso bot"}, agent)
	if p.VersionAuthor != "Contoso bot" || p.RenderMode != "themed" || p.ContentSHA != SHA256("<p>x</p>") || p.DataMetadata != nil {
		t.Fatalf("%+v", p)
	}
	// Broken managed blocks are refused at write time with their own code.
	broken := `<script id="pages-data-schema" type="application/schema+json">{}</script><script id="pages-data" type="application/json">{nope</script>`
	_, err = PrepareDeploy(DeployInput{Slug: "x", HTML: broken}, agent)
	e, ok := apierror.As(err)
	if !ok || e.Status != 409 || !strings.HasPrefix(e.Message, "refusing to publish: this document's Pages-managed blocks are not valid") {
		t.Fatalf("%v", err)
	}
}

func TestAssertExpectedVersion(t *testing.T) {
	page := &LockedPage{PublishedVersionID: 145}
	for _, ok := range []any{nil, Null, "145", 145.0, jsx.ID(145), 145} {
		if err := AssertExpectedVersion(page, ok); err != nil {
			t.Fatalf("%v: %v", ok, err)
		}
	}
	err := AssertExpectedVersion(page, 143.0)
	wantCode(t, err, 409, "stale_version")
	e, _ := apierror.As(err)
	want := "published version changed (expected 143, is 145) — another write landed after you read version 143. Reread the page, confirm version 145 is not already the change you intended, and only then retry once with expected_version 145."
	if e.Message != want {
		t.Fatal(e.Message)
	}
	b, _ := json.Marshal(e.Details)
	if string(b) != `{"expected_version":"143","published_version_id":"145","retry_with_expected_version":"145"}` {
		t.Fatal(string(b))
	}
	err = AssertExpectedVersion(&LockedPage{}, "7")
	e, _ = apierror.As(err)
	b, _ = json.Marshal(e.Details)
	if !strings.Contains(e.Message, "is null)") || string(b) != `{"expected_version":"7","published_version_id":null,"retry_with_expected_version":null}` {
		t.Fatal(e.Message, string(b))
	}
}

func TestParseTime(t *testing.T) {
	for in, want := range map[string]string{
		"2026-08-01T00:00:00Z":          "2026-08-01T00:00:00.000Z",
		"2026-08-01T02:00:00.1234+02:00": "2026-08-01T00:00:00.123Z",
		"2026-08-01":                    "2026-08-01T00:00:00.000Z",
	} {
		if got := isoString(ParseTime(in)); got != want {
			t.Fatalf("%s: %s", in, got)
		}
	}
	for _, bad := range []string{"", "not-a-date", "2026-13-01", "2026-08-01T24:00:01Z"} {
		if !ParseTime(bad).IsZero() {
			t.Fatal(bad)
		}
	}
}
