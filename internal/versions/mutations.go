// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagecookie"
	"github.com/ElcanoTek/pages/internal/passwordvault"
	"github.com/ElcanoTek/pages/internal/routens"
)

// ── page creation ───────────────────────────────────────────────────────────

// CreatePageInput creates an empty page. Workspace selection is reversible
// organization metadata, so both human admins and authenticated agents may
// choose it; theme/password are set via their own calls later.
type CreatePageInput struct {
	Slug            any // a string (non-string ⇒ 400 slug_required)
	Title           any // nil ⇒ ""
	ClientID        any // nil/Null ⇒ NULL
	WorkspaceID     any // nil/Null ⇒ Ungrouped
	RequireApproval any // bool; nil ⇒ false
}

// CreatePage creates a page. 409 slug_exists when a live page holds the slug
// (soft-deleted rows free theirs), 400 reserved_slug, 404 workspace_not_found.
func CreatePage(ctx context.Context, d *db.DB, in CreatePageInput, a Actor) (*CreatedPage, error) {
	requireApproval, err := boolArg(in.RequireApproval, false, "require_approval")
	if err != nil {
		return nil, err
	}
	slug, err := NormalizeSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	if err := routens.AssertSlugNotReserved(slug); err != nil {
		return nil, err
	}
	var workspaceID WorkspaceRef
	if !isNullish(in.WorkspaceID) {
		if err := assertWorkspaceOrganizer(a); err != nil {
			return nil, err
		}
		if workspaceID, err = NormalizeWorkspaceID(in.WorkspaceID); err != nil {
			return nil, err
		}
	}
	title := in.Title
	if title == nil {
		title = ""
	}
	titleText, titleOK := pgText(title)
	clientText, clientOK := pgText(in.ClientID)
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*CreatedPage, error) {
		var workspaceName *string
		if workspaceID != 0 {
			var name string
			err := tx.QueryRow(ctx, `SELECT name FROM workspaces WHERE id = $1 FOR KEY SHARE`, int64(workspaceID)).Scan(&name)
			if db.IsNoRows(err) {
				return nil, apierror.NotFound("workspace not found", "workspace_not_found", nil)
			}
			if err != nil {
				return nil, err
			}
			workspaceName = &name
		}
		// Only LIVE pages hold a slug (the partial unique index frees a
		// soft-deleted row's slug for reuse). The pre-check is a fast path; the
		// index is the real guard, so a concurrent create racing under READ
		// COMMITTED surfaces as a 23505 mapped to the same 409.
		exists := apierror.Conflict("slug already exists: "+slug, "slug_exists", nil)
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM pages WHERE slug = $1 AND deleted_at IS NULL`, slug).Scan(&one)
		if err == nil {
			return nil, exists
		}
		if !db.IsNoRows(err) {
			return nil, err
		}
		var wsArg any
		if workspaceID != 0 {
			wsArg = int64(workspaceID)
		}
		p := &CreatedPage{WorkspaceName: workspaceName}
		err = tx.QueryRow(ctx,
			`INSERT INTO pages (slug, title, client_id, workspace_id, require_approval)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING id, slug, title, client_id, workspace_id, theme_id,
			           require_approval, disabled, published_version_id, created_at, updated_at`,
			slug, nullableText(titleText, titleOK), nullableText(clientText, clientOK), wsArg, requireApproval).
			Scan(&p.ID, &p.Slug, &p.Title, &p.ClientID, &p.WorkspaceID, &p.ThemeID,
				&p.RequireApproval, &p.Disabled, &p.PublishedVersionID, &p.CreatedAt, &p.UpdatedAt)
		if isUniqueViolation(err) {
			return nil, exists
		}
		if err != nil {
			return nil, err
		}
		var meta any
		if workspaceID != 0 {
			meta = map[string]any{"workspace_id": workspaceID}
		}
		if err := audit.Write(ctx, tx, a.entry("create_page", p.ID, 0, meta)); err != nil {
			return nil, err
		}
		return p, nil
	})
}

// ── publish / rollback / approve / reject ───────────────────────────────────

// PublishInput promotes a draft.
type PublishInput struct {
	Slug            string
	VersionID       jsx.ID // required (0 ⇒ 400 version_id_required)
	ExpectedVersion any
}

// Publish promotes a `draft` to `approved` and makes it live. Agents may do
// so on open pages only.
func Publish(ctx context.Context, d *db.DB, in PublishInput, a Actor) (*Version, error) {
	if in.VersionID == 0 {
		return nil, apierror.BadRequest("version_id is required", "version_id_required", nil)
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Version, error) {
		page, err := LockPage(ctx, tx, in.Slug)
		if err != nil {
			return nil, err
		}
		if err := AssertAgentMayMovePointer(page, a); err != nil {
			return nil, err
		}
		v, err := lockVersion(ctx, tx, page.ID, in.VersionID)
		if err != nil {
			return nil, err
		}
		if v.Status != "draft" {
			return nil, apierror.Conflict(fmt.Sprintf("can only publish a draft version (status=%s)", v.Status), "not_draft", nil)
		}
		if _, err := tx.Exec(ctx, `UPDATE page_versions SET status = 'approved' WHERE id = $1`, in.VersionID); err != nil {
			return nil, err
		}
		if err := movePointer(ctx, tx, page, in.VersionID, in.ExpectedVersion, a, "publish", nil); err != nil {
			return nil, err
		}
		v.Status = "approved"
		return v, nil
	})
}

// RollbackInput moves the pointer to an approved version.
type RollbackInput struct {
	Slug            string
	VersionID       jsx.ID // 0 ⇒ the most recent approved version that isn't live
	ExpectedVersion any
	Note            any // audit-only reason
}

// Rollback moves the pointer to an already-`approved` version (no status
// change).
func Rollback(ctx context.Context, d *db.DB, in RollbackInput, a Actor) (*Version, error) {
	note, err := normalizeNote(in.Note)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Version, error) {
		page, err := LockPage(ctx, tx, in.Slug)
		if err != nil {
			return nil, err
		}
		if err := AssertAgentMayMovePointer(page, a); err != nil {
			return nil, err
		}
		target := in.VersionID
		if target == 0 {
			err := tx.QueryRow(ctx,
				`SELECT id FROM page_versions
				  WHERE page_id = $1 AND status = 'approved'
				    AND id IS DISTINCT FROM $2::bigint
				  ORDER BY created_at DESC, id DESC LIMIT 1`, page.ID, page.PublishedVersionID).Scan(&target)
			if db.IsNoRows(err) {
				return nil, apierror.Conflict("no earlier approved version to roll back to", "no_rollback_target", nil)
			}
			if err != nil {
				return nil, err
			}
		}
		v, err := lockVersion(ctx, tx, page.ID, target)
		if err != nil {
			return nil, err
		}
		if v.Status != "approved" {
			return nil, apierror.Conflict(fmt.Sprintf("can only roll back to an approved version (status=%s)", v.Status), "not_approved", nil)
		}
		if err := movePointer(ctx, tx, page, target, in.ExpectedVersion, a, "rollback", note); err != nil {
			return nil, err
		}
		return v, nil
	})
}

// ReviewInput names a version to approve or reject.
type ReviewInput struct {
	Slug            string
	VersionID       jsx.ID
	ExpectedVersion any // approve only
	Note            any // reject only (audit metadata)
}

// Approve is admin-only: stamp a reviewer on a `pending` version, set it
// `approved`, and make it live. The API layer enforces admin+CSRF; this
// requires ActorType "user" as defense in depth.
func Approve(ctx context.Context, d *db.DB, in ReviewInput, a Actor) (*Version, error) {
	if err := AssertAdminAction(a, ""); err != nil {
		return nil, err
	}
	if in.VersionID == 0 {
		return nil, apierror.BadRequest("version_id is required", "version_id_required", nil)
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Version, error) {
		page, err := LockPage(ctx, tx, in.Slug)
		if err != nil {
			return nil, err
		}
		v, err := lockVersion(ctx, tx, page.ID, in.VersionID)
		if err != nil {
			return nil, err
		}
		if v.Status != "pending" {
			return nil, apierror.Conflict(fmt.Sprintf("can only approve a pending version (status=%s)", v.Status), "not_pending", nil)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE page_versions SET status = 'approved', reviewed_by = $2, reviewed_at = now() WHERE id = $1`,
			in.VersionID, a.Actor); err != nil {
			return nil, err
		}
		if err := movePointer(ctx, tx, page, in.VersionID, in.ExpectedVersion, a, "approve", nil); err != nil {
			return nil, err
		}
		v.Status = "approved"
		reviewer := a.Actor
		v.ReviewedBy = &reviewer
		return v, nil
	})
}

