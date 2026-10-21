// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Workspace organization and update-prompt preparation.

import (
	"context"
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/updateprompts"
	"github.com/ElcanoTek/pages/internal/workspaces"
)

// ── workspaces ──────────────────────────────────────────────────────────────

func listWorkspaces(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	q, _ := argStr(a, "query")
	query := strings.ToLower(jsstr.Trim(q))
	var queryValue any
	if query != "" {
		queryValue = query
	}
	filters := jsonv.ObjectOf("query", queryValue)
	cursor, _ := argStr(a, "cursor")
	after, err := decodeCursor(cursor, "workspaces", filters)
	if err != nil {
		return nil, err
	}
	if after != nil {
		_, isStr := after.Value("name").(string)
		if !isStr || !decimalIDRE.MatchString(jsToString(after.Value("id"), after.Has("id"))) {
			return nil, apierror.BadRequest("invalid pagination cursor", "invalid_cursor", nil)
		}
	}
	rows, err := workspaces.List(ctx, c.d)
	if err != nil {
		return nil, err
	}
	if query != "" {
		kept := rows[:0:0]
		for _, r := range rows {
			if strings.Contains(strings.ToLower(r.Name), query) {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if after != nil {
		// Keep PostgreSQL's exact collation/order authoritative.
		// Re-sorting or tuple-comparing here can disagree for Unicode
		// names; locate the cursor row in the already ordered result and
		// continue after it.
		name, _ := after.Value("name").(string)
		id := jsonv.ToString(after.Value("id"))
		index := -1
		for i, r := range rows {
			if strings.ToLower(r.Name) == name && r.ID.String() == id {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, apierror.BadRequest("invalid or stale pagination cursor", "invalid_cursor", nil)
		}
		rows = rows[index+1:]
	}
	limit, _ := argInt(a, "limit")
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	var next any
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		next = encodeCursor("workspaces", jsonv.ObjectOf("name", strings.ToLower(last.Name), "id", last.ID.String()), filters)
	}
	if rows == nil {
		rows = []workspaces.Workspace{}
	}
	return jsonv.ObjectOf("workspaces", rows, "next_cursor", next), nil
}

func createWorkspace(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	name, _ := argStr(a, "name")
	w, err := workspaces.Create(ctx, c.d, name, c.actor)
	if err != nil {
		return nil, err
	}
	return jsonv.ObjectOf("workspace", w), nil
}

func renameWorkspace(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	id, _ := argStr(a, "workspace_id")
	name, _ := argStr(a, "name")
	renamed, err := workspaces.Rename(ctx, c.d, id, name, c.actor)
	if err != nil {
		return nil, err
	}
	rows, err := workspaces.List(ctx, c.d)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if sameID(r.ID, renamed.ID) {
			return jsonv.ObjectOf("workspace", r), nil
		}
	}
	out, err := spread(renamed, jsonv.ObjectOf("page_count", 0))
	if err != nil {
		return nil, err
	}
	return jsonv.ObjectOf("workspace", out), nil
}

func setPageWorkspace(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	assigned, err := workspaces.AssignPage(ctx, c.d, slug, argAny(a, "workspace_id"), c.actor)
	if err != nil {
		return nil, err
	}
	return jsonv.ObjectOf("page", assigned), nil
}

// ── update prompts ──────────────────────────────────────────────────────────

// prepareInput: allowUnbound is a server-side switch, never read off the
// arguments, so a tool caller cannot set one — it relaxes a safety gate,
// and the only caller entitled to it is the legacy compatibility alias.
func prepareInput(a *jsonv.Object, allowUnbound bool) updateprompts.PrepareInput {
	updateType, ok := argStr(a, "update_type")
	if !ok || updateType == "" {
		updateType = "auto"
	}
	publish := argBoolOr(a, "publish", true)
	in := updateprompts.PrepareInput{
		Slug:                  a.Value("slug"),
		Instructions:          a.Value("instructions"),
		Recurring:             argBoolOr(a, "recurring", false),
		UpdateType:            updateType,
		Publish:               &publish,
		AllowUnboundRecurring: allowUnbound,
	}
	if v, ok := a.Get("sources"); ok {
		in.Sources = v
	}
	return in
}

func prepareDashboardUpdateTool(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	return updateprompts.Prepare(ctx, c.d, prepareInput(a, false))
}

func configurePageRefresh(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	if truthy(a.Value("instructions")) {
		return updateprompts.Prepare(ctx, c.d, prepareInput(a, false))
	}
	workflow, ok := a.Get("workflow")
	if !ok || !truthy(workflow) {
		return nil, apierror.BadRequest(
			"instructions are required; older clients may instead supply their credential-free workflow",
			"update_instructions_required", nil)
	}
	if err := updateprompts.AssertCredentialFree(workflow, ""); err != nil {
		return nil, err
	}
	// daily_at_utc is the legacy client's cadence. It belongs to whatever
	// schedules the job, so it is accepted and no longer written into the
	// prompt.
	legacy := jsonv.ObjectOf(
		"slug", a.Value("slug"),
		"instructions", "Use this credential-free source, freshness, reconciliation, and mapping contract exactly: "+jsonv.Stringify(workflow),
		"recurring", true,
		"update_type", "data",
	)
	if v, ok := a.Get("publish"); ok {
		legacy.Set("publish", v)
	}
	in := prepareInput(legacy, true)
	// The legacy workflow already names a connector per source; lift those
	// into real bindings so an older caller gets the same no-substitutions
	// gate. A workflow whose entries omit mcp_server yields none, and that
	// client has no way to supply them — this alias exists precisely so it
	// keeps working, so it is exempt from the recurring-bindings gate (its
	// source detail still travels inside the serialized contract above).
	if lifted := updateprompts.SourcesFromWorkflow(workflow); lifted != nil {
		in.Sources = lifted
	}
	prepared, err := updateprompts.Prepare(ctx, c.d, in)
	if err != nil {
		return nil, err
	}
	return spread(prepared, jsonv.ObjectOf("next_step",
		"Pages did not install the legacy cadence and did not honor run_now. Use prompt verbatim as the prompt of the recurring task and set its cadence on the task; do not claim this update was configured or executed."))
}
