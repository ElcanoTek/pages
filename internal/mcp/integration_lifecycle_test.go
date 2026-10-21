// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Port of test/mcp.integration.js sections 10–18: pagination, workspaces,
// dedupe and optimistic concurrency, the approval gate, page settings,
// takedown, and create-or-deploy races.

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/versions"
	"github.com/ElcanoTek/pages/internal/workspaces"
)

func TestListingWorkspacesAndLifecycle(t *testing.T) {
	h := newHarness(t)
	v1 := strAt(h.data(h.call("deploy_page", o("slug", "mcpdemo", "title", "MCP Demo", "html", pageHTML(1), "publish", true))), "version", "id")
	h.data(h.call("deploy_page", o("slug", "mcp-other", "title", "Other", "html", pageHTML(5), "publish", true)))

	// 10. Pagination is bounded and cursor-based.
	first := h.data(h.call("list_pages", o("limit", num(1))))
	eq(t, len(toAnySlice(first["pages"])), 1)
	if strAt(first, "next_cursor") == "" {
		t.Fatal("look-ahead emits next_cursor")
	}
	second := h.data(h.call("list_pages", o("limit", num(1), "cursor", first["next_cursor"])))
	eq(t, len(toAnySlice(second["pages"])), 1)
	if strAt(second, "pages", 0, "id") == strAt(first, "pages", 0, "id") {
		t.Fatal("the next page does not repeat the boundary row")
	}
	eq(t, h.fails(h.call("list_pages", o("workspace_id", nil, "limit", num(1), "cursor", first["next_cursor"]))).code(), "invalid_cursor", "cursor is bound to its filters")

	// 11. Reversible workspace organization, including explicit Ungrouped.
	alpha := h.data(h.call("create_workspace", o("name", "MCP Alpha")))["workspace"].(map[string]any)
	beta := h.data(h.call("create_workspace", o("name", "MCP Beta")))["workspace"].(map[string]any)
	if _, ok := alpha["id"].(string); !ok {
		t.Fatal("workspace ids are strings")
	}
	wp1 := h.data(h.call("list_workspaces", o("limit", num(1))))
	eq(t, len(toAnySlice(wp1["workspaces"])), 1)
	if strAt(wp1, "next_cursor") == "" {
		t.Fatal("next_cursor")
	}
	wp2 := h.data(h.call("list_workspaces", o("limit", num(1), "cursor", wp1["next_cursor"])))
	eq(t, len(toAnySlice(wp2["workspaces"])), 1)
	if strAt(wp2, "workspaces", 0, "id") == strAt(wp1, "workspaces", 0, "id") {
		t.Fatal("workspace pages do not repeat")
	}
	renamed := h.data(h.call("rename_workspace", o("workspace_id", alpha["id"], "name", "MCP Client Alpha")))["workspace"]
	eq(t, strAt(renamed, "name"), "MCP Client Alpha")
	assigned := h.data(h.call("set_page_workspace", o("slug", "mcpdemo", "workspace_id", alpha["id"])))["page"]
	eq(t, dig(assigned, "workspace_id"), alpha["id"])
	if _, ok := dig(assigned, "workspace_id").(string); !ok {
		t.Fatal("an assigned workspace id is a string")
	}
	eq(t, dig(assigned, "workspace_name"), "MCP Client Alpha")
	var grouped []string
	for _, p := range toAnySlice(h.data(h.call("list_pages", o("workspace_id", alpha["id"])))["pages"]) {
		grouped = append(grouped, strAt(p, "slug"))
	}
	eq(t, grouped, []string{"mcpdemo"})
	var found []string
	for _, w := range toAnySlice(h.data(h.call("list_workspaces", o("query", "client alpha")))["workspaces"]) {
		found = append(found, strAt(w, "id"))
	}
	eq(t, found, []string{alpha["id"].(string)})
	ungroupedMove := h.data(h.call("set_page_workspace", o("slug", "mcpdemo", "workspace_id", nil)))["page"]
	eq(t, dig(ungroupedMove, "workspace_id"), nil)
	eq(t, dig(ungroupedMove, "workspace_name"), nil)
	if !slices.ContainsFunc(toAnySlice(h.data(h.call("list_pages", o("workspace_id", nil)))["pages"]), func(p any) bool { return strAt(p, "slug") == "mcpdemo" }) {
		t.Fatal("Ungrouped is selectable")
	}
	// Deletion stays human-only: the domain allows it, MCP does not offer it.
	human := versions.Actor{Actor: "mcp-test-admin", ActorType: audit.User, IP: "127.0.0.1"}
	for _, id := range []any{alpha["id"], beta["id"]} {
		if _, err := workspaces.Remove(context.Background(), h.d, id, human); err != nil {
			t.Fatal(err)
		}
	}

	// 12. Identical content dedupes; a current expected_version succeeds; a
	// stale one is a structured error that leaves the pointer.
	duplicate := h.data(h.call("deploy_page", o("slug", "mcpdemo", "html", pageHTML(1), "publish", false)))
	eq(t, duplicate["deduped"], true)
	eq(t, dig(duplicate, "version", "id"), v1)
	eq(t, duplicate["version_is_live"], true)
	updated := h.data(h.call("update_page", o("slug", "mcpdemo", "html", pageHTML(2), "publish", true, "expected_version", v1)))
	v2 := strAt(updated, "version", "id")
	if v2 == v1 {
		t.Fatal("a new version")
	}
	eq(t, updated["version_is_live"], true)
	eq(t, h.fails(h.call("update_page", o("slug", "mcpdemo", "html", pageHTML(3), "publish", true, "expected_version", v1))).code(), "stale_version")
	eq(t, dig(h.data(h.call("get_page", o("slug", "mcpdemo"))), "page", "published_version_id"), v2)
	history1 := h.data(h.call("list_versions", o("slug", "mcpdemo", "limit", num(1))))
	eq(t, len(toAnySlice(history1["versions"])), 1)
	history2 := h.data(h.call("list_versions", o("slug", "mcpdemo", "limit", num(1), "cursor", history1["next_cursor"])))
	if strAt(history2, "versions", 0, "id") == strAt(history1, "versions", 0, "id") {
		t.Fatal("version pages do not repeat")
	}
	mustMatch(t, `v2`, strAt(h.data(h.call("get_version", o("slug", "mcpdemo", "version_id", v2))), "version", "html"))
	eq(t, h.data(h.call("rollback_page", o("slug", "mcpdemo", "version_id", v1, "expected_version", v2)))["live_version_id"], v1)

	// 13. A gate added after a live release reports the prior live pointer.
	prior := h.data(h.call("deploy_page", o("slug", "mcpgated", "title", "Gated", "html", pageHTML(10))))
	h.exec("UPDATE pages SET require_approval = true WHERE slug = 'mcpgated'")
	gated := h.data(h.call("update_page", o("slug", "mcpgated", "html", pageHTML(11), "publish", true)))
	eq(t, dig(gated, "version", "status"), "pending")
	eq(t, gated["published"], false)
	eq(t, gated["version_is_live"], false)
	eq(t, gated["page_is_live"], true, "the prior published version still serves")
	eq(t, gated["live_version_id"], dig(prior, "version", "id"))
	mustMatch(t, `(?i)previously published version .* remains live`, strAt(gated, "next_step"))
	eq(t, dig(h.data(h.call("get_page", o("slug", "mcpgated"))), "page", "published_version_id"), dig(prior, "version", "id"))
	eq(t, h.fails(h.call("publish_page", o("slug", "mcpgated", "version_id", dig(gated, "version", "id")))).code(), "approval_required")
	h.exec("UPDATE pages SET require_approval = false WHERE slug = 'mcpgated'")
	draft := h.data(h.call("update_page", o("slug", "mcpgated", "html", pageHTML(11), "publish", false)))
	eq(t, draft["deduped"], false, "open page skips an unusable gated-era pending row")
	eq(t, dig(draft, "version", "status"), "draft")
	eq(t, h.data(h.call("publish_page", o("slug", "mcpgated", "version_id", dig(draft, "version", "id"), "expected_version", dig(prior, "version", "id"))))["version_is_live"], true)

	// 14. Remaining mutations keep their authority and serving semantics.
	automatic := h.data(h.call("deploy_page", o("slug", "mcpauto", "title", "Auto", "html", pageHTML(20))))
	eq(t, automatic["published"], true, "open-page deploy defaults to publish")
	mustMatch(t, `(?i)live now`, strAt(automatic, "next_step"))
	password := h.data(h.call("set_password", o("slug", "mcpauto", "password", "integration-password")))
	eq(t, password["has_password"], true)
	eq(t, password["page_is_live"], true)
	eq(t, h.data(h.call("set_title", o("slug", "mcpauto", "title", "Renamed Auto")))["title"], "Renamed Auto")
	// Schema failures are tool errors, and strict schemas reject unknown and
	// prototype-named keys.
	for _, args := range []any{
		o("slug", "mcpauto", "version_id", "not-an-id"),
	} {
		mustMatch(t, `(?i)Invalid arguments`, h.fails(h.call("publish_page", args)).text)
	}
	mustMatch(t, `(?i)Invalid arguments`, h.fails(h.call("get_page", o("slug", "mcpauto", "bogus", num(1)))).text)
	mustMatch(t, `(?i)Invalid arguments`, h.fails(h.call("get_page", o("slug", "mcpauto", "constructor", num(1)))).text)
	mustMatch(t, `(?i)Invalid arguments`, h.fails(h.call("set_password", o("slug", "mcpauto", "password", ""))).text, "MCP cannot express admin-only password clearing")
	deleted := h.data(h.call("delete_page", o("slug", "mcpauto")))
	eq(t, deleted["deleted"], true)
	eq(t, deleted["page_is_live"], false)
	eq(t, h.fails(h.call("get_page", o("slug", "mcpauto"))).code(), "page_not_found")
	eq(t, h.data(h.call("deploy_page", o("slug", "mcpauto", "title", "Reborn", "html", pageHTML(21))))["created"], true, "soft deletion frees the active slug")

	// 15. Admin takedown cannot be bypassed through deploy or delete.
	h.exec("UPDATE pages SET disabled = true WHERE slug = 'mcpauto'")
	eq(t, h.fails(h.call("delete_page", o("slug", "mcpauto"))).code(), "disabled_takedown")
	eq(t, h.fails(h.call("deploy_page", o("slug", "mcpauto", "html", pageHTML(22)))).code(), "disabled_takedown")

	// 16. Dedupe guidance distinguishes live from an older approved target.
	dedupeV1 := h.data(h.call("deploy_page", o("slug", "mcpdedup", "title", "Dedupe", "html", pageHTML(30))))
	eq(t, h.data(h.call("deploy_page", o("slug", "mcpdedup", "html", pageHTML(30), "publish", false)))["version_is_live"], true)
	h.data(h.call("deploy_page", o("slug", "mcpdedup", "html", pageHTML(31))))
	priorDuplicate := h.data(h.call("deploy_page", o("slug", "mcpdedup", "html", pageHTML(30), "publish", false)))
	eq(t, dig(priorDuplicate, "version", "id"), dig(dedupeV1, "version", "id"))
	eq(t, priorDuplicate["version_is_live"], false)
	mustMatch(t, `rollback_page`, strAt(priorDuplicate, "next_step"))

	// 17. create-or-deploy is atomic and converges under a slug race.
	eq(t, h.fails(h.call("deploy_page", o("slug", "mcpatomic-fail", "title", "Must Roll Back", "html", pageHTML(40), "expected_version", v1))).code(), "stale_version")
	eq(t, h.fails(h.call("get_page", o("slug", "mcpatomic-fail"))).code(), "page_not_found", "failed deploy leaves no empty page")
	var wg sync.WaitGroup
	raced := make([]response, 2)
	for i, title := range []string{"Race A", "Race B"} {
		wg.Add(1)
		go func(i int, title string) {
			defer wg.Done()
			raced[i] = h.call("deploy_page", o("slug", "mcpatomic-race", "title", title, "html", pageHTML(41+i)))
		}(i, title)
	}
	wg.Wait()
	created := 0
	for _, r := range raced {
		d := h.data(r)
		if d["created"] == true {
			created++
		}
		eq(t, d["page_is_live"], true, "both racers complete normally")
	}
	eq(t, created, 1, "one racer creates the page")

	// 18. The successful publish_page branch.
	base := h.data(h.call("deploy_page", o("slug", "mcppublish", "title", "Publish", "html", pageHTML(50))))
	pubDraft := h.data(h.call("update_page", o("slug", "mcppublish", "html", pageHTML(51), "publish", false)))
	eq(t, dig(pubDraft, "version", "status"), "draft")
	published := h.data(h.call("publish_page", o("slug", "mcppublish", "version_id", dig(pubDraft, "version", "id"), "expected_version", dig(base, "version", "id"))))
	eq(t, published["version_is_live"], true)
	eq(t, published["live_version_id"], dig(pubDraft, "version", "id"))
}