// Reject is admin-only: mark a draft/pending version `rejected` (terminal;
// redeploying the content creates a fresh row). Does NOT move the pointer.
func Reject(ctx context.Context, d *db.DB, in ReviewInput, a Actor) (*Version, error) {
	if err := AssertAdminAction(a, ""); err != nil {
		return nil, err
	}
	if in.VersionID == 0 {
		return nil, apierror.BadRequest("version_id is required", "version_id_required", nil)
	}
	note, err := normalizeNote(in.Note)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Version, error) {
		page, err := LockPage(ctx, tx, in.Slug)
		if err != nil {
			return nil, err
		}
		v, err := lockVersion(ctx, tx, page.ID, in.VersionID)
		if err != nil {
			return nil, err
		}
		if v.Status != "pending" && v.Status != "draft" {
			return nil, apierror.Conflict(fmt.Sprintf("can only reject a draft/pending version (status=%s)", v.Status), "not_rejectable", nil)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE page_versions SET status = 'rejected', reviewed_by = $2, reviewed_at = now() WHERE id = $1`,
			in.VersionID, a.Actor); err != nil {
			return nil, err
		}
		var meta any
		if note != nil && *note != "" {
			meta = map[string]any{"note": *note}
		}
		if err := audit.Write(ctx, tx, a.entry("reject", page.ID, in.VersionID, meta)); err != nil {
			return nil, err
		}
		v.Status = "rejected"
		reviewer := a.Actor
		v.ReviewedBy = &reviewer
		return v, nil
	})
}

// ── admin-only page settings ────────────────────────────────────────────────
// These never touch the pointer or version content — they flip page flags or
// the theme. Admin-cookie+CSRF is enforced at the API layer; ActorType
// "user" is required here as defense in depth.

// SetDisabled is the takedown kill switch: disabled pages refuse to render.
// disabled must be a bool (400 bad_boolean).
func SetDisabled(ctx context.Context, d *db.DB, slug string, disabled any, a Actor) (*DisabledResult, error) {
	if err := AssertAdminAction(a, ""); err != nil {
		return nil, err
	}
	value, ok := disabled.(bool)
	if !ok {
		return nil, apierror.BadRequest("disabled must be a boolean", "bad_boolean", nil)
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*DisabledResult, error) {
		page, err := LockPage(ctx, tx, slug)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE pages SET disabled = $1, updated_at = now() WHERE id = $2`, value, page.ID); err != nil {
			return nil, err
		}
		action := "enable"
		if value {
			action = "disable"
		}
		if err := audit.Write(ctx, tx, a.entry(action, page.ID, 0, nil)); err != nil {
			return nil, err
		}
		return &DisabledResult{Slug: page.Slug, Disabled: value}, nil
	})
}

