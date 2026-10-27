// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/render"
	"github.com/ElcanoTek/pages/internal/routens"
)

// SHA256 is the hex SHA-256 of text's UTF-8 bytes (a version's content_sha256).
func SHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Version sources.
const (
	SourceAPI   = "api"
	SourceMCP   = "mcp"
	SourceAdmin = "admin"
)

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// ── locking and authority ───────────────────────────────────────────────────

// LockPage is SELECT … FOR UPDATE on a live page, serializing writers for it.
// It is the first statement of every mutation, so the row lock is held for
// the whole transaction. 404 page_not_found.
func LockPage(ctx context.Context, q db.Querier, slug string) (*LockedPage, error) {
	s, err := NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	p := &LockedPage{}
	err = q.QueryRow(ctx,
		`SELECT id, slug, title, require_approval, disabled, published_version_id
		   FROM pages WHERE slug = $1 AND deleted_at IS NULL FOR UPDATE`, s).
		Scan(&p.ID, &p.Slug, &p.Title, &p.RequireApproval, &p.Disabled, &p.PublishedVersionID)
	if db.IsNoRows(err) {
		return nil, pageNotFound(s)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func lockVersion(ctx context.Context, q db.Querier, pageID, versionID jsx.ID) (*Version, error) {
	v := &Version{}
	err := q.QueryRow(ctx, `SELECT `+versionColumns+`
	   FROM page_versions WHERE id = $1 AND page_id = $2 FOR UPDATE`, versionID, pageID).Scan(v.scanArgs()...)
	if db.IsNoRows(err) {
		return nil, apierror.NotFound(fmt.Sprintf("version %s not found on this page", jsString(versionID)), "version_not_found", nil)
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

func idOrNil(id jsx.ID) any {
	if id == 0 {
		return nil
	}
	return id.String()
}

// AssertExpectedVersion is the optimistic-concurrency check: when expected is
// set (not nil/Null) and is not the page's current published_version_id → 409
// stale_version.
//
// A bare "published version changed" is a dead end: it tells the caller its
// write lost a race but not what to do next. It is also the guaranteed outcome
// of a host that stages writes for human approval, since expected_version is
// frozen when the card is staged. So name the version that overtook it and the
// one call that recovers. The "confirm first" clause is load-bearing: retrying
// blind with the new id is exactly how one agent silently clobbers another's
// deploy, which is what this check exists to prevent.
func AssertExpectedVersion(page *LockedPage, expected any) error {
	if isNullish(expected) || sameDBID(expected, page.PublishedVersionID) {
		return nil
	}
	exp := jsString(expected)
	cur := jsString(page.PublishedVersionID)
	return apierror.Conflict(
		fmt.Sprintf("published version changed (expected %s, is %s) — another write landed after you read version %s. "+
			"Reread the page, confirm version %s is not already the change you intended, and only then retry once with expected_version %s.",
			exp, cur, exp, cur, cur),
		"stale_version",
		jsonv.ObjectOf(
			"expected_version", exp,
			"published_version_id", idOrNil(page.PublishedVersionID),
			"retry_with_expected_version", idOrNil(page.PublishedVersionID),
		))
}

// AssertAgentMayMovePointer is the shared gate for every agent action that
// publishes, rolls back, or deletes a page. Humans (admin) may always. Agents
// are refused on two admin-controlled states:
//   - disabled — an admin takedown kill switch. Blocking DELETE here is what
//     stops the delete→recreate slug-reuse bypass of the takedown.
//   - require_approval — a human must approve every live change.
func AssertAgentMayMovePointer(page *LockedPage, a Actor) error {
	if a.ActorType == audit.User {
		return nil
	}
	if page.Disabled {
		return apierror.Forbidden(
			"this page has been disabled (taken down) by an admin — agents cannot publish, roll back, or delete it",
			"disabled_takedown", nil)
	}
	if page.RequireApproval {
		return apierror.Forbidden(
			"this page requires approval — agents may only deploy a pending version; a human must approve",
			"approval_required", nil)
	}
	return nil
}

// AssertAdminAction refuses a non-human actor with 403 admin_only (message
// "" ⇒ "this action is admin-only").
func AssertAdminAction(a Actor, message string) error {
	if a.ActorType != audit.User {
		if message == "" {
			message = "this action is admin-only"
		}
		return apierror.Forbidden(message, "admin_only", nil)
	}
	return nil
}

// assertWorkspaceOrganizer: workspace membership changes only organization
// metadata and is reversible, so authenticated agents share this bounded
// authority with human admins — deliberately separate from AssertAdminAction.
func assertWorkspaceOrganizer(a Actor) error {
	if a.ActorType != audit.User && a.ActorType != audit.Agent {
		return apierror.Forbidden("workspace organization requires an authenticated user or agent", "organizer_only", nil)
	}
	return nil
}

// movePointer is the single statement that changes what's "live". It
// validates the optimistic-concurrency token and writes the audit row inside
// the caller's transaction. note lands in the AUDIT metadata, never on the
// version: page_versions is append-only, so the log entry for the move is the
// only honest place to record why the pointer moved.
func movePointer(ctx context.Context, q db.Querier, page *LockedPage, versionID jsx.ID, expected any, a Actor, action string, note *string) error {
	if err := AssertExpectedVersion(page, expected); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `UPDATE pages SET published_version_id = $1, updated_at = now() WHERE id = $2`, versionID, page.ID); err != nil {
		return err
	}
	meta := map[string]any{"from": idOrNil(page.PublishedVersionID), "to": versionID.String()}
	if note != nil {
		meta["note"] = *note
	}
	return audit.Write(ctx, q, a.entry(action, page.ID, versionID, meta))
}

// ── prepare ─────────────────────────────────────────────────────────────────

// DataMetadata marks a managed-data write. Passing one to PrepareDeploy makes
// the deploy dedupe on (data, template, source, render mode) identity rather
// than raw bytes; Action names the audit action ("" ⇒ "data_update").
// PrepareDeploy replaces the hashes and stamps with those parsed from the
// document itself.
type DataMetadata struct {
	Action      string
	DataSHA     string
	SchemaSHA   string
	TemplateSHA string
	SourceAsOf  string
	RefreshedAt string
}

// TemplateBinding is immutable provenance: which template revision produced a
// version. TemplateName and Revision are optional (a carried binding has
// neither); they only reach the audit metadata.
type TemplateBinding struct {
	TemplateName      string
	TemplateVersionID jsx.ID
	Revision          *int
	ConfigSHA         string
}

// CarryTemplateBinding reuses a published version's provenance for the next
// version: a refresh or a config edit does not change which template revision
// produced the design. nil when the version had no binding.
func CarryTemplateBinding(templateVersionID jsx.ID, configSHA *string) *TemplateBinding {
	if templateVersionID == 0 {
		return nil
	}
	return &TemplateBinding{TemplateVersionID: templateVersionID, ConfigSHA: deref(configSHA)}
}

// DeployInput is a deploy request. See the package's loosely-typed-input
// convention (args.go) for the `any` fields.
type DeployInput struct {
	Slug string
	// HTML must be non-empty (400 html_required); a REST caller passes "" for
	// a non-string body value.
	HTML string
	// RenderMode is "themed" | "raw"; nil/Null ⇒ raw for a Bento deck, else
	// themed.
	RenderMode any
	Note       any // string ≤ 500 or nil/Null
	Author     any // falsy ⇒ the actor
	Source     string
	// Publish must be a bool; nil ⇒ false (Null ⇒ 400 bad_boolean).
	Publish         any
	ExpectedVersion any
	DataMetadata    *DataMetadata
	TemplateBinding *TemplateBinding
}

// Prepared is a validated, normalized deploy ready for DeployLocked.
type Prepared struct {
	Slug                  string
	HTML                  string
	ConfigSchemaGenerated bool
	RenderMode            string
	Note                  *string
	VersionAuthor         string
	Source                string
	Publish               bool
	ExpectedVersion       any
	ContentSHA            string
	DedupeByData          bool
	DataMetadata          *DataMetadata
	TemplateBinding       *TemplateBinding
	// CoverageGaps is the stored form of the document's coverage gaps
	// (coveragegaps.go), computed here, outside any transaction, so the
	// version row is written with them. "" leaves the column NULL, for a read
	// to fill.
	CoverageGaps string
}

// The write and read paths used to disagree about what a managed page is. A
// patch that broke the JSON inside #pages-data published happily and then
// dead-ended the managed-data toolchain on that page (and the live page
// rendered blank). A page in that state is broken no matter who looks at it,
// so refuse the write. page_not_data_managed / page_not_template_managed mean
// "an ordinary raw page", which must keep deploying; anything else means the
// author reached for the managed contract and got it wrong.
func assertManagedBlocksDeployable(html string) (*pagedata.Managed, error) {
	m, err := pagedata.ParseManagedHTML(html)
	if err == nil {
		return m, nil
	}
	e, ok := apierror.As(err)
	if !ok || e.Code == "" {
		return nil, err
	}
	if e.Code == "page_not_data_managed" || e.Code == "page_not_template_managed" {
		return nil, nil
	}
	return nil, apierror.Conflict(
		"refusing to publish: this document's Pages-managed blocks are not valid, so the page would render "+
			"blank and every managed-data tool would fail on it. "+e.Message,
		e.Code, e.Details)
}

func bentoRenderModeError() error {
	return apierror.BadRequest(
		`this document is a Bento deck (it carries a <script type="application/bento+json"> block), which Pages serves raw: `+
			`it is its own application with a complete design, so render_mode "themed" would splice Flag's head tags ahead of `+
			`the deck's own CSP and draw a Page menu over its toolbar. Omit render_mode or pass "raw".`,
		"bad_render_mode", nil)
}

// PrepareDeploy validates and normalizes a deploy outside any transaction.
// Every deploy path routes through it, so a page is normalized once: a derived
// #pages-config-schema is written in BEFORE the content hash, a stored
// #pages-nav copy and the deck host adaptation are stripped, and managed
// blocks must parse.
func PrepareDeploy(in DeployInput, a Actor) (*Prepared, error) {
	publish, err := boolArg(in.Publish, false, "publish")
	if err != nil {
		return nil, err
	}
	if in.HTML == "" {
		return nil, apierror.BadRequest("html is required", "html_required", nil)
	}
	// A Bento deck carries its own complete design, so it is raw unless the
	// caller says otherwise — and "otherwise" can only be a mistake.
	deck := render.IsBentoDeck(in.HTML)
	var mode string
	if isNullish(in.RenderMode) {
		mode = "themed"
		if deck {
			mode = "raw"
		}
	} else {
		s, ok := in.RenderMode.(string)
		if !ok || (s != "themed" && s != "raw") {
			return nil, apierror.BadRequest("bad render_mode: "+jsString(in.RenderMode), "bad_render_mode", nil)
		}
		mode = s
	}
	if deck && mode != "raw" {
		return nil, bentoRenderModeError()
	}
	source := in.Source
	if source == "" {
		source = SourceAPI
	}
	if source != SourceAPI && source != SourceMCP && source != SourceAdmin {
		return nil, apierror.BadRequest("bad source: "+source, "bad_source", nil)
	}
	note, err := normalizeNote(in.Note)
	if err != nil {
		return nil, err
	}
	normalized, err := pagedata.EnsureConfigSchema(in.HTML)
	if err != nil {
		return nil, err
	}
	// #pages-nav is injected at render time with the CURRENT sibling list of
	// whichever portal authorised the request; a stored copy would be a second
	// answer written by whoever authored the page. Likewise the deck host
	// adaptation: a file saved from the hosted editor carries it.
	html := render.StripDeckHostAdaptation(render.StripInjectedNav(normalized.HTML))
	managed, err := assertManagedBlocksDeployable(html)
	if err != nil {
		return nil, err
	}
	dedupeByData := in.DataMetadata != nil
	var dm *DataMetadata
	if managed != nil {
		action := "deploy"
		if in.DataMetadata != nil {
			action = in.DataMetadata.Action
			if action == "" {
				action = "data_update"
			}
		}
		dm = &DataMetadata{
			Action: action, DataSHA: managed.DataSHA256, SchemaSHA: managed.SchemaSHA256,
			TemplateSHA: managed.TemplateSHA256, SourceAsOf: managed.SourceAsOf(), RefreshedAt: managed.RefreshedAt(),
		}
	}
	if dm == nil {
		dedupeByData = false
	}
	gaps := pagedata.NoCoverageGaps()
	if managed != nil {
		gaps = pagedata.DetectCoverageGaps(managed.Data())
	}
	slug, err := NormalizeSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	author := a.Actor
	if jsTruthy(in.Author) {
		author, _ = pgText(in.Author)
	}
	return &Prepared{
		Slug: slug, HTML: html, ConfigSchemaGenerated: normalized.Generated, RenderMode: mode, Note: note,
		VersionAuthor: author, Source: source, Publish: publish, ExpectedVersion: in.ExpectedVersion,
		ContentSHA: SHA256(html), DedupeByData: dedupeByData, DataMetadata: dm, TemplateBinding: in.TemplateBinding,
		CoverageGaps: coverageGapsJSON(gaps),
	}, nil
}

// ── deploy ──────────────────────────────────────────────────────────────────

// DeployResult is a deploy's outcome. Live is the truthful serving state:
// published this call, or the dedupe matched the version the pointer already
// references, and the page is not disabled. ConfigSchemaGenerated is set (and
// serialized) only by the paths that accept raw html.
type DeployResult struct {
	Version               Version `json:"version"`
	Deduped               bool    `json:"deduped"`
	Published             bool    `json:"published"`
	Gated                 bool    `json:"gated"`
	Live                  bool    `json:"live"`
	ConfigSchemaGenerated bool    `json:"config_schema_generated,omitempty"`
}

// CreateDeployResult is createAndDeploy's result: {created, ...deploy}.
type CreateDeployResult struct {
	Created bool `json:"created"`
	DeployResult
}

// DeployLockedOptions tunes DeployLocked. DedupeVersionID restricts dedupe to
// that one version (deck saves: never dedupe a deliberate edit onto an older
// historical version).
type DeployLockedOptions struct {
	DedupeVersionID jsx.ID
}

// DeployLocked runs the version state machine for a prepared deploy on a page
// the caller has locked (LockPage) in q's transaction:
//   - expected_version is checked first, on EVERY deploy — publish:false and
//     approval-gated writes included;
//   - identical content dedupes onto the existing version (same render mode,
//     same template binding, never a rejected row; open pages skip gated-era
//     pending rows and gated pages skip open-era drafts);
//   - otherwise a new row is inserted: pending on a gated page, else draft;
//   - Publish on an open page approves it and moves the pointer (an exact
//     managed-data retry against the live version is a no-op). On a gated
//     page Publish is ignored: the version waits for a human.
func DeployLocked(ctx context.Context, q db.Querier, page *LockedPage, p *Prepared, a Actor, opts DeployLockedOptions) (*DeployResult, error) {
	// Optimistic concurrency, before anything is read or written. It used to
	// be enforced only inside movePointer, so publish:false and every write to
	// an approval-gated page silently dropped the caller's check — and the
	// gated page is where a human is trusting the review queue. It precedes
	// the takedown check too: a stale expected_version against a DISABLED page
	// answers 409 stale_version.
	if err := AssertExpectedVersion(page, p.ExpectedVersion); err != nil {
		return nil, err
	}
	var bindingID jsx.ID
	if p.TemplateBinding != nil {
		bindingID = p.TemplateBinding.TemplateVersionID
	}
	statusSkip := "AND status <> 'pending'"
	if page.RequireApproval {
		statusSkip = "AND status <> 'draft'"
	}
	var row pgx.Row
	if p.DedupeByData {
		dm := p.DataMetadata
		row = q.QueryRow(ctx, `SELECT `+versionColumns+`
		   FROM page_versions
		  WHERE page_id = $1
		    AND data_sha256 = $2
		    AND data_template_sha256 = $3
		    AND source_as_of = $4::timestamptz
		    AND render_mode = $5
		    AND ($6::bigint IS NULL OR id = $6)
		    AND template_version_id IS NOT DISTINCT FROM $7::bigint
		    AND status <> 'rejected'
		    `+statusSkip+`
		  ORDER BY id DESC LIMIT 1`,
			page.ID, dm.DataSHA, dm.TemplateSHA, dm.SourceAsOf, p.RenderMode, opts.DedupeVersionID, bindingID)
	} else {
		row = q.QueryRow(ctx, `SELECT `+versionColumns+`
		   FROM page_versions
		  WHERE page_id = $1 AND content_sha256 = $2
		    AND render_mode = $3
		    AND ($4::bigint IS NULL OR id = $4)
		    AND template_version_id IS NOT DISTINCT FROM $5::bigint
		    AND status <> 'rejected'
		    `+statusSkip+`
		  ORDER BY id DESC LIMIT 1`,
			page.ID, p.ContentSHA, p.RenderMode, opts.DedupeVersionID, bindingID)
	}
	version := &Version{}
	deduped := true
	if err := row.Scan(version.scanArgs()...); err != nil {
		if !db.IsNoRows(err) {
			return nil, err
		}
		deduped = false
	}

	if !deduped {
		status := "draft"
		if page.RequireApproval {
			status = "pending"
		}
		dm, tb := p.DataMetadata, p.TemplateBinding
		var dataSHA, templateSHA, sourceAsOf, refreshedAt, configSHA any
		if dm != nil {
			dataSHA, templateSHA, sourceAsOf, refreshedAt = dm.DataSHA, dm.TemplateSHA, dm.SourceAsOf, dm.RefreshedAt
		}
		if tb != nil {
			configSHA = tb.ConfigSHA
		}
		var gaps any
		if p.CoverageGaps != "" {
			gaps = p.CoverageGaps
		}
		err := q.QueryRow(ctx,
			`INSERT INTO page_versions
			   (page_id, html, content_sha256, status, render_mode, author, source, note,
			    data_sha256, data_template_sha256, source_as_of, refreshed_at,
			    template_version_id, config_sha256, coverage_gaps)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::timestamptz, $12::timestamptz, $13, $14, $15::text::jsonb)
			 RETURNING `+versionColumns,
			page.ID, p.HTML, p.ContentSHA, status, p.RenderMode, p.VersionAuthor, p.Source, p.Note,
			dataSHA, templateSHA, sourceAsOf, refreshedAt, bindingID, configSHA, gaps).Scan(version.scanArgs()...)
		if err != nil {
			return nil, err
		}
		// A managed write names what it actually did (data refresh, config
		// edit, template build) rather than being flattened into one label.
		action := "deploy"
		meta := map[string]any{"status": status, "render_mode": p.RenderMode, "deduped": false}
		if dm != nil {
			action = dm.Action
			if action == "" {
				action = "data_update"
			}
			meta["data_sha256"] = dm.DataSHA
			meta["schema_sha256"] = dm.SchemaSHA
			meta["template_sha256"] = dm.TemplateSHA
			meta["source_as_of"] = dm.SourceAsOf
			meta["refreshed_at"] = dm.RefreshedAt
		}
		if tb != nil {
			if tb.TemplateName != "" {
				meta["template"] = tb.TemplateName
			}
			meta["template_version_id"] = tb.TemplateVersionID.String()
			if tb.Revision != nil {
				meta["template_revision"] = *tb.Revision
			}
			meta["config_sha256"] = tb.ConfigSHA
		}
		if err := audit.Write(ctx, q, a.entry(action, page.ID, version.ID, meta)); err != nil {
			return nil, err
		}
	}

	// Fast-path publish on open pages only. On approval-gated pages publish is
	// silently ignored (the version waits in the pending queue for a human).
	publishedNow := false
	if p.Publish && !page.RequireApproval {
		if err := AssertAgentMayMovePointer(page, a); err != nil {
			return nil, err
		}
		// The pointer may only ever reference an `approved` version. rejected
		// is terminal — the dedupe never matches one; defense in depth.
		if version.Status == "rejected" {
			return nil, apierror.Conflict("cannot publish a rejected version; deploy new content", "rejected_terminal", nil)
		}
		if version.Status != "approved" {
			if _, err := q.Exec(ctx, `UPDATE page_versions SET status = 'approved' WHERE id = $1`, version.ID); err != nil {
				return nil, err
			}
			version.Status = "approved"
		}
		// An exact data retry against the already-live immutable version is a
		// semantic no-op: no pointer/audit write. Note the dedupe can land on
		// an OLDER version (it keys on identity, not the pointer); publishing
		// that moves the pointer backward, which is the content a byte-supplying
		// caller asked for. Callers whose contract is "retrying is a no-op"
		// (templates.createPage) scope their own idempotency check first.
		if p.DataMetadata == nil || page.PublishedVersionID != version.ID {
			if err := movePointer(ctx, q, page, version.ID, p.ExpectedVersion, a, "publish", nil); err != nil {
				return nil, err
			}
			publishedNow = true
		}
	}
	live := !page.Disabled && (publishedNow || (page.PublishedVersionID != 0 && page.PublishedVersionID == version.ID))
	return &DeployResult{Version: *version, Deduped: deduped, Published: publishedNow, Gated: page.RequireApproval, Live: live}, nil
}

// Deploy is the "make/update the dashboard" call: insert a new version (or
// dedupe), optionally publishing it on an open page.
func Deploy(ctx context.Context, d *db.DB, in DeployInput, a Actor) (*DeployResult, error) {
	p, err := PrepareDeploy(in, a)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*DeployResult, error) {
		page, err := LockPage(ctx, tx, p.Slug)
		if err != nil {
			return nil, err
		}
		r, err := DeployLocked(ctx, tx, page, p, a, DeployLockedOptions{})
		if err != nil {
			return nil, err
		}
		r.ConfigSchemaGenerated = p.ConfigSchemaGenerated
		return r, nil
	})
}

// ── deck saves ──────────────────────────────────────────────────────────────

// SaveDeckInput is a hosted Bento editor save.
type SaveDeckInput struct {
	Slug        string
	PageID      jsx.ID // the page the edit session was minted for
	BaseVersion string // X-Pages-Base-Version, a decimal id
	HTML        string
}

var baseVersionRE = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

var maxBigint = big.NewInt(9223372036854775807)

// SaveDeck saves a deck edit. Deck saves continue from the latest editor
// snapshot, not the live pointer: the same page lock and state machine as
// every authoring path, but never deduped onto an older historical version.
func SaveDeck(ctx context.Context, d *db.DB, in SaveDeckInput, a Actor) (*DeployResult, error) {
	if a.ActorType != audit.User {
		return nil, apierror.Forbidden("a deck edit session requires a staff actor", "admin_only", nil)
	}
	if !baseVersionRE.MatchString(in.BaseVersion) || bigOver(in.BaseVersion) {
		return nil, apierror.BadRequest("reopen the deck from the admin before saving: a valid X-Pages-Base-Version is required", "deck_base_version_required", nil)
	}
	p, err := PrepareDeploy(DeployInput{
		Slug: in.Slug, HTML: in.HTML, RenderMode: "raw", Note: "Saved from the Bento editor", Source: SourceAdmin, Publish: false,
	}, a)
	if err != nil {
		return nil, err
	}
	base, _ := jsx.ParseID(in.BaseVersion)
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*DeployResult, error) {
		page, err := LockPage(ctx, tx, p.Slug)
		if err != nil {
			return nil, err
		}
		if page.ID != in.PageID || page.Disabled {
			return nil, apierror.Forbidden("this edit session is no longer available for this page", "deck_session_unavailable", nil)
		}
		var latestID jsx.ID
		var latestHTML, latestSHA, latestMode, latestStatus string
		err = tx.QueryRow(ctx,
			`SELECT id, html, content_sha256, render_mode, status FROM page_versions
			  WHERE page_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, page.ID).
			Scan(&latestID, &latestHTML, &latestSHA, &latestMode, &latestStatus)
		found := err == nil
		if err != nil && !db.IsNoRows(err) {
			return nil, err
		}
		if !found || !render.IsBentoDeck(latestHTML) || !render.IsBentoDeck(p.HTML) {
			return nil, apierror.Forbidden("this page is no longer a deck — reopen it from the admin", "deck_session_unavailable", nil)
		}
		var one int
		baseErr := tx.QueryRow(ctx, `SELECT 1 FROM page_versions WHERE page_id = $1 AND id = $2`, page.ID, base).Scan(&one)
		if baseErr != nil && !db.IsNoRows(baseErr) {
			return nil, baseErr
		}
		unusable := "pending"
		if page.RequireApproval {
			unusable = "draft"
		}
		sameSnapshot := latestSHA == p.ContentSHA && latestMode == p.RenderMode &&
			latestStatus != "rejected" && latestStatus != unusable
		if baseErr != nil || (latestID != base && !sameSnapshot) {
			return nil, apierror.Conflict(
				"a newer deck version was saved — reopen the editor from the admin and reconcile your changes before saving again",
				"stale_deck_version",
				jsonv.ObjectOf("base_version_id", in.BaseVersion, "latest_version_id", latestID.String()))
		}
		return DeployLocked(ctx, tx, page, p, a, DeployLockedOptions{DedupeVersionID: latestID})
	})
}

func bigOver(s string) bool {
	n, ok := new(big.Int).SetString(s, 10)
	return !ok || n.Cmp(maxBigint) > 0
}

// ── create-or-deploy ────────────────────────────────────────────────────────

// CreateAndDeployInput is MCP's create-or-update fast path: a deploy that
// creates the page (with Title/ClientID/WorkspaceID/RequireApproval) when it is missing.
type CreateAndDeployInput struct {
	DeployInput
	Title           any // nil ⇒ ""
	ClientID        any // nil/Null ⇒ NULL
	WorkspaceID     any // nil/Null ⇒ NULL
	RequireApproval any // bool; nil ⇒ false
}

// CreateAndDeployOptions: AssertExistingPage, when set, runs with the final
// page row locked whenever the page already existed (including after an
// INSERT loses a concurrent create), so a create-only caller's missing-row
// check cannot turn a rejected replacement into an ordinary deploy.
type CreateAndDeployOptions struct {
	AssertExistingPage func(ctx context.Context, page *LockedPage) error
}

// CreateAndDeploy runs CreateAndDeployWithTx in its own transaction: a failed
// version insert/publish cannot strand an empty page, and concurrent callers
// converge through the partial live-slug unique index.
func CreateAndDeploy(ctx context.Context, d *db.DB, in CreateAndDeployInput, a Actor) (*CreateDeployResult, error) {
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*CreateDeployResult, error) {
		return CreateAndDeployWithTx(ctx, tx, in, a, CreateAndDeployOptions{})
	})
}

// CreateAndDeployWithTx is the transaction-aware variant used by staged
// uploads and template builds: the verified upload consumption, immutable
// version, pointer move, and audit row commit together, so an ambiguous
// commit is safely retryable. q must be a transaction.
func CreateAndDeployWithTx(ctx context.Context, q db.Querier, in CreateAndDeployInput, a Actor, opts CreateAndDeployOptions) (*CreateDeployResult, error) {
	requireApproval, err := boolArg(in.RequireApproval, false, "require_approval")
	if err != nil {
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
	p, err := PrepareDeploy(in.DeployInput, a)
	if err != nil {
		return nil, err
	}
	lockSQL := `SELECT id, slug, require_approval, disabled, published_version_id
	   FROM pages WHERE slug = $1 AND deleted_at IS NULL FOR UPDATE`
	scanPage := func(row pgx.Row) (*LockedPage, error) {
		pg := &LockedPage{}
		err := row.Scan(&pg.ID, &pg.Slug, &pg.RequireApproval, &pg.Disabled, &pg.PublishedVersionID)
		if db.IsNoRows(err) {
			return nil, nil
		}
		return pg, err
	}
	page, err := scanPage(q.QueryRow(ctx, lockSQL, p.Slug))
	if err != nil {
		return nil, err
	}
	created := false
	if page == nil {
		if err := routens.AssertSlugNotReserved(p.Slug); err != nil {
			return nil, err
		}
		var wsArg any
		if workspaceID != 0 {
			var name string
			err := q.QueryRow(ctx, `SELECT name FROM workspaces WHERE id = $1 FOR KEY SHARE`, int64(workspaceID)).Scan(&name)
			if db.IsNoRows(err) {
				return nil, apierror.NotFound("workspace not found", "workspace_not_found", nil)
			}
			if err != nil {
				return nil, err
			}
			wsArg = int64(workspaceID)
		}
		title := in.Title
		if title == nil {
			title = ""
		}
		titleText, titleOK := pgText(title)
		clientText, clientOK := pgText(in.ClientID)
		page, err = scanPage(q.QueryRow(ctx,
			`INSERT INTO pages (slug, title, client_id, workspace_id, require_approval)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (slug) WHERE deleted_at IS NULL DO NOTHING
			 RETURNING id, slug, require_approval, disabled, published_version_id`,
			p.Slug, nullableText(titleText, titleOK), nullableText(clientText, clientOK), wsArg, requireApproval))
		if err != nil {
			return nil, err
		}
		if page != nil {
			if !isNullish(p.ExpectedVersion) {
				return nil, apierror.Conflict("expected_version cannot be used when creating a missing page", "stale_version", nil)
			}
			created = true
			var meta any
			if workspaceID != 0 {
				meta = map[string]any{"workspace_id": workspaceID}
			}
			if err := audit.Write(ctx, q, a.entry("create_page", page.ID, 0, meta)); err != nil {
				return nil, err
			}
		} else {
			// A concurrent create committed while INSERT waited on the unique
			// index. Take the normal page lock before checking the contract.
			page, err = scanPage(q.QueryRow(ctx, lockSQL, p.Slug))
			if err != nil {
				return nil, err
			}
			if page == nil {
				return nil, apierror.Conflict("slug changed concurrently: "+p.Slug, "slug_race", nil)
			}
		}
	}
	if !created && opts.AssertExistingPage != nil {
		if err := opts.AssertExistingPage(ctx, page); err != nil {
			return nil, err
		}
	}
	r, err := DeployLocked(ctx, q, page, p, a, DeployLockedOptions{})
	if err != nil {
		return nil, err
	}
	r.ConfigSchemaGenerated = p.ConfigSchemaGenerated
	return &CreateDeployResult{Created: created, DeployResult: *r}, nil
}

func nullableText(s string, ok bool) any {
	if !ok {
		return nil
	}
	return s
}

// ── managed data ────────────────────────────────────────────────────────────

// DataUpdateInput is an update_page_data write. Data, SourceAsOf and Expect
// are parsed JSON values (jsonv: *jsonv.Object, []any, float64, …).
type DataUpdateInput struct {
	Slug            string
	Data            any
	SourceAsOf      any
	ExpectedVersion any // required (400 expected_version_required)
	Publish         any // bool; nil ⇒ true
	Note            any
	Expect          any
	Now             time.Time // zero ⇒ time.Now()
}

// DataUpdateResult is updatePageData's result.
type DataUpdateResult struct {
	DeployResult
	Schema         *jsonv.Object      `json:"schema"`
	Envelope       *jsonv.Object      `json:"envelope"`
	DataSHA256     string             `json:"data_sha256"`
	SchemaSHA256   string             `json:"schema_sha256"`
	TemplateSHA256 string             `json:"template_sha256"`
	DataProfile    *pagedata.Profile  `json:"data_profile"`
	DataWarnings   []pagedata.Warning `json:"data_warnings"`
}

func nullToNil(v any) any {
	if v == Null {
		return nil
	}
	return v
}

// UpdatePageData runs UpdatePageDataWithTx in its own transaction.
func UpdatePageData(ctx context.Context, d *db.DB, in DataUpdateInput, a Actor) (*DataUpdateResult, error) {
	// The same argument checks UpdatePageDataWithTx makes first, without
	// opening a transaction for a request that cannot succeed.
	if _, err := NormalizeSlug(in.Slug); err != nil {
		return nil, err
	}
	if isNullish(in.ExpectedVersion) {
		return nil, apierror.BadRequest("expected_version is required for data updates", "expected_version_required", nil)
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*DataUpdateResult, error) {
		return UpdatePageDataWithTx(ctx, tx, in, a)
	})
}

// UpdatePageDataWithTx serializes writers on the page, uses the currently
// published version as the immutable layout/schema template, validates and
// reconciles the structured data, then enters the ordinary version/publish
// state machine. expected_version is mandatory even for publish:false so
// canaries cannot be built from stale layout bytes. Both the inline and the
// staged-upload paths run this exact body; q must be a transaction.
func UpdatePageDataWithTx(ctx context.Context, q db.Querier, in DataUpdateInput, a Actor) (*DataUpdateResult, error) {
	slug, err := NormalizeSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	if isNullish(in.ExpectedVersion) {
		return nil, apierror.BadRequest("expected_version is required for data updates", "expected_version_required", nil)
	}
	publish := in.Publish
	if publish == nil {
		publish = true
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	page, err := LockPage(ctx, q, slug)
	if err != nil {
		return nil, err
	}
	if err := AssertExpectedVersion(page, in.ExpectedVersion); err != nil {
		return nil, err
	}
	notManaged := apierror.Conflict("page has no published template to manage", "page_not_data_managed", nil)
	if page.PublishedVersionID == 0 {
		return nil, notManaged
	}
	var tplHTML, tplMode string
	var tplBinding jsx.ID
	var tplConfigSHA *string
	err = q.QueryRow(ctx,
		`SELECT html, render_mode, template_version_id, config_sha256
		   FROM page_versions WHERE id = $1 AND page_id = $2`, page.PublishedVersionID, page.ID).
		Scan(&tplHTML, &tplMode, &tplBinding, &tplConfigSHA)
	if db.IsNoRows(err) {
		return nil, apierror.Conflict("published template is unavailable", "page_not_data_managed", nil)
	}
	if err != nil {
		return nil, err
	}
	managed, err := pagedata.ParseManagedHTML(tplHTML)
	if err != nil {
		return nil, err
	}
	data, expect := nullToNil(in.Data), nullToNil(in.Expect)

	// Profile the incoming payload and reconcile it against whatever the
	// caller said it would contain BEFORE anything is written: a refresh that
	// fails its own arithmetic must not reach a client.
	previousProfile := pagedata.ProfileData(managed.Data())
	dataProfile := pagedata.ProfileData(data)
	if err := pagedata.AssertExpectedProfile(dataProfile, expect, ""); err != nil {
		return nil, err
	}
	nextSHA, err := pagedata.SemanticHash(data)
	if err != nil {
		return nil, err
	}
	warnings := pagedata.CompareDataProfiles(previousProfile, dataProfile, pagedata.CompareOptions{
		Expected: expect, PreviousDataSHA: managed.DataSHA256, NextDataSHA: nextSHA,
	})
	materialized, err := pagedata.Materialize(managed, data, nullToNil(in.SourceAsOf), now)
	if err != nil {
		return nil, err
	}
	// Source coverage is monotonic across every non-rejected data version,
	// not just the live pointer.
	floor, err := SourceCoverageFloor(ctx, q, page.ID, managed.SourceAsOf())
	if err != nil {
		return nil, err
	}
	newSource, _ := materialized.Envelope.Value("source_as_of").(string)
	if floor != "" {
		if ns, fl := ParseTime(newSource), ParseTime(floor); !ns.IsZero() && ns.Before(fl) {
			return nil, apierror.Conflict(
				fmt.Sprintf("source_as_of cannot move backward (current coverage is %s)", floor),
				"source_regression", jsonv.ObjectOf("current_source_as_of", floor))
		}
	}
	refreshed, _ := materialized.Envelope.Value("refreshed_at").(string)
	p, err := PrepareDeploy(DeployInput{
		Slug: slug, HTML: materialized.HTML, RenderMode: tplMode, Note: in.Note, Source: SourceMCP,
		Publish: publish, ExpectedVersion: in.ExpectedVersion,
		DataMetadata: &DataMetadata{
			DataSHA: materialized.DataSHA256, SchemaSHA: materialized.SchemaSHA256, TemplateSHA: materialized.TemplateSHA256,
			SourceAsOf: newSource, RefreshedAt: refreshed,
		},
		// Provenance survives a refresh: a template-built page stays
		// attributable to the revision that produced its design.
		TemplateBinding: CarryTemplateBinding(tplBinding, tplConfigSHA),
	}, a)
	if err != nil {
		return nil, err
	}
	r, err := DeployLocked(ctx, q, page, p, a, DeployLockedOptions{})
	if err != nil {
		return nil, err
	}
	stored, err := storedManaged(ctx, q, r.Version.ID, page.ID)
	if err != nil {
		return nil, err
	}
	// A dedupe already says "the exact data and source coverage were already
	// live", and data_unchanged's message ("a version was still created")
	// would be untrue there: drop the stale-refresh pair in that case.
	if r.Deduped {
		kept := []pagedata.Warning{}
		for _, w := range warnings {
			if w.Code != "data_unchanged" && w.Code != "coverage_did_not_advance" {
				kept = append(kept, w)
			}
		}
		warnings = kept
	}
	if warnings == nil {
		warnings = []pagedata.Warning{}
	}
	return &DataUpdateResult{
		DeployResult: *r, Schema: stored.Schema, Envelope: stored.Envelope,
		DataSHA256: stored.DataSHA256, SchemaSHA256: stored.SchemaSHA256, TemplateSHA256: stored.TemplateSHA256,
		DataProfile: dataProfile, DataWarnings: warnings,
	}, nil
}

// storedManaged re-reads the version actually stored (a dedupe returns an
// older row's bytes, refreshed_at included).
func storedManaged(ctx context.Context, q db.Querier, versionID, pageID jsx.ID) (*pagedata.Managed, error) {
	var html string
	if err := q.QueryRow(ctx, `SELECT html FROM page_versions WHERE id = $1 AND page_id = $2`, versionID, pageID).Scan(&html); err != nil {
		return nil, err
	}
	return pagedata.ParseManagedHTML(html)
}

// ConfigUpdateInput is an update_page_config write.
type ConfigUpdateInput struct {
	Slug            string
	Config          any // parsed JSON (jsonv); complete, it replaces
	ExpectedVersion any // required
	Publish         any // bool; nil ⇒ true
	Note            any
}

// ConfigUpdateResult is updatePageConfig's result.
type ConfigUpdateResult struct {
	DeployResult
	ConfigSchema       *jsonv.Object `json:"config_schema"`
	Config             *jsonv.Object `json:"config"`
	ConfigSHA256       string        `json:"config_sha256"`
	ConfigSchemaSHA256 string        `json:"config_schema_sha256"`
	Envelope           *jsonv.Object `json:"envelope"`
	DataSHA256         string        `json:"data_sha256"`
	TemplateSHA256     string        `json:"template_sha256"`
}

// UpdatePageConfig replaces ONLY the config block of the published version:
// the data block is left byte-for-byte alone (so editing a campaign's
// identity can never disturb its numbers) and source coverage is inherited
// unchanged. Otherwise structurally UpdatePageData.
func UpdatePageConfig(ctx context.Context, d *db.DB, in ConfigUpdateInput, a Actor) (*ConfigUpdateResult, error) {
	slug, err := NormalizeSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	if isNullish(in.ExpectedVersion) {
		return nil, apierror.BadRequest("expected_version is required for config updates", "expected_version_required", nil)
	}
	publish := in.Publish
	if publish == nil {
		publish = true
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*ConfigUpdateResult, error) {
		page, err := LockPage(ctx, tx, slug)
		if err != nil {
			return nil, err
		}
		if err := AssertExpectedVersion(page, in.ExpectedVersion); err != nil {
			return nil, err
		}
		if page.PublishedVersionID == 0 {
			return nil, apierror.Conflict("page has no published version to update", "update_page_not_published", nil)
		}
		var html, mode string
		var bindingID jsx.ID
		var configSHA *string
		err = tx.QueryRow(ctx,
			`SELECT html, render_mode, template_version_id, config_sha256
			   FROM page_versions WHERE id = $1 AND page_id = $2`, page.PublishedVersionID, page.ID).
			Scan(&html, &mode, &bindingID, &configSHA)
		if db.IsNoRows(err) {
			return nil, apierror.Conflict("published version is unavailable", "update_page_not_published", nil)
		}
		if err != nil {
			return nil, err
		}
		managed, err := parseConfigManaged(html)
		if err != nil {
			return nil, err
		}
		update := pagedata.BlockUpdate{}
		if in.Config != nil {
			update.Config, update.SetConfig = nullToNil(in.Config), true
		}
		m, err := pagedata.MaterializeBlocks(managed, update, pagedata.MaterializeOptions{})
		if err != nil {
			return nil, err
		}
		binding := CarryTemplateBinding(bindingID, configSHA)
		if binding != nil {
			binding.ConfigSHA = m.ConfigSHA256
		}
		source, _ := m.Envelope.Value("source_as_of").(string)
		refreshed, _ := m.Envelope.Value("refreshed_at").(string)
		// The identity moves because config is part of the template a later
		// refresh pours rows into, so this cannot silently dedupe onto the
		// pre-edit version. Source coverage is carried across verbatim.
		p, err := PrepareDeploy(DeployInput{
			Slug: slug, HTML: m.HTML, RenderMode: mode, Note: in.Note, Source: SourceMCP,
			Publish: publish, ExpectedVersion: in.ExpectedVersion,
			DataMetadata: &DataMetadata{
				Action: "config_update", DataSHA: m.DataSHA256, SchemaSHA: m.SchemaSHA256, TemplateSHA: m.TemplateSHA256,
				SourceAsOf: source, RefreshedAt: refreshed,
			},
			TemplateBinding: binding,
		}, a)
		if err != nil {
			return nil, err
		}
		r, err := DeployLocked(ctx, tx, page, p, a, DeployLockedOptions{})
		if err != nil {
			return nil, err
		}
		stored, err := storedManaged(ctx, tx, r.Version.ID, page.ID)
		if err != nil {
			return nil, err
		}
		return &ConfigUpdateResult{
			DeployResult: *r, ConfigSchema: stored.ConfigSchema, Config: stored.Config,
			ConfigSHA256: stored.ConfigSHA256, ConfigSchemaSHA256: stored.ConfigSchemaSHA256,
			Envelope: stored.Envelope, DataSHA256: stored.DataSHA256, TemplateSHA256: stored.TemplateSHA256,
		}, nil
	})
}
