// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package portals

import "github.com/ElcanoTek/pages/internal/jsx"

// The result shapes below reproduce the JavaScript objects key for key, in
// insertion order — which differs between verbs ({...row, home_page_slug}
// puts the slug after updated_at; the list query puts it before), so each
// shape has its own type.

// Row is a portal row as every mutation's RETURNING clause reads it.
type Row struct {
	ID         jsx.ID   `json:"id"`
	Slug       string   `json:"slug"`
	Name       string   `json:"name"`
	HomePageID jsx.ID   `json:"home_page_id"`
	CreatedAt  jsx.Time `json:"created_at"`
	UpdatedAt  jsx.Time `json:"updated_at"`
}

const portalColumns = "id, slug, name, home_page_id, created_at, updated_at"

func (r *Row) scanArgs() []any {
	return []any{&r.ID, &r.Slug, &r.Name, &r.HomePageID, &r.CreatedAt, &r.UpdatedAt}
}

// Summary is one List row, and the portal half of Get's Detail.
// HomePageSlug is null when there is no home page or it has been deleted;
// PageCount counts live member pages.
type Summary struct {
	ID           jsx.ID   `json:"id"`
	Slug         string   `json:"slug"`
	Name         string   `json:"name"`
	HomePageID   jsx.ID   `json:"home_page_id"`
	HomePageSlug *string  `json:"home_page_slug"`
	CreatedAt    jsx.Time `json:"created_at"`
	UpdatedAt    jsx.Time `json:"updated_at"`
	PageCount    int      `json:"page_count"`
}

// Member is one membership row in Get's Detail. Members whose page has been
// soft-deleted are included, flagged PageDeleted. SwitcherIsOwn and RenderMode
// are null when nothing is published.
type Member struct {
	PageID        jsx.ID   `json:"page_id"`
	Slug          string   `json:"slug"`
	Title         string   `json:"title"`
	Label         *string  `json:"label"`
	DisplayTitle  string   `json:"display_title"`
	SortOrder     int      `json:"sort_order"`
	AddedAt       jsx.Time `json:"added_at"`
	HasPassword   bool     `json:"has_password"`
	Disabled      bool     `json:"disabled"`
	Published     bool     `json:"published"`
	PageDeleted   bool     `json:"page_deleted"`
	ShowsSwitcher bool     `json:"shows_switcher"`
	SwitcherIsOwn *bool    `json:"switcher_is_own"`
	RenderMode    *string  `json:"render_mode"`
}

// LinkedPage is one LinkAudit.Missing entry.
type LinkedPage struct {
	PageID jsx.ID `json:"page_id"`
	Slug   string `json:"slug"`
	Title  string `json:"title"`
}

// LinkAudit reports same-host links in the home page's published HTML that
// resolve to live pages outside the membership. Scanned is false when there
// is no published home page or the audit could not run.
type LinkAudit struct {
	Scanned bool         `json:"scanned"`
	Missing []LinkedPage `json:"missing"`
}

// Detail is Get's result.
type Detail struct {
	Portal    Summary   `json:"portal"`
	Members   []Member  `json:"members"`
	LinkAudit LinkAudit `json:"link_audit"`
}

// PageMembership is one ListForPage row: a portal that exposes the page.
// IsHome is null when the portal has no home page.
type PageMembership struct {
	ID        jsx.ID  `json:"id"`
	Slug      string  `json:"slug"`
	Name      string  `json:"name"`
	Label     *string `json:"label"`
	SortOrder int     `json:"sort_order"`
	IsHome    *bool   `json:"is_home"`
}

// CreatedPortal is the portal in Create's result ({...row, home_page_slug:
// null, page_count: 0}).
type CreatedPortal struct {
	Row
	HomePageSlug *string `json:"home_page_slug"`
	PageCount    int     `json:"page_count"`
}