// SetApproval toggles the per-page approval gate. requireApproval must be a
// bool (400 bad_boolean).
func SetApproval(ctx context.Context, d *db.DB, slug string, requireApproval any, a Actor) (*ApprovalResult, error) {
	if err := AssertAdminAction(a, ""); err != nil {
		return nil, err
	}
	value, ok := requireApproval.(bool)
	if !ok {
		return nil, apierror.BadRequest("require_approval must be a boolean", "bad_boolean", nil)
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*ApprovalResult, error) {
		page, err := LockPage(ctx, tx, slug)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE pages SET require_approval = $1, updated_at = now() WHERE id = $2`, value, page.ID); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, a.entry("set_approval", page.ID, 0, map[string]any{"require_approval": value})); err != nil {
			return nil, err
		}
		return &ApprovalResult{Slug: page.Slug, RequireApproval: value}, nil
	})
}

// SetTheme points the page at a curated theme by name ("" or "flag" ⇒ the
// Flag default, stored as NULL). 400 unknown_theme. A caller holding a
// non-string JSON value passes String(value).
func SetTheme(ctx context.Context, d *db.DB, slug, theme string, a Actor) (*ThemeResult, error) {
	if err := AssertAdminAction(a, ""); err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*ThemeResult, error) {
		page, err := LockPage(ctx, tx, slug)
		if err != nil {
			return nil, err
		}
		var themeID jsx.ID
		if theme != "" && theme != "flag" {
			err := tx.QueryRow(ctx, `SELECT id FROM themes WHERE name = $1`, theme).Scan(&themeID)
			if db.IsNoRows(err) {
				return nil, apierror.BadRequest("unknown theme: "+theme, "unknown_theme", nil)
			}
			if err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE pages SET theme_id = $1, updated_at = now() WHERE id = $2`, themeID, page.ID); err != nil {
			return nil, err
		}
		name := theme
		if name == "" {
			name = "flag"
		}
		if err := audit.Write(ctx, tx, a.entry("set_theme", page.ID, 0, map[string]any{"theme": name})); err != nil {
			return nil, err
		}
		return &ThemeResult{Slug: page.Slug, Theme: name}, nil
	})
}

