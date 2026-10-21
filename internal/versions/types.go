// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/jsx"
)

// Actor is the audit/authority identity every mutation runs as — JavaScript's
// actorCtx. REST builds it from the bearer token (ActorType "agent", TokenID
// set), MCP from the authenticated tool context, the admin API from the
// signed-in staff session (ActorType "user"). Authority checks key on
// ActorType only: "user" is a human admin, "agent" an API/MCP token.
type Actor struct {
	Actor     string
	ActorType string // audit.User | audit.Agent | audit.System
	TokenID   jsx.ID
	IP        string
}

func (a Actor) entry(action string, pageID, versionID jsx.ID, metadata any) audit.Entry {
	return audit.Entry{
		Actor: a.Actor, ActorType: a.ActorType, Action: action,
		PageID: pageID, VersionID: versionID, TokenID: a.TokenID, IP: a.IP, Metadata: metadata,
	}
}

// Version is a version row without its html — the shape every list, deploy,
// publish, approve and rollback result carries. Key order matches the
// JavaScript rows.
type Version struct {
	ID            jsx.ID   `json:"id"`
	PageID        jsx.ID   `json:"page_id"`
	ContentSHA256 string   `json:"content_sha256"`
	Status        string   `json:"status"`
	RenderMode    string   `json:"render_mode"`
	Author        string   `json:"author"`
	Source        string   `json:"source"`
	Note          *string  `json:"note"`
	ReviewedBy    *string  `json:"reviewed_by"`
	ReviewedAt    jsx.Time `json:"reviewed_at"`
	CreatedAt     jsx.Time `json:"created_at"`
}

const versionColumns = `id, page_id, content_sha256, status, render_mode, author, source,
       note, reviewed_by, reviewed_at, created_at`

func (v *Version) scanArgs() []any {
	return []any{&v.ID, &v.PageID, &v.ContentSHA256, &v.Status, &v.RenderMode, &v.Author, &v.Source,
		&v.Note, &v.ReviewedBy, &v.ReviewedAt, &v.CreatedAt}
}

// VersionDetail is one version's full row, html included (getVersion). html
// sits second-after-page_id, as the JavaScript row did.
type VersionDetail struct {
	ID            jsx.ID   `json:"id"`
	PageID        jsx.ID   `json:"page_id"`
	HTML          string   `json:"html"`
	ContentSHA256 string   `json:"content_sha256"`
	Status        string   `json:"status"`
	RenderMode    string   `json:"render_mode"`
	Author        string   `json:"author"`
	Source        string   `json:"source"`
	Note          *string  `json:"note"`
	ReviewedBy    *string  `json:"reviewed_by"`
	ReviewedAt    jsx.Time `json:"reviewed_at"`
	CreatedAt     jsx.Time `json:"created_at"`
}

// Summary is the row without html ({html, ...version} in JavaScript).
func (v *VersionDetail) Summary() Version {
	return Version{
		ID: v.ID, PageID: v.PageID, ContentSHA256: v.ContentSHA256, Status: v.Status, RenderMode: v.RenderMode,
		Author: v.Author, Source: v.Source, Note: v.Note, ReviewedBy: v.ReviewedBy, ReviewedAt: v.ReviewedAt, CreatedAt: v.CreatedAt,
	}
}

// ListedVersion is a history row: published means pointer equality even
// during an admin takedown; live additionally requires the page not be
// disabled.
type ListedVersion struct {
	Version
	IsPublished bool `json:"is_published"`
	IsLive      bool `json:"is_live"`
}

// Page is getPage's page object.
type Page struct {
	ID                 jsx.ID   `json:"id"`
	Slug               string   `json:"slug"`
	Title              string   `json:"title"`
	ClientID           *string  `json:"client_id"`
	WorkspaceID        jsx.ID   `json:"workspace_id"`
	WorkspaceName      *string  `json:"workspace_name"`
	ThemeID            jsx.ID   `json:"theme_id"`
	ThemeName          string   `json:"theme_name"`
	RequireApproval    bool     `json:"require_approval"`
	Disabled           bool     `json:"disabled"`
	PublishedVersionID jsx.ID   `json:"published_version_id"`
	CreatedAt          jsx.Time `json:"created_at"`
	UpdatedAt          jsx.Time `json:"updated_at"`
	HasPassword        bool     `json:"has_password"`
}

