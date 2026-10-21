// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package workspaces is one-level page organization. Workspaces do not
// participate in serving or versioning: moving a page only changes its
// nullable workspace_id. Reversible organization (create, rename, assign) is
// available to authenticated users and agents; destructive removal remains
// human-admin-only. Every mutation is transactional and audit-logged with the
// data change.
package workspaces

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/versions"
)

// Workspace is a workspace row. PageCount (live member pages) is present on
// List and Create results and absent from Rename's.
type Workspace struct {
	ID        jsx.ID   `json:"id"`
	Name      string   `json:"name"`
	CreatedAt jsx.Time `json:"created_at"`
	UpdatedAt jsx.Time `json:"updated_at"`
	PageCount *int     `json:"page_count,omitempty"`
}

// Removed is Remove's result.
type Removed struct {
	ID                 jsx.ID `json:"id"`
	Name               string `json:"name"`
	Deleted            bool   `json:"deleted"`
	UngroupedPageCount int    `json:"ungrouped_page_count"`
}

// Assignment is AssignPage's result. WorkspaceID is the normalized input (a
// JSON number when safe, as JavaScript returned it; null for Ungrouped).
type Assignment struct {
	Slug          string                `json:"slug"`
	WorkspaceID   versions.WorkspaceRef `json:"workspace_id"`
	WorkspaceName *string               `json:"workspace_name"`
}

// AssertOrganizer allows users and agents: creating, renaming and assigning
// only change reversible organization metadata (403 organizer_only).
func AssertOrganizer(a versions.Actor) error {
	if a.ActorType != audit.User && a.ActorType != audit.Agent {
		return apierror.Forbidden("workspace organization requires an authenticated user or agent", "organizer_only", nil)
	}
	return nil
}

// AssertHumanAdmin: removing a workspace detaches every member in bulk and is
// intentionally a separate, stricter trust boundary (403 admin_only).
func AssertHumanAdmin(a versions.Actor) error {
	if a.ActorType != audit.User {
		return apierror.Forbidden("removing a workspace is admin-only", "admin_only", nil)
	}
	return nil
}

// NormalizeID validates a workspace id (positive safe integer or decimal
// string within BIGINT range). With nullable, nil/Null/"" ⇒ 0 (Ungrouped).
func NormalizeID(v any, nullable bool) (versions.WorkspaceRef, error) {
	if nullable && (v == nil || v == versions.Null || v == "") {
		return 0, nil
	}
	id, err := versions.NormalizeWorkspaceID(v)
	if err != nil {
		return 0, apierror.BadRequest("workspace_id must be a positive database id or null", "bad_workspace_id", nil)
	}
	return id, nil
}

var controlRE = regexp.MustCompile("[\u0000-\u001f\u007f]")

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// NormalizeName trims a workspace name and collapses internal whitespace.
// 400 workspace_name_required / bad_workspace_name / workspace_name_too_long.
func NormalizeName(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", apierror.BadRequest("workspace name is required", "workspace_name_required", nil)
	}
	if controlRE.MatchString(s) {
		return "", apierror.BadRequest("workspace name contains unsupported control characters", "bad_workspace_name", nil)
	}
	name := strings.Join(strings.FieldsFunc(s, isJSSpace), " ")
	if name == "" {
		return "", apierror.BadRequest("workspace name is required", "workspace_name_required", nil)
	}
	if jsstr.UTF16Len(name) > 100 {
		return "", apierror.BadRequest("workspace name must be 100 characters or fewer", "workspace_name_too_long", nil)
	}
	return name, nil
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return err != nil && errors.As(err, &pg) && pg.Code == "23505"
}