// SetPassword sets or clears the per-page client password (scrypt via
// pagecookie). Authority is asymmetric by risk: SETTING is agent-capable (the
// "make this dashboard client-accessible" step); CLEARING (nil/Null/"") is
// admin-only, because on a live client page it flips the page back to
// staff-only and the content host then 403s every real client — a silent
// takedown.
func SetPassword(ctx context.Context, d *db.DB, slug string, password any, a Actor) (*PasswordResult, error) {
	clearing := isNullish(password) || password == ""
	if clearing {
		if err := AssertAdminAction(a, "clearing a page password is admin-only"); err != nil {
			return nil, err
		}
	}
	var hash, sealed any
	if !clearing {
		pw, ok := password.(string)
		if !ok {
			return nil, apierror.BadRequest("password must be a non-empty string", "bad_password", nil)
		}
		if jsonv.UTF16Len(pw) > 512 {
			return nil, apierror.BadRequest("password must be 512 characters or fewer", "password_too_long", nil)
		}
		// Hash BEFORE opening the transaction: scrypt is deliberately
		// expensive and must not run while holding the page's row lock.
		h, err := pagecookie.HashPassword(pw)
		if err != nil {
			return nil, err
		}
		hash = h
		// The staff-readable copy is sealed against this exact hash and
		// written in the same UPDATE, so the two can never disagree.
		if s := passwordvault.Seal(pw, h); s != "" {
			sealed = s
		}
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*PasswordResult, error) {
		page, err := LockPage(ctx, tx, slug)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE pages SET password_hash = $1, password_ciphertext = $2, updated_at = now() WHERE id = $3`,
			hash, sealed, page.ID); err != nil {
			return nil, err
		}
		action := "clear_password"
		if hash != nil {
			action = "set_password"
		}
		if err := audit.Write(ctx, tx, a.entry(action, page.ID, 0, nil)); err != nil {
			return nil, err
		}
		return &PasswordResult{Slug: page.Slug, HasPassword: hash != nil}, nil
	})
}

// RevealPassword returns the page's current client password, for staff to
// resend it without rotating it. Admin-only and audited: an agent never reads
// a password, and every reveal names who looked. Available=false means Pages
// has no copy that opens yet (a password set before copies were kept appears
// after a client next signs in with it, or once it is set again).
func RevealPassword(ctx context.Context, d *db.DB, slug string, a Actor) (*RevealedPassword, error) {
	if err := AssertAdminAction(a, "showing a page password is admin-only"); err != nil {
		return nil, err
	}
	s, err := NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*RevealedPassword, error) {
		var id jsx.ID
		var pageSlug string
		var hash, ciphertext *string
		err := tx.QueryRow(ctx,
			`SELECT id, slug, password_hash, password_ciphertext
			   FROM pages WHERE slug = $1 AND deleted_at IS NULL`, s).Scan(&id, &pageSlug, &hash, &ciphertext)
		if db.IsNoRows(err) {
			return nil, pageNotFound(s)
		}
		if err != nil {
			return nil, err
		}
		if hash == nil || *hash == "" {
			return &RevealedPassword{Slug: pageSlug}, nil
		}
		password, ok := passwordvault.Open(deref(ciphertext), *hash)
		if err := audit.Write(ctx, tx, a.entry("reveal_password", id, 0, map[string]any{"available": ok})); err != nil {
			return nil, err
		}
		out := &RevealedPassword{Slug: pageSlug, HasPassword: true, Available: ok}
		if ok {
			out.Password = &password
		}
		return out, nil
	})
}

// SetTitle renames a page: agent-capable authoring metadata. title must be a
// non-blank string (400 title_required); it is trimmed and cut to 200
// characters. Unchanged titles write no audit row.
func SetTitle(ctx context.Context, d *db.DB, slug string, title any, a Actor) (*TitleResult, error) {
	t, ok := title.(string)
	if !ok || jsTrim(t) == "" {
		return nil, apierror.BadRequest("title is required", "title_required", nil)
	}
	clean := jsonv.SliceUTF16(jsTrim(t), 0, 200)
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*TitleResult, error) {
		page, err := LockPage(ctx, tx, slug)
		if err != nil {
			return nil, err
		}
		if page.Title == clean {
			return &TitleResult{Slug: page.Slug, Title: clean}, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE pages SET title = $1, updated_at = now() WHERE id = $2`, clean, page.ID); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, a.entry("set_title", page.ID, 0, map[string]any{"title": clean})); err != nil {
			return nil, err
		}
		return &TitleResult{Slug: page.Slug, Title: clean}, nil
	})
}

