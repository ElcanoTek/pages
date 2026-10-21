// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package workspaces

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/versions"
)

var (
	agent = versions.Actor{Actor: "ws-agent", ActorType: "agent"}
	admin = versions.Actor{Actor: "admin@example.test", ActorType: "user"}
)

func wantCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	e, ok := apierror.As(err)
	if !ok || e.Status != status || e.Code != code {
		t.Fatalf("want %d %s, got %v", status, code, err)
	}
}

func TestNormalize(t *testing.T) {
	if n, _ := NormalizeName("  Client   Reporting  "); n != "Client Reporting" {
		t.Fatal(n)
	}
	_, err := NormalizeName("   ")
	wantCode(t, err, 400, "workspace_name_required")
	_, err = NormalizeName(strings.Repeat("x", 101))
	wantCode(t, err, 400, "workspace_name_too_long")
	_, err = NormalizeName("bad\nname")
	wantCode(t, err, 400, "bad_workspace_name")
	_, err = NormalizeName(nil)
	wantCode(t, err, 400, "workspace_name_required")
	if id, _ := NormalizeID("42", false); id != 42 {
		t.Fatal(id)
	}
	id, _ := NormalizeID("9007199254740992", false)
	if b, _ := json.Marshal(id); string(b) != `"9007199254740992"` {
		t.Fatal(string(b))
	}
	if id, err := NormalizeID(nil, true); id != 0 || err != nil {
		t.Fatal(id, err)
	}
	for _, bad := range []any{0.0, -1.0, 1.5, "nope", float64(1 << 53), "9223372036854775808"} {
		_, err := NormalizeID(bad, false)
		wantCode(t, err, 400, "bad_workspace_id")
		if e, _ := apierror.As(err); e.Message != "workspace_id must be a positive database id or null" {
			t.Fatal(e.Message)
		}
	}
}

func TestAuthority(t *testing.T) {
	if AssertOrganizer(agent) != nil || AssertOrganizer(admin) != nil {
		t.Fatal("users and agents organize")
	}
	wantCode(t, AssertOrganizer(versions.Actor{ActorType: "service"}), 403, "organizer_only")
	if AssertHumanAdmin(admin) != nil {
		t.Fatal("admin removes")
	}
	wantCode(t, AssertHumanAdmin(agent), 403, "admin_only")
}

func TestWorkspaceLifecycle(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	if ws, _ := List(ctx, d); len(ws) != 0 {
		t.Fatal("new installation starts with no workspaces")
	}
	alpha, err := Create(ctx, d, "Alpha", admin)
	if err != nil || *alpha.PageCount != 0 {
		t.Fatal(err)
	}
	_, err = Create(ctx, d, "aLPHa", agent)
	wantCode(t, err, 409, "workspace_exists")
	beta, _ := Create(ctx, d, "Beta", agent)

	if _, err := versions.CreatePage(ctx, d, versions.CreatePageInput{Slug: "adm"}, admin); err != nil {
		t.Fatal(err)
	}
	_, err = AssignPage(ctx, d, "adm", "999999", admin)
	wantCode(t, err, 404, "workspace_not_found")
	_, err = AssignPage(ctx, d, "missing", alpha.ID.String(), admin)
	wantCode(t, err, 404, "page_not_found")
	as, err := AssignPage(ctx, d, "adm", alpha.ID.String(), agent)
	if err != nil || *as.WorkspaceName != "Alpha" {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(as); string(b) != `{"slug":"adm","workspace_id":`+alpha.ID.String()+`,"workspace_name":"Alpha"}` {
		t.Fatal(string(b))
	}
	pv, _ := versions.GetPage(ctx, d, "adm")
	if pv.Page.WorkspaceID != alpha.ID || *pv.Page.WorkspaceName != "Alpha" {
		t.Fatal("membership persisted")
	}

	renamed, err := Rename(ctx, d, alpha.ID.String(), "Client Alpha", agent)
	if err != nil || renamed.Name != "Client Alpha" || renamed.PageCount != nil {
		t.Fatal(err)
	}
	_, err = Rename(ctx, d, alpha.ID.String(), "beta", agent)
	wantCode(t, err, 409, "workspace_exists")

	grouped, err := versions.CreatePage(ctx, d, versions.CreatePageInput{Slug: "agent-grouped", WorkspaceID: alpha.ID.String()}, agent)
	if err != nil || grouped.WorkspaceID != alpha.ID || *grouped.WorkspaceName != "Client Alpha" {
		t.Fatal(err)
	}
	_, err = versions.CreatePage(ctx, d, versions.CreatePageInput{Slug: "nowhere", WorkspaceID: "999999"}, agent)
	wantCode(t, err, 404, "workspace_not_found")
	_, err = Remove(ctx, d, alpha.ID, agent)
	wantCode(t, err, 403, "admin_only")

	if _, err := AssignPage(ctx, d, "adm", beta.ID.String(), admin); err != nil {
		t.Fatal(err)
	}
	if _, err := versions.CreatePage(ctx, d, versions.CreatePageInput{Slug: "beta-2", WorkspaceID: beta.ID}, admin); err != nil {
		t.Fatal(err)
	}
	ws, _ := List(ctx, d)
	counts := map[string]int{}
	for _, w := range ws {
		counts[w.Name] = *w.PageCount
	}
	if counts["Client Alpha"] != 1 || counts["Beta"] != 2 {
		t.Fatal(counts)
	}
	removed, err := Remove(ctx, d, beta.ID.String(), admin)
	if err != nil || removed.UngroupedPageCount != 2 || !removed.Deleted {
		t.Fatal(err, removed)
	}
	_, err = Remove(ctx, d, beta.ID.String(), admin)
	wantCode(t, err, 404, "workspace_not_found")
	pv, _ = versions.GetPage(ctx, d, "adm")
	if pv.Page.WorkspaceID != 0 {
		t.Fatal("member moves to Ungrouped")
	}
	ungrouped, err := AssignPage(ctx, d, "agent-grouped", nil, agent)
	if err != nil || ungrouped.WorkspaceID != 0 || ungrouped.WorkspaceName != nil {
		t.Fatal(err)
	}
	var n int
	_ = d.QueryRow(ctx, `SELECT count(DISTINCT action) FROM audit_log WHERE action IN ('create_workspace','rename_workspace','delete_workspace','set_workspace')`).Scan(&n)
	if n != 4 {
		t.Fatal("every workspace mutation is audited", n)
	}
	var reason string
	_ = d.QueryRow(ctx, `SELECT metadata->>'reason' FROM audit_log WHERE action = 'set_workspace' AND metadata->>'reason' IS NOT NULL LIMIT 1`).Scan(&reason)
	if reason != "workspace_removed" {
		t.Fatal(reason)
	}
}
