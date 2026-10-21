// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package portals

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagecookie"
	"github.com/ElcanoTek/pages/internal/passwordvault"
	"github.com/ElcanoTek/pages/internal/versions"
)

// ── mutations (admin only, every one audited in its own transaction) ────────

func entry(a versions.Actor, action string, pageID jsx.ID, meta any) audit.Entry {
	return audit.Entry{Actor: a.Actor, ActorType: a.ActorType, Action: action, PageID: pageID,
		TokenID: a.TokenID, IP: a.IP, Metadata: meta}
}

// hashAndSeal hashes the secret and seals the staff-readable copy against
// that exact hash. scrypt is deliberately expensive: callers run it BEFORE
// opening the transaction so the KDF never runs while holding a row lock
// (mirrors versions.SetPassword).
func hashAndSeal(secret string) (hash string, sealed any, err error) {
	hash, err = pagecookie.HashPassword(secret)
	if err != nil {
		return "", nil, err
	}
	if s := passwordvault.Seal(secret, hash); s != "" {
		sealed = s
	}
	return hash, sealed, nil
}

// Create creates a portal with a generated (default) or supplied credential.
// 409 portal_exists / portal_name_exists on a live duplicate slug / name
// (case-insensitive). The plaintext is returned once and never audited; staff
// can show it again through RevealPassword.
func Create(ctx context.Context, d *db.DB, in CreateInput, a versions.Actor) (*Created, error) {
	if err := AssertPortalAdmin(a); err != nil {
		return nil, err
	}
	slug, err := NormalizeSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	name, err := NormalizeName(in.Name)
	if err != nil {
		return nil, err
	}
	secret, generated, err := secretFor(in.Password)
	if err != nil {
		return nil, err
	}
	hash, sealed, err := hashAndSeal(secret)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Created, error) {
		var row Row
		err := tx.QueryRow(ctx,
			`INSERT INTO page_portals (slug, name, password_hash, password_ciphertext) VALUES ($1, $2, $3, $4)
			 RETURNING `+portalColumns, slug, name, hash, sealed).Scan(row.scanArgs()...)
		if err != nil {
			return nil, uniqueViolation(err, slug, name)
		}
		if err := audit.Write(ctx, tx, entry(a, "create_portal", 0, map[string]any{
			"portal_id": row.ID, "slug": row.Slug, "name": row.Name,
		})); err != nil {
			return nil, err
		}
		return &Created{
			Portal:            CreatedPortal{Row: row},
			Password:          secret,
			PasswordGenerated: generated,
		}, nil
	})
}

// Rename renames a portal (404 portal_not_found, 409 portal_name_exists). An
// unchanged name writes nothing.
func Rename(ctx context.Context, d *db.DB, in RenameInput, a versions.Actor) (*Renamed, error) {
	if err := AssertPortalAdmin(a); err != nil {
		return nil, err
	}
	id, err := NormalizeID(in.ID, "")
	if err != nil {
		return nil, err
	}
	name, err := NormalizeName(in.Name)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Renamed, error) {
		current, err := lockPortal(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if current.Name == name {
			return &Renamed{Portal: *current}, nil
		}
		var row Row
		err = tx.QueryRow(ctx,
			`UPDATE page_portals SET name = $2, updated_at = now() WHERE id = $1
			 RETURNING `+portalColumns, current.ID, name).Scan(row.scanArgs()...)
		if err != nil {
			return nil, uniqueViolation(err, current.Slug, name)
		}
		if err := audit.Write(ctx, tx, entry(a, "rename_portal", 0, map[string]any{
			"portal_id": row.ID, "slug": row.Slug, "from": current.Name, "to": row.Name,
		})); err != nil {
			return nil, err
		}
		return &Renamed{Portal: row}, nil
	})
}