// DeletePage soft-deletes: sets deleted_at (every read/serve query filters it)
// and NULLs the live pointer. The row, its versions and the audit trail stay,
// and the slug is freed for reuse; RestorePage undoes it. Authority mirrors
// publish/rollback: agents may delete OPEN pages only.
func DeletePage(ctx context.Context, d *db.DB, slug string, a Actor) (*DeleteResult, error) {
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*DeleteResult, error) {
		page, err := LockPage(ctx, tx, slug)
		if err != nil {
			return nil, err
		}
		if err := AssertAgentMayMovePointer(page, a); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE pages SET published_version_id = NULL, deleted_at = now(), updated_at = now() WHERE id = $1`, page.ID); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, a.entry("delete_page", page.ID, 0,
			map[string]any{"had_published": idOrNil(page.PublishedVersionID)})); err != nil {
			return nil, err
		}
		return &DeleteResult{Slug: page.Slug, Deleted: true}, nil
	})
}

// RestorePage is admin-only undelete of the most recently deleted row for the
// slug, provided no LIVE page has taken it since (409 slug_taken). A reserved
// slug is refused (400 reserved_slug): restoring it would put a live page at
// an address a route now owns.
func RestorePage(ctx context.Context, d *db.DB, slug string, a Actor) (*RestoreResult, error) {
	if err := AssertAdminAction(a, ""); err != nil {
		return nil, err
	}
	s, err := NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	if err := routens.AssertSlugNotReserved(s); err != nil {
		return nil, err
	}
	taken := apierror.Conflict("a live page already uses this slug: "+s, "slug_taken", nil)
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*RestoreResult, error) {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM pages WHERE slug = $1 AND deleted_at IS NULL`, s).Scan(&one)
		if err == nil {
			return nil, taken
		}
		if !db.IsNoRows(err) {
			return nil, err
		}
		var id jsx.ID
		err = tx.QueryRow(ctx,
			`SELECT id FROM pages WHERE slug = $1 AND deleted_at IS NOT NULL
			  ORDER BY deleted_at DESC LIMIT 1 FOR UPDATE`, s).Scan(&id)
		if db.IsNoRows(err) {
			return nil, apierror.NotFound("no deleted page to restore: "+s, "page_not_found", nil)
		}
		if err != nil {
			return nil, err
		}
		// The pre-check is advisory; a racing create/restore surfaces as the
		// partial unique index's 23505, mapped to the same 409.
		if _, err := tx.Exec(ctx, `UPDATE pages SET deleted_at = NULL, updated_at = now() WHERE id = $1`, id); err != nil {
			if isUniqueViolation(err) {
				return nil, taken
			}
			return nil, err
		}
		if err := audit.Write(ctx, tx, a.entry("restore_page", id, 0, nil)); err != nil {
			return nil, err
		}
		return &RestoreResult{Slug: s, Restored: true}, nil
	})
}

// ── refresh checks ──────────────────────────────────────────────────────────

// RefreshCheckOutcomes is the closed vocabulary of recorded check outcomes
// (mirrored by the pages_last_check_all_or_none constraint).
var RefreshCheckOutcomes = []string{"updated", "source_not_updated", "source_unreachable", "blocked", "failed"}