// List is every workspace with its live member count, ordered by name.
func List(ctx context.Context, q db.Querier) ([]Workspace, error) {
	rows, err := q.Query(ctx,
		`SELECT w.id, w.name, w.created_at, w.updated_at,
		        COUNT(p.id) FILTER (WHERE p.deleted_at IS NULL)::int AS page_count
		   FROM workspaces w
		   LEFT JOIN pages p ON p.workspace_id = w.id
		  GROUP BY w.id
		  ORDER BY lower(w.name), w.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		var w Workspace
		var n int
		if err := rows.Scan(&w.ID, &w.Name, &w.CreatedAt, &w.UpdatedAt, &n); err != nil {
			return nil, err
		}
		w.PageCount = &n
		out = append(out, w)
	}
	return out, rows.Err()
}

func exists(name string) error {
	return apierror.Conflict("workspace already exists: "+name, "workspace_exists", nil)
}

// Create creates a workspace (409 workspace_exists on a case-insensitive
// duplicate).
func Create(ctx context.Context, d *db.DB, name any, a versions.Actor) (*Workspace, error) {
	if err := AssertOrganizer(a); err != nil {
		return nil, err
	}
	clean, err := NormalizeName(name)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Workspace, error) {
		w := &Workspace{}
		err := tx.QueryRow(ctx,
			`INSERT INTO workspaces (name) VALUES ($1) RETURNING id, name, created_at, updated_at`, clean).
			Scan(&w.ID, &w.Name, &w.CreatedAt, &w.UpdatedAt)
		if isUniqueViolation(err) {
			return nil, exists(clean)
		}
		if err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, entry(a, "create_workspace", 0,
			map[string]any{"workspace_id": w.ID.String(), "name": w.Name})); err != nil {
			return nil, err
		}
		zero := 0
		w.PageCount = &zero
		return w, nil
	})
}

// Rename renames a workspace (404 workspace_not_found, 409 workspace_exists).
// An unchanged name writes nothing.
func Rename(ctx context.Context, d *db.DB, id any, name any, a versions.Actor) (*Workspace, error) {
	if err := AssertOrganizer(a); err != nil {
		return nil, err
	}
	wid, err := NormalizeID(id, false)
	if err != nil {
		return nil, err
	}
	clean, err := NormalizeName(name)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Workspace, error) {
		cur := &Workspace{}
		err := tx.QueryRow(ctx, `SELECT id, name, created_at, updated_at FROM workspaces WHERE id = $1 FOR UPDATE`, int64(wid)).
			Scan(&cur.ID, &cur.Name, &cur.CreatedAt, &cur.UpdatedAt)
		if db.IsNoRows(err) {
			return nil, apierror.NotFound("workspace not found", "workspace_not_found", nil)
		}
		if err != nil {
			return nil, err
		}
		if cur.Name == clean {
			return cur, nil
		}
		w := &Workspace{}
		err = tx.QueryRow(ctx,
			`UPDATE workspaces SET name = $2, updated_at = now() WHERE id = $1
			 RETURNING id, name, created_at, updated_at`, int64(wid), clean).
			Scan(&w.ID, &w.Name, &w.CreatedAt, &w.UpdatedAt)
		if isUniqueViolation(err) {
			return nil, exists(clean)
		}
		if err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, entry(a, "rename_workspace", 0,
			map[string]any{"workspace_id": w.ID.String(), "from": cur.Name, "to": w.Name})); err != nil {
			return nil, err
		}
		return w, nil
	})
}

// Remove deletes a workspace (human admin only), detaching every member page
// to Ungrouped with a per-page audit entry. Pages, slugs, pointers and
// versions are untouched.
func Remove(ctx context.Context, d *db.DB, id any, a versions.Actor) (*Removed, error) {
	if err := AssertHumanAdmin(a); err != nil {
		return nil, err
	}
	wid, err := NormalizeID(id, false)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Removed, error) {
		out := &Removed{Deleted: true}
		err := tx.QueryRow(ctx, `SELECT id, name FROM workspaces WHERE id = $1 FOR UPDATE`, int64(wid)).Scan(&out.ID, &out.Name)
		if db.IsNoRows(err) {
			return nil, apierror.NotFound("workspace not found", "workspace_not_found", nil)
		}
		if err != nil {
			return nil, err
		}
		// Detach explicitly so the membership change updates page timestamps
		// and gets a per-page audit entry. The FK's ON DELETE SET NULL remains
		// a defense-in-depth guarantee for any future deletion path.
		rows, err := tx.Query(ctx,
			`UPDATE pages SET workspace_id = NULL, updated_at = now()
			  WHERE workspace_id = $1 RETURNING id, deleted_at IS NOT NULL`, int64(wid))
		if err != nil {
			return nil, err
		}
		type detached struct {
			id      jsx.ID
			deleted bool
		}
		var pages []detached
		for rows.Next() {
			var p detached
			if err := rows.Scan(&p.id, &p.deleted); err != nil {
				rows.Close()
				return nil, err
			}
			pages = append(pages, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, p := range pages {
			if err := audit.Write(ctx, tx, entry(a, "set_workspace", p.id,
				map[string]any{"from": wid, "to": nil, "reason": "workspace_removed"})); err != nil {
				return nil, err
			}
			if !p.deleted {
				out.UngroupedPageCount++
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, int64(wid)); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, entry(a, "delete_workspace", 0, map[string]any{
			"workspace_id": wid, "name": out.Name, "ungrouped_page_count": out.UngroupedPageCount,
		})); err != nil {
			return nil, err
		}
		return out, nil
	})
}

// AssignPage moves a page into a workspace (workspaceID nil/Null/"" ⇒
// Ungrouped). 404 workspace_not_found / page_not_found. An unchanged
// membership writes nothing.
func AssignPage(ctx context.Context, d *db.DB, slug string, workspaceID any, a versions.Actor) (*Assignment, error) {
	if err := AssertOrganizer(a); err != nil {
		return nil, err
	}
	s, err := versions.NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	wid, err := NormalizeID(workspaceID, true)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Assignment, error) {
		var name *string
		if wid != 0 {
			// Lock the workspace before the page. Remove takes the same order,
			// preventing assign/delete deadlocks and making deletion races
			// deterministic.
			var n string
			err := tx.QueryRow(ctx, `SELECT name FROM workspaces WHERE id = $1 FOR KEY SHARE`, int64(wid)).Scan(&n)
			if db.IsNoRows(err) {
				return nil, apierror.NotFound("workspace not found", "workspace_not_found", nil)
			}
			if err != nil {
				return nil, err
			}
			name = &n
		}
		var pageID, current jsx.ID
		var pageSlug string
		err := tx.QueryRow(ctx,
			`SELECT id, slug, workspace_id FROM pages WHERE slug = $1 AND deleted_at IS NULL FOR UPDATE`, s).
			Scan(&pageID, &pageSlug, &current)
		if db.IsNoRows(err) {
			return nil, apierror.NotFound("page not found: "+s, "page_not_found", nil)
		}
		if err != nil {
			return nil, err
		}
		if current != wid.ID() {
			var arg any
			if wid != 0 {
				arg = int64(wid)
			}
			if _, err := tx.Exec(ctx, `UPDATE pages SET workspace_id = $1, updated_at = now() WHERE id = $2`, arg, pageID); err != nil {
				return nil, err
			}
			var from any
			if current != 0 {
				from = current.String()
			}
			if err := audit.Write(ctx, tx, entry(a, "set_workspace", pageID, map[string]any{"from": from, "to": wid})); err != nil {
				return nil, err
			}
		}
		return &Assignment{Slug: pageSlug, WorkspaceID: wid, WorkspaceName: name}, nil
	})
}

func entry(a versions.Actor, action string, pageID jsx.ID, meta any) audit.Entry {
	return audit.Entry{Actor: a.Actor, ActorType: a.ActorType, Action: action, PageID: pageID, TokenID: a.TokenID, IP: a.IP, Metadata: meta}
}