// SetPassword rotates the portal credential (generated when Password is
// omitted). There is no clear path: the column is NOT NULL because an empty
// credential digest would collide with every staff-only page (migration 017).
// Rotating is also the revocation primitive — a portal session embeds
// PortalCredentialDigest(password_hash), so a new hash (fresh salt even for
// the same password) invalidates every live session for this portal the
// moment it commits.
func SetPassword(ctx context.Context, d *db.DB, in SetPasswordInput, a versions.Actor) (*PasswordSet, error) {
	if err := AssertPortalAdmin(a); err != nil {
		return nil, err
	}
	id, err := NormalizeID(in.ID, "")
	if err != nil {
		return nil, err
	}
	secret, generated, err := secretFor(in.Password)
	if err != nil {
		return nil, err
	}
	hash, sealed, err := hashAndSeal(secret)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*PasswordSet, error) {
		current, err := lockPortal(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		var row Row
		if err := tx.QueryRow(ctx,
			`UPDATE page_portals SET password_hash = $2, password_ciphertext = $3, updated_at = now() WHERE id = $1
			 RETURNING `+portalColumns, current.ID, hash, sealed).Scan(row.scanArgs()...); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, entry(a, "set_portal_password", 0, map[string]any{
			"portal_id": current.ID, "slug": current.Slug, "generated": generated,
		})); err != nil {
			return nil, err
		}
		return &PasswordSet{Portal: row, Password: secret, PasswordGenerated: generated}, nil
	})
}

// RevealPassword returns the portal's current password, for staff to resend
// it to the partner without rotating it, which would sign out every partner
// device. Admin only and audited, like every portal action; Available=false
// means Pages has no copy that opens yet (internal/passwordvault).
func RevealPassword(ctx context.Context, d *db.DB, id any, a versions.Actor) (*RevealedPassword, error) {
	if err := AssertPortalAdmin(a); err != nil {
		return nil, err
	}
	pid, err := NormalizeID(id, "")
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*RevealedPassword, error) {
		var portalID jsx.ID
		var slug, hash string
		var ciphertext *string
		err := tx.QueryRow(ctx,
			`SELECT id, slug, password_hash, password_ciphertext
			   FROM page_portals WHERE id = $1 AND deleted_at IS NULL`, pid).
			Scan(&portalID, &slug, &hash, &ciphertext)
		if db.IsNoRows(err) {
			return nil, portalNotFound()
		}
		if err != nil {
			return nil, err
		}
		out := &RevealedPassword{ID: portalID}
		if ciphertext != nil {
			if pw, ok := passwordvault.Open(*ciphertext, hash); ok {
				out.Available, out.Password = true, &pw
			}
		}
		if err := audit.Write(ctx, tx, entry(a, "reveal_portal_password", 0, map[string]any{
			"portal_id": portalID, "slug": slug, "available": out.Available,
		})); err != nil {
			return nil, err
		}
		return out, nil
	})
}