const maxCheckDetail = 500

// RefreshCheckInput records a deliberate refresh check.
type RefreshCheckInput struct {
	Slug           string
	Outcome        string
	Detail         any // string ≤ 500 (trimmed; blank ⇒ null) or nil/Null
	SourceAsOfSeen any // ISO timestamp string or nil/Null
	Now            time.Time
}

// RecordRefreshCheck stamps that someone looked, and what they saw, without
// creating a version. The healthy outcome of a daily refresh is often "the
// source has no new day, so I published nothing", which otherwise writes
// nothing anywhere and is byte-identical to a page whose job was deleted. It
// moves checked_at without touching the pointer, the data, or any hash.
// Explicitly not a version: that would put a fresh entry in every page's
// history every day and defeat the dedupe.
func RecordRefreshCheck(ctx context.Context, d *db.DB, in RefreshCheckInput, a Actor) (*RefreshCheckResult, error) {
	slug, err := NormalizeSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	valid := false
	for _, o := range RefreshCheckOutcomes {
		valid = valid || o == in.Outcome
	}
	if !valid {
		return nil, apierror.BadRequest("outcome must be one of: "+strings.Join(RefreshCheckOutcomes, ", "), "refresh_check_outcome_invalid", nil)
	}
	var detail *string
	if !isNullish(in.Detail) {
		s, ok := in.Detail.(string)
		if !ok {
			return nil, apierror.BadRequest("detail must be a string", "refresh_check_detail_invalid", nil)
		}
		s = jsTrim(s)
		if s != "" {
			if jsonv.UTF16Len(s) > maxCheckDetail {
				return nil, apierror.BadRequest(fmt.Sprintf("detail must be at most %d characters", maxCheckDetail), "refresh_check_detail_invalid", nil)
			}
			detail = &s
		}
	}
	var seen time.Time
	if !isNullish(in.SourceAsOfSeen) {
		switch v := in.SourceAsOfSeen.(type) {
		case string:
			seen = ParseTime(v)
		case time.Time:
			seen = msTime(v)
		}
		if seen.IsZero() {
			return nil, apierror.BadRequest("source_as_of_seen must be an RFC3339 timestamp", "refresh_check_source_invalid", nil)
		}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	checkedAt := msTime(now)
	var seenArg, seenMeta any
	if !seen.IsZero() {
		seenArg, seenMeta = seen, isoString(seen)
	}
	var detailMeta any
	if detail != nil {
		detailMeta = *detail
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*RefreshCheckResult, error) {
		page, err := LockPage(ctx, tx, slug)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE pages
			    SET last_check_at = $1, last_check_outcome = $2,
			        last_check_detail = $3, last_check_source_as_of = $4
			  WHERE id = $5`, checkedAt, in.Outcome, detail, seenArg, page.ID); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, a.entry("record_refresh_check", page.ID, 0,
			map[string]any{"outcome": in.Outcome, "detail": detailMeta, "source_as_of_seen": seenMeta})); err != nil {
			return nil, err
		}
		// Read the freshness back rather than echoing the request: the
		// caller's next decision needs the page's coverage, which this call
		// did not change.
		var source, refreshed jsx.Time
		var legacy *string
		err = tx.QueryRow(ctx,
			`SELECT pv.source_as_of, pv.refreshed_at,
			        CASE WHEN pv.data_sha256 IS NULL AND position('pages-data' IN pv.html) > 0
			             THEN pv.html END AS legacy_managed_html
			   FROM pages p LEFT JOIN page_versions pv ON pv.id = p.published_version_id
			  WHERE p.id = $1`, page.ID).Scan(&source, &refreshed, &legacy)
		if err != nil && !db.IsNoRows(err) {
			return nil, err
		}
		c, err := indexedOrLegacyCoverage(source, refreshed, legacy)
		if err != nil {
			return nil, err
		}
		return &RefreshCheckResult{Slug: page.Slug, Freshness: FreshnessOf(FreshnessInput{
			SourceAsOf: c.sourceAsOf, RefreshedAt: c.refreshedAt,
			LastCheckAt: checkedAt, LastCheckOutcome: in.Outcome, LastCheckDetail: deref(detail), LastCheckSourceAsOf: seen,
		}, now)}, nil
	})
}