// VersionNumbers are the admin index's page-local ordinals
// (ListPagesOptions.IncludeVersionNumbers). PublishedVersionNumber is null
// when nothing is published.
type VersionNumbers struct {
	VersionCount           int  `json:"version_count"`
	PublishedVersionNumber *int `json:"published_version_number"`
}

// PageSummary is one listPages row. The embedded *VersionNumbers is nil (and
// its keys absent) unless IncludeVersionNumbers was requested.
type PageSummary struct {
	Page
	IsLive bool `json:"is_live"`
	*VersionNumbers
	Freshness *Freshness `json:"freshness"`
}

// LastCheck is the page's last recorded deliberate refresh check.
type LastCheck struct {
	LastCheckAt         jsx.Time `json:"last_check_at"`
	LastCheckOutcome    *string  `json:"last_check_outcome"`
	LastCheckDetail     *string  `json:"last_check_detail"`
	LastCheckSourceAsOf jsx.Time `json:"last_check_source_as_of"`
}

// PageView is getPage's result. REST serializes it whole (plus urls), so the
// JavaScript key "lastCheck" is kept.
type PageView struct {
	Page      Page           `json:"page"`
	Published *VersionDetail `json:"published"`
	LastCheck LastCheck      `json:"lastCheck"`
}

// CreatedPage is createPage's result: the inserted row plus workspace_name.
type CreatedPage struct {
	ID                 jsx.ID   `json:"id"`
	Slug               string   `json:"slug"`
	Title              string   `json:"title"`
	ClientID           *string  `json:"client_id"`
	WorkspaceID        jsx.ID   `json:"workspace_id"`
	ThemeID            jsx.ID   `json:"theme_id"`
	RequireApproval    bool     `json:"require_approval"`
	Disabled           bool     `json:"disabled"`
	PublishedVersionID jsx.ID   `json:"published_version_id"`
	CreatedAt          jsx.Time `json:"created_at"`
	UpdatedAt          jsx.Time `json:"updated_at"`
	WorkspaceName      *string  `json:"workspace_name"`
}

// LockedPage is the row LockPage returns and holds FOR UPDATE. Title is ""
// for the rows createAndDeploy locks (it does not select it).
type LockedPage struct {
	ID                 jsx.ID `json:"id"`
	Slug               string `json:"slug"`
	Title              string `json:"title"`
	RequireApproval    bool   `json:"require_approval"`
	Disabled           bool   `json:"disabled"`
	PublishedVersionID jsx.ID `json:"published_version_id"`
}

// Theme is one listThemes row.
type Theme struct {
	ID          jsx.ID `json:"id"`
	Name        string `json:"name"`
	DefaultMode string `json:"default_mode"`
}

// ── small mutation results ─────────────────────────────────────────────────

// DisabledResult is setDisabled's result.
type DisabledResult struct {
	Slug     string `json:"slug"`
	Disabled bool   `json:"disabled"`
}

// ApprovalResult is setApproval's result.
type ApprovalResult struct {
	Slug            string `json:"slug"`
	RequireApproval bool   `json:"require_approval"`
}

// ThemeResult is setTheme's result.
type ThemeResult struct {
	Slug  string `json:"slug"`
	Theme string `json:"theme"`
}

// PasswordResult is setPassword's result.
type PasswordResult struct {
	Slug        string `json:"slug"`
	HasPassword bool   `json:"has_password"`
}

// RevealedPassword is revealPassword's result. Password is null when
// Available is false.
type RevealedPassword struct {
	Slug        string  `json:"slug"`
	HasPassword bool    `json:"has_password"`
	Available   bool    `json:"available"`
	Password    *string `json:"password"`
}

// TitleResult is setTitle's result.
type TitleResult struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

// DeleteResult is deletePage's result.
type DeleteResult struct {
	Slug    string `json:"slug"`
	Deleted bool   `json:"deleted"`
}

// RestoreResult is restorePage's result.
type RestoreResult struct {
	Slug     string `json:"slug"`
	Restored bool   `json:"restored"`
}

// RefreshCheckResult is recordRefreshCheck's result.
type RefreshCheckResult struct {
	Slug      string     `json:"slug"`
	Freshness *Freshness `json:"freshness"`
}