// AddPage adds a page to a portal: the one mutation that changes who can see
// what, which is why the whole package is admin-only. 404 portal_not_found /
// page_not_found, 409 portal_page_exists / portal_fanout_exceeded. It reports
// whether it just reclassified a staff-only page as client-readable so the
// admin UI can say so at the moment of adding; the same fact is stamped into
// the audit metadata.
func AddPage(ctx context.Context, d *db.DB, in AddPageInput, a versions.Actor) (*PageAdded, error) {
	if err := AssertPortalAdmin(a); err != nil {
		return nil, err
	}
	id, err := NormalizeID(in.ID, "")
	if err != nil {
		return nil, err
	}
	slug, err := versions.NormalizeSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	label, err := NormalizeLabel(in.Label)
	if err != nil {
		return nil, err
	}
	sortOrder, err := NormalizeSortOrder(in.SortOrder)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*PageAdded, error) {
		// Portal first, then page — the same lock order as every function
		// here, so concurrent membership changes serialize instead of
		// deadlocking.
		portal, err := lockPortal(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		page, err := versions.LockPage(ctx, tx, slug)
		if err != nil {
			return nil, err
		}
		var hasPassword, alreadyMember bool
		var portalCount int
		if err := tx.QueryRow(ctx,
			`SELECT (p.password_hash IS NOT NULL) AS has_password,
			        (SELECT count(*)::int FROM page_portal_members m
			           JOIN page_portals pp ON pp.id = m.portal_id AND pp.deleted_at IS NULL
			          WHERE m.page_id = p.id) AS portal_count,
			        EXISTS (SELECT 1 FROM page_portal_members m2
			                 WHERE m2.page_id = p.id AND m2.portal_id = $2) AS already_member
			   FROM pages p WHERE p.id = $1`, page.ID, portal.ID).
			Scan(&hasPassword, &portalCount, &alreadyMember); err != nil {
			return nil, err
		}
		exists := apierror.Conflict(page.Slug+" is already in this portal", "portal_page_exists", nil)
		if alreadyMember {
			return nil, exists
		}
		// Counted under the page's row lock, so two admins adding the same
		// page to two different portals cannot both pass the cap.
		if portalCount >= MaxPortalsPerPage {
			return nil, apierror.Conflict(
				fmt.Sprintf("%s is already in %d portals (limit %d)", page.Slug, portalCount, MaxPortalsPerPage),
				"portal_fanout_exceeded", nil)
		}
		// Both checks above are made under the page's row lock, but the
		// primary key is the actual guarantee, and a raced insert should read
		// as the same 409 rather than a raw 500.
		m := MemberView{Slug: page.Slug, Title: page.Title}
		err = tx.QueryRow(ctx,
			`INSERT INTO page_portal_members (portal_id, page_id, label, sort_order)
			 VALUES ($1, $2, $3, $4)
			 RETURNING page_id, label, sort_order, added_at`, portal.ID, page.ID, label, sortOrder).
			Scan(&m.PageID, &m.Label, &m.SortOrder, &m.AddedAt)
		if isUniqueViolation(err) {
			return nil, exists
		}
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE page_portals SET updated_at = now() WHERE id = $1`, portal.ID); err != nil {
			return nil, err
		}
		// A staff-only page (password_hash IS NULL) becomes readable by anyone
		// holding the portal password. That is the intended end state — it is
		// how a partner-facing dashboard gets made — but it is a
		// reclassification of confidential content, so it is recorded as one.
		reclassifies := !hasPassword
		if err := audit.Write(ctx, tx, entry(a, "add_portal_page", page.ID, map[string]any{
			"portal_id":               portal.ID,
			"portal_slug":             portal.Slug,
			"page_slug":               page.Slug,
			"label":                   label,
			"sort_order":              sortOrder,
			"page_had_password":       hasPassword,
			"reclassifies_staff_only": reclassifies,
		})); err != nil {
			return nil, err
		}
		m.DisplayTitle = displayTitle(m.Label, page.Title, page.Slug)
		return &PageAdded{Member: m, PortalID: portal.ID, ReclassifiesStaffOnly: reclassifies}, nil
	})
}

// UpdatePage edits the curated half of a membership (its partner-facing label
// and its order) without a remove/add round trip, which would drop the home
// page and read as two unrelated events in the audit log. Absent fields are
// left alone; an explicit null label falls back to pages.title. 400
// portal_member_nothing_to_update, 404 portal_page_not_found.
func UpdatePage(ctx context.Context, d *db.DB, in UpdatePageInput, a versions.Actor) (*PageUpdated, error) {
	if err := AssertPortalAdmin(a); err != nil {
		return nil, err
	}
	id, err := NormalizeID(in.ID, "")
	if err != nil {
		return nil, err
	}
	target, err := normalizeMemberTarget(in.Slug, in.PageID)
	if err != nil {
		return nil, err
	}
	setsLabel := in.Label != nil
	setsOrder := in.SortOrder != nil
	if !setsLabel && !setsOrder {
		return nil, apierror.BadRequest("pass a label or a sort_order to change", "portal_member_nothing_to_update", nil)
	}
	var nextLabel *string
	if setsLabel {
		if nextLabel, err = NormalizeLabel(in.Label); err != nil {
			return nil, err
		}
	}
	nextOrder := 0
	if setsOrder {
		if nextOrder, err = NormalizeSortOrder(in.SortOrder); err != nil {
			return nil, err
		}
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*PageUpdated, error) {
		portal, err := lockPortal(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		page, err := resolveMemberPage(ctx, tx, target)
		if err != nil {
			return nil, err
		}
		var labelFrom *string
		var orderFrom int
		err = tx.QueryRow(ctx,
			`SELECT label, sort_order FROM page_portal_members
			  WHERE portal_id = $1 AND page_id = $2 FOR UPDATE`, portal.ID, page.ID).Scan(&labelFrom, &orderFrom)
		if db.IsNoRows(err) {
			return nil, apierror.NotFound(page.Slug+" is not in this portal", "portal_page_not_found", nil)
		}
		if err != nil {
			return nil, err
		}
		m := MemberView{Slug: page.Slug, Title: page.Title}
		if err := tx.QueryRow(ctx,
			`UPDATE page_portal_members
			    SET label = CASE WHEN $3 THEN $4 ELSE label END,
			        sort_order = CASE WHEN $5 THEN $6::int ELSE sort_order END
			  WHERE portal_id = $1 AND page_id = $2
			  RETURNING page_id, label, sort_order, added_at`,
			portal.ID, page.ID, setsLabel, nextLabel, setsOrder, nextOrder).
			Scan(&m.PageID, &m.Label, &m.SortOrder, &m.AddedAt); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE page_portals SET updated_at = now() WHERE id = $1`, portal.ID); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, entry(a, "update_portal_page", page.ID, map[string]any{
			"portal_id":       portal.ID,
			"portal_slug":     portal.Slug,
			"page_slug":       page.Slug,
			"label_from":      labelFrom,
			"label_to":        m.Label,
			"sort_order_from": orderFrom,
			"sort_order_to":   m.SortOrder,
		})); err != nil {
			return nil, err
		}
		m.DisplayTitle = displayTitle(m.Label, page.Title, page.Slug)
		return &PageUpdated{Member: m, PortalID: portal.ID}, nil
	})
}