// Created is Create's result. Password is the plaintext credential; it is
// returned here and never written to the audit log.
type Created struct {
	Portal            CreatedPortal `json:"portal"`
	Password          string        `json:"password"`
	PasswordGenerated bool          `json:"password_generated"`
}

// Renamed is Rename's result.
type Renamed struct {
	Portal Row `json:"portal"`
}

// PasswordSet is SetPassword's result.
type PasswordSet struct {
	Portal            Row    `json:"portal"`
	Password          string `json:"password"`
	PasswordGenerated bool   `json:"password_generated"`
}

// RevealedPassword is RevealPassword's result. Password is null when
// Available is false (Pages holds no copy that opens).
type RevealedPassword struct {
	ID        jsx.ID  `json:"id"`
	Available bool    `json:"available"`
	Password  *string `json:"password"`
}

// MemberView is the member in AddPage/UpdatePage results: the membership
// row, then the page's slug/title and the partner-facing display title.
type MemberView struct {
	PageID       jsx.ID   `json:"page_id"`
	Label        *string  `json:"label"`
	SortOrder    int      `json:"sort_order"`
	AddedAt      jsx.Time `json:"added_at"`
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	DisplayTitle string   `json:"display_title"`
}

// PageAdded is AddPage's result. ReclassifiesStaffOnly reports that a
// staff-only page just became readable with the portal credential.
type PageAdded struct {
	Member                MemberView `json:"member"`
	PortalID              jsx.ID     `json:"portal_id"`
	ReclassifiesStaffOnly bool       `json:"reclassifies_staff_only"`
}

// PageUpdated is UpdatePage's result.
type PageUpdated struct {
	Member   MemberView `json:"member"`
	PortalID jsx.ID     `json:"portal_id"`
}

// PageRemoved is RemovePage's result.
type PageRemoved struct {
	PortalID    jsx.ID `json:"portal_id"`
	PageSlug    string `json:"page_slug"`
	Removed     bool   `json:"removed"`
	HomeCleared bool   `json:"home_cleared"`
}

// HomePortal is the portal in SetHome's result ({...row, home_page_slug}).
type HomePortal struct {
	Row
	HomePageSlug *string `json:"home_page_slug"`
}

// HomeSet is SetHome's result.
type HomeSet struct {
	Portal HomePortal `json:"portal"`
}

// Retired is Remove's result.
type Retired struct {
	ID          jsx.ID `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Deleted     bool   `json:"deleted"`
	MemberCount int    `json:"member_count"`
}

// ── inputs ──────────────────────────────────────────────────────────────────
//
// Fields typed `any` take the raw decoded JSON value, following the versions
// convention: nil = key omitted, versions.Null = explicit null.

// CreateInput is Create's input. Password nil/Null/"" ⇒ generated.
type CreateInput struct {
	Slug     any
	Name     any
	Password any
}

// RenameInput is Rename's input.
type RenameInput struct {
	ID   any
	Name any
}

// SetPasswordInput is SetPassword's input. Password nil/Null/"" ⇒ generated.
type SetPasswordInput struct {
	ID       any
	Password any
}

// AddPageInput is AddPage's input. Label nil/Null/"" ⇒ none; SortOrder
// nil/Null/"" ⇒ 0.
type AddPageInput struct {
	ID        any
	Slug      any
	Label     any
	SortOrder any
}

// UpdatePageInput is UpdatePage's input. Exactly one of Slug and PageID
// addresses the member. Label and SortOrder: nil (omitted) leaves the field
// alone; versions.Null clears a label (fall back to pages.title) or resets
// the order to 0. At least one must be present.
type UpdatePageInput struct {
	ID        any
	Slug      any
	PageID    any
	Label     any
	SortOrder any
}

// RemovePageInput is RemovePage's input; exactly one of Slug and PageID.
type RemovePageInput struct {
	ID     any
	Slug   any
	PageID any
}

// SetHomeInput is SetHome's input. Slug nil/Null/"" clears the home page.
type SetHomeInput struct {
	ID   any
	Slug any
}