// RemovePage takes the page out of this portal only (404
// portal_page_not_found). Membership is read live per request (no
// portal-authorised render mints a page cookie), so this is effective on the
// member's very next request. A home page that is no longer a member would
// send every arriving partner to a page this portal no longer authorizes, so
// it goes with it (HomeCleared).
func RemovePage(ctx context.Context, d *db.DB, in RemovePageInput, a versions.Actor) (*PageRemoved, error) {
	if err := AssertPortalAdmin(a); err != nil {
		return nil, err
	}
	id, err := NormalizeID(in.ID, "")
	if err != nil {
		return nil, err
	}
	target, err := normalizeMemberTarget(in.Slug, in.PageID)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*PageRemoved, error) {
		portal, err := lockPortal(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		page, err := resolveMemberPage(ctx, tx, target)
		if err != nil {
			return nil, err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM page_portal_members WHERE portal_id = $1 AND page_id = $2`, portal.ID, page.ID)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, apierror.NotFound(page.Slug+" is not in this portal", "portal_page_not_found", nil)
		}
		homeCleared := portal.HomePageID != 0 && portal.HomePageID == page.ID
		if _, err := tx.Exec(ctx,
			`UPDATE page_portals
			    SET home_page_id = CASE WHEN home_page_id = $2 THEN NULL ELSE home_page_id END,
			        updated_at = now()
			  WHERE id = $1`, portal.ID, page.ID); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, entry(a, "remove_portal_page", page.ID, map[string]any{
			"portal_id":    portal.ID,
			"portal_slug":  portal.Slug,
			"page_slug":    page.Slug,
			"home_cleared": homeCleared,
		})); err != nil {
			return nil, err
		}
		return &PageRemoved{PortalID: portal.ID, PageSlug: page.Slug, Removed: true, HomeCleared: homeCleared}, nil
	})
}

// SetHome sets which of the portal's dashboards a partner lands on; a
// nil/Null/"" slug clears it. It must be a current member (400
// portal_page_not_found): a home page outside the portal is a link to a
// password wall. An unchanged home writes nothing.
func SetHome(ctx context.Context, d *db.DB, in SetHomeInput, a versions.Actor) (*HomeSet, error) {
	if err := AssertPortalAdmin(a); err != nil {
		return nil, err
	}
	id, err := NormalizeID(in.ID, "")
	if err != nil {
		return nil, err
	}
	clearing := isNullish(in.Slug) || in.Slug == ""
	var wanted string
	if !clearing {
		if wanted, err = versions.NormalizeSlug(in.Slug); err != nil {
			return nil, err
		}
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*HomeSet, error) {
		portal, err := lockPortal(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		var page *versions.LockedPage
		if !clearing {
			if page, err = versions.LockPage(ctx, tx, wanted); err != nil {
				return nil, err
			}
			tag, err := tx.Exec(ctx, `SELECT 1 FROM page_portal_members WHERE portal_id = $1 AND page_id = $2`, portal.ID, page.ID)
			if err != nil {
				return nil, err
			}
			if tag.RowsAffected() == 0 {
				return nil, apierror.BadRequest(page.Slug+" is not in this portal — add it first", "portal_page_not_found", nil)
			}
		}
		var newID jsx.ID
		var newSlug *string
		if page != nil {
			newID, newSlug = page.ID, &page.Slug
		}
		if (page != nil && portal.HomePageID != 0 && portal.HomePageID == page.ID) || (clearing && portal.HomePageID == 0) {
			return &HomeSet{Portal: HomePortal{Row: *portal, HomePageSlug: newSlug}}, nil
		}
		var row Row
		if err := tx.QueryRow(ctx,
			`UPDATE page_portals SET home_page_id = $2, updated_at = now() WHERE id = $1
			 RETURNING `+portalColumns, portal.ID, newID).Scan(row.scanArgs()...); err != nil {
			return nil, err
		}
		fromSlug, err := slugOfPage(ctx, tx, portal.HomePageID)
		if err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, entry(a, "set_portal_home", newID, map[string]any{
			"portal_id":   portal.ID,
			"portal_slug": portal.Slug,
			"from":        fromSlug,
			"to":          newSlug,
		})); err != nil {
			return nil, err
		}
		return &HomeSet{Portal: HomePortal{Row: row, HomePageSlug: newSlug}}, nil
	})
}

// Remove retires a portal. Soft delete, like a page: the row, its membership
// and its audit trail stay, and the slug and name free for reuse. It is also
// the kill switch for the credential, since every read filters
// `deleted_at IS NULL` — so retiring a portal ends every live session for it.
// Member pages are untouched.
func Remove(ctx context.Context, d *db.DB, id any, a versions.Actor) (*Retired, error) {
	if err := AssertPortalAdmin(a); err != nil {
		return nil, err
	}
	pid, err := NormalizeID(id, "")
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Retired, error) {
		portal, err := lockPortal(ctx, tx, pid)
		if err != nil {
			return nil, err
		}
		var memberCount int
		if err := tx.QueryRow(ctx,
			`SELECT count(*)::int AS member_count FROM page_portal_members WHERE portal_id = $1`, portal.ID).
			Scan(&memberCount); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE page_portals SET deleted_at = now(), updated_at = now() WHERE id = $1`, portal.ID); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, entry(a, "delete_portal", 0, map[string]any{
			"portal_id":    portal.ID,
			"slug":         portal.Slug,
			"name":         portal.Name,
			"member_count": memberCount,
		})); err != nil {
			return nil, err
		}
		return &Retired{ID: portal.ID, Slug: portal.Slug, Name: portal.Name, Deleted: true, MemberCount: memberCount}, nil
	})
}

// ── internal helpers (all operate on the transaction) ───────────────────────

func displayTitle(label *string, title, slug string) string {
	if label != nil && *label != "" {
		return *label
	}
	if title != "" {
		return title
	}
	return slug
}

// lockPortal is SELECT … FOR UPDATE on a live portal, the first statement of
// every mutation, so the row lock is held for the whole transaction.
func lockPortal(ctx context.Context, q db.Querier, id int64) (*Row, error) {
	r := &Row{}
	err := q.QueryRow(ctx, `SELECT `+portalColumns+` FROM page_portals
	  WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(r.scanArgs()...)
	if db.IsNoRows(err) {
		return nil, portalNotFound()
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// resolveMemberPage: the slug path goes through versions.LockPage, so a live
// page 404s with the same code and locks the same way as everywhere else. The
// id path deliberately does NOT filter deleted_at — it is the only way to
// reach a membership row whose page has been soft-deleted.
func resolveMemberPage(ctx context.Context, q db.Querier, t memberTarget) (*versions.LockedPage, error) {
	if t.slug != "" {
		return versions.LockPage(ctx, q, t.slug)
	}
	p := &versions.LockedPage{}
	err := q.QueryRow(ctx, `SELECT id, slug, title FROM pages WHERE id = $1 FOR UPDATE`, t.pageID).
		Scan(&p.ID, &p.Slug, &p.Title)
	if db.IsNoRows(err) {
		return nil, apierror.NotFound("page not found: "+strconv.FormatInt(t.pageID, 10), "page_not_found", nil)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func slugOfPage(ctx context.Context, q db.Querier, pageID jsx.ID) (*string, error) {
	if pageID == 0 {
		return nil, nil
	}
	var slug string
	err := q.QueryRow(ctx, `SELECT slug FROM pages WHERE id = $1`, pageID).Scan(&slug)
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &slug, nil
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return err != nil && errors.As(err, &pg) && pg.Code == "23505"
}

// uniqueViolation maps the two live-uniqueness indexes (both 23505; the
// constraint name says which) to their 409s, and passes anything else through.
func uniqueViolation(err error, slug, name string) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23505" {
		return err
	}
	switch pg.ConstraintName {
	case "page_portals_name_live_uidx":
		return apierror.Conflict("a portal is already named: "+name, "portal_name_exists", nil)
	case "page_portals_slug_live_uidx":
		return apierror.Conflict("portal already exists: "+slug, "portal_exists", nil)
	}
	return err
}
