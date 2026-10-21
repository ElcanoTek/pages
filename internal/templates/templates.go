// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package templates

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/preflight"
	"github.com/ElcanoTek/pages/internal/versions"
)

// NameRE mirrors the CHECK in migrations/015. No slashes: a template is a
// design, not a place in a hierarchy, and keeping it flat means a name can
// never be mistaken for a page slug in an upload row.
var NameRE = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*$`)

// MaxNameChars bounds a template name.
const MaxNameChars = 64

// Template revision sources. "cli" is a file-backed registration by an
// operator with shell access (`pages template`) — the path that needs no
// agent and no model output.
const (
	SourceAPI   = "api"
	SourceMCP   = "mcp"
	SourceAdmin = "admin"
	SourceCLI   = "cli"
)

// Sources is TEMPLATE_SOURCES, in its JavaScript order. The MCP output schema
// for a revision's source must stay in step with it.
var Sources = []string{SourceAPI, SourceMCP, SourceAdmin, SourceCLI}

// noSourceCoverage: a page created without data represents no source coverage
// yet. The epoch is the lowest possible floor, so the first real ingest can
// never be rejected as a regression however old the data it covers.
const noSourceCoverage = "1970-01-01T00:00:00Z"

// wrapTx lets tests observe or fail the statements a transaction runs; in
// production it is the transaction itself.
var wrapTx = func(tx pgx.Tx) db.Querier { return tx }

func inTx[T any](ctx context.Context, d *db.DB, fn func(q db.Querier) (T, error)) (T, error) {
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (T, error) { return fn(wrapTx(tx)) })
}

// NormalizeTemplateName validates and canonicalizes a template name (trimmed,
// lowercased). A non-string is 400 template_required; a bad shape 400
// bad_template_name.
func NormalizeTemplateName(name any) (string, error) {
	s, ok := name.(string)
	if !ok {
		return "", apierror.BadRequest("template is required", "template_required", nil)
	}
	s = jsstr.Trim(s)
	// JavaScript lowercases U+0130 to "i̇" (two code points), which the
	// pattern then rejects; Go's simple case mapping would make it "i".
	normalized := strings.ToLower(s)
	if strings.ContainsRune(s, 'İ') || !NameRE.MatchString(normalized) || len(normalized) > MaxNameChars {
		return "", apierror.BadRequest(
			fmt.Sprintf("template must be url-safe with no slashes (a-z 0-9 - _), at most %d characters", MaxNameChars),
			"bad_template_name", nil)
	}
	return normalized, nil
}

// ParseTemplateHTML is the contract check. Everything it enforces is already
// hardened for pages: 2020-12 self-containment, the ReDoS pattern screen, size
// ceilings, and "the payload currently in the document must satisfy its own
// schema" — applied here to both pairs.
func ParseTemplateHTML(html string) (*pagedata.Managed, error) {
	return pagedata.ParseManaged(html, pagedata.TemplateSpec)
}

// managedCheck is preflight's managed-block rule, judged on the bytes as they
// will be stored.
func managedCheck(h string) error {
	e, err := pagedata.EnsureConfigSchema(h)
	if err != nil {
		return err
	}
	_, err = pagedata.ParseManagedHTML(e.HTML)
	return err
}

// analyze is preflight.analyze(html, {renderMode: "themed"}).
func analyze(html string) preflight.Report {
	return preflight.Analyze(html, preflight.Options{RenderMode: "themed", ManagedCheck: managedCheck})
}

func parseObject(raw string) (*jsonv.Object, error) {
	v, err := jsonv.ParseString(raw)
	if err != nil {
		return nil, err
	}
	o, ok := v.(*jsonv.Object)
	if !ok {
		return nil, fmt.Errorf("templates: stored JSON is not an object")
	}
	return o, nil
}

func normalizeNote(note *string) *string {
	if note == nil {
		return nil
	}
	trimmed := jsstr.Trim(*note)
	if trimmed == "" {
		return nil
	}
	trimmed = jsstr.Prefix(trimmed, 500)
	return &trimmed
}

func auditEntry(a versions.Actor, action string, metadata any) audit.Entry {
	return audit.Entry{Actor: a.Actor, ActorType: a.ActorType, Action: action, TokenID: a.TokenID, IP: a.IP, Metadata: metadata}
}

func intPtr(n int) *int { return &n }

// asObject views a decoded JSON object (jsonv or a plain map) as *jsonv.Object.
func asObject(v any) *jsonv.Object {
	switch o := v.(type) {
	case *jsonv.Object:
		return o
	case map[string]any:
		return jsonv.FromMap(o)
	}
	return nil
}

// isNullish is JavaScript `v == null` under the versions input convention.
func isNullish(v any) bool { return v == nil || v == versions.Null }

// unNull maps versions.Null to the JSON null pagedata expects.
func unNull(v any) any {
	if v == versions.Null {
		return nil
	}
	return v
}

// ── registration ────────────────────────────────────────────────────────────

// RegisterInput is a registration. Title and Description nil keep the stored
// values (a new template gets ""); Source "" means "mcp".
type RegisterInput struct {
	Name        any
	HTML        string
	Title       *string
	Description *string
	Note        *string
	Source      string
}

const templateColumns = `id, name, title, description, current_version_id, created_at, updated_at`
const revisionColumns = `id, revision, content_sha256, config_schema_sha256, data_schema_sha256,
       (sample_data IS NOT NULL), author, source, note, created_at`

func scanTemplate(row pgx.Row, t *Template) error {
	return row.Scan(&t.ID, &t.Name, &t.Title, &t.Description, &t.CurrentVersionID, &t.CreatedAt, &t.UpdatedAt)
}

func scanRevision(row pgx.Row, r *Revision) error {
	return row.Scan(&r.VersionID, &r.Revision, &r.ContentSHA256, &r.ConfigSchemaSHA256, &r.DataSchemaSHA256,
		&r.HasSampleData, &r.Author, &r.Source, &r.Note, &r.CreatedAt)
}

// Register registers a template revision in its own transaction.
func Register(ctx context.Context, d *db.DB, in RegisterInput, a versions.Actor) (*RegisterResult, error) {
	return inTx(ctx, d, func(q db.Querier) (*RegisterResult, error) { return RegisterWithTx(ctx, q, in, a) })
}

// RegisterWithTx is transaction-aware so a staged upload can register inside
// the same transaction that verifies and consumes its bytes (see
// uploads.Deploy): an ambiguous commit is then safely retryable. q must be a
// transaction.
func RegisterWithTx(ctx context.Context, q db.Querier, in RegisterInput, a versions.Actor) (*RegisterResult, error) {
	name, err := NormalizeTemplateName(in.Name)
	if err != nil {
		return nil, err
	}
	if jsstr.Trim(in.HTML) == "" {
		return nil, apierror.BadRequest("html is required", "html_required", nil)
	}
	source := in.Source
	if source == "" {
		source = SourceMCP
	}
	if source != SourceAPI && source != SourceMCP && source != SourceAdmin && source != SourceCLI {
		return nil, apierror.BadRequest("bad source: "+source, "bad_source", nil)
	}
	parsed, err := ParseTemplateHTML(in.HTML)
	if err != nil {
		return nil, err
	}
	contentSHA := versions.SHA256(in.HTML)

	lockSQL := `SELECT ` + templateColumns + ` FROM page_templates WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`
	tmpl := &Template{}
	found := true
	if err := scanTemplate(q.QueryRow(ctx, lockSQL, name), tmpl); err != nil {
		if !db.IsNoRows(err) {
			return nil, err
		}
		found = false
	}
	created := false
	if !found {
		title, description := "", ""
		if in.Title != nil {
			title = *in.Title
		}
		if in.Description != nil {
			description = *in.Description
		}
		err := scanTemplate(q.QueryRow(ctx,
			`INSERT INTO page_templates (name, title, description)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (name) WHERE deleted_at IS NULL DO NOTHING
			 RETURNING `+templateColumns, name, title, description), tmpl)
		switch {
		case err == nil:
			created = true
		case db.IsNoRows(err):
			// A concurrent register committed while the INSERT waited on the
			// unique index. Take the row lock and continue as a new revision.
			if err := scanTemplate(q.QueryRow(ctx, lockSQL, name), tmpl); err != nil {
				if db.IsNoRows(err) {
					return nil, apierror.Conflict("template name changed concurrently: "+name, "template_race", nil)
				}
				return nil, err
			}
		default:
			return nil, err
		}
	}

	// An identical re-register is the same revision, not a new one. Scoped to
	// the newest revision so a genuine revert to older bytes still records a
	// revision — "current" must always be the most recently registered design.
	newest := &Revision{}
	hasNewest := true
	if err := scanRevision(q.QueryRow(ctx,
		`SELECT `+revisionColumns+` FROM page_template_versions
		  WHERE template_id = $1 ORDER BY revision DESC LIMIT 1`, tmpl.ID), newest); err != nil {
		if !db.IsNoRows(err) {
			return nil, err
		}
		hasNewest = false
	}
	deduped := hasNewest && newest.ContentSHA256 == contentSHA
	version := newest
	if !deduped {
		next := 1
		if hasNewest {
			next = newest.Revision + 1
		}
		var sample any
		if parsed.Example != nil {
			// Preview-only. pagedata deletes the block it came from on every
			// materialization, so this can never reach a page.
			sample = jsonv.Stringify(parsed.Example)
		}
		version = &Revision{}
		if err := scanRevision(q.QueryRow(ctx,
			`INSERT INTO page_template_versions
			   (template_id, revision, html, content_sha256, config_schema, data_schema,
			    config_schema_sha256, data_schema_sha256, reference_config, sample_data,
			    author, source, note)
			 VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, $9::jsonb, $10::jsonb, $11, $12, $13)
			 RETURNING `+revisionColumns,
			tmpl.ID, next, in.HTML, contentSHA, jsonv.Stringify(parsed.ConfigSchema), jsonv.Stringify(parsed.Schema),
			parsed.ConfigSchemaSHA256, parsed.SchemaSHA256, jsonv.Stringify(parsed.Config), sample,
			a.Actor, source, normalizeNote(in.Note)), version); err != nil {
			return nil, err
		}
	}

	// The pointer always names the newest revision. Pages pin the exact
	// revision they were built from, so moving it never touches anything
	// already deployed.
	metadataUpdated := !created && ((in.Title != nil && *in.Title != tmpl.Title) ||
		(in.Description != nil && *in.Description != tmpl.Description))
	updated := tmpl
	if !deduped || metadataUpdated {
		updated = &Template{}
		if err := scanTemplate(q.QueryRow(ctx,
			`UPDATE page_templates
			    SET current_version_id = $2,
			        title = COALESCE($3, title),
			        description = COALESCE($4, description),
			        updated_at = now()
			  WHERE id = $1
			RETURNING `+templateColumns, tmpl.ID, version.VersionID, in.Title, in.Description), updated); err != nil {
			return nil, err
		}
		action := "register_template"
		switch {
		case deduped:
			action = "update_template_metadata"
		case created:
			action = "create_template"
		}
		meta := jsonv.ObjectOf("template", name)
		if metadataUpdated {
			meta.Set("before", jsonv.ObjectOf("title", tmpl.Title, "description", tmpl.Description))
			meta.Set("after", jsonv.ObjectOf("title", updated.Title, "description", updated.Description))
		}
		meta.Set("template_version_id", version.VersionID.String())
		meta.Set("revision", float64(version.Revision))
		meta.Set("content_sha256", contentSHA)
		meta.Set("config_schema_sha256", parsed.ConfigSchemaSHA256)
		meta.Set("data_schema_sha256", parsed.SchemaSHA256)
		if err := audit.Write(ctx, q, auditEntry(a, action, meta)); err != nil {
			return nil, err
		}
	}
	t := *updated
	t.CurrentRevision = intPtr(version.Revision)
	return &RegisterResult{
		Created:         created,
		Deduped:         deduped,
		MetadataUpdated: metadataUpdated,
		Template:        t,
		Revision:        *version,
		ConfigSchema:    parsed.ConfigSchema,
		DataSchema:      parsed.Schema,
		ReferenceConfig: parsed.Config,
		HasSampleData:   parsed.Example != nil,
		Preflight:       analyze(in.HTML),
	}, nil
}

// ── validation without writing ──────────────────────────────────────────────

func errorInfo(err error, defaultCode string, withDetails bool) *ErrorInfo {
	e, ok := apierror.As(err)
	if !ok {
		return &ErrorInfo{Code: defaultCode, Message: err.Error()}
	}
	info := &ErrorInfo{Code: e.Code, Message: e.Message}
	if info.Code == "" {
		info.Code = defaultCode
	}
	if withDetails {
		info.Details = e.Details
	}
	return info
}

// ValidateHTML answers "is this our template format?", WITHOUT writing
// anything. name nil (or blank) skips the name check.
//
// The library's upload path calls this first so a human sees exactly what is
// wrong before anything is registered. It answers two different questions and
// keeps them apart, because they have different consequences:
//
//	contract  — does this satisfy the four-block template contract? A failure
//	            here means it cannot be registered at all.
//	preflight — does the design actually work in the sandbox+CSP it will be
//	            served under? Advisory for a page, but a template's findings
//	            are INHERITED by every page built from it, so the library shows
//	            them as blocking-by-default and makes the operator override.
//
// html is the raw input; a non-string is reported as html_required.
func ValidateHTML(html any, name any) ValidateReport {
	report := ValidateReport{}
	s, ok := html.(string)
	if !ok || jsstr.Trim(s) == "" {
		report.ContractError = &ErrorInfo{Code: "html_required", Message: "no HTML was supplied"}
		return report
	}
	report.Bytes = intPtr(len(s))

	if !isNullish(name) && jsstr.Trim(jsonv.ToString(name)) != "" {
		if n, err := NormalizeTemplateName(name); err != nil {
			report.NameError = errorInfo(err, "bad_template_name", false)
		} else {
			report.Name = &n
		}
	}

	parsed, err := ParseTemplateHTML(s)
	if err != nil {
		report.ContractError = errorInfo(err, "template_contract_invalid", true)
		// Still preflight it: an operator fixing the blocks wants to know about
		// the broken chart control in the same pass, not on the next upload.
		pf := analyze(s)
		report.Preflight = &pf
		return report
	}

	report.ContractOK = true
	report.ConfigSchema = parsed.ConfigSchema
	report.DataSchema = parsed.Schema
	report.ReferenceConfig = parsed.Config
	report.DataKeys = append([]string{}, parsed.Data().Keys()...)
	// A template that ships real rows would publish one campaign's numbers
	// into every page built from it until the first refresh.
	if rows, isArr := parsed.Data().Value("rows").([]any); isArr {
		empty := len(rows) == 0
		report.ShipsEmpty = &empty
	}
	// An optional example dataset is what lets the library show this design
	// populated. Report that it was found and validated against the data
	// schema; the rows themselves are not echoed back.
	hasSample := parsed.Example != nil
	report.HasSampleData = &hasSample
	if hasSample {
		report.SampleDataKeys = append([]string{}, parsed.Example.Keys()...)
	}
	// Values that are in the config AND still written into the design. Every
	// page built from this would show the hardcoded copy.
	report.HardcodedConfigValues = hardcodedConfigValues(s, parsed)
	pf := analyze(s)
	report.Preflight = &pf
	return report
}

// hardcodedConfigValues is the honest limit of promoting a page.
//
// Moving a value into #pages-config makes it vary per instance ONLY if the
// design reads it from there. A page that also has it written into its markup
// — <title>Acme</title>, a heading, a legend — keeps that copy, and every page
// built from the design will show it. That is invisible until a client sees
// another client's name, so it is reported: for each config STRING value, does
// the same text appear anywhere outside the config block?
//
// Advisory, never blocking. A false positive is possible and harmless (a value
// that genuinely belongs to the design), and the caller is told where to look
// rather than being second-guessed.
const (
	hardcodedMinChars    = 4
	hardcodedMaxFindings = 25
)

type candidate struct{ path, value string }

func collectStrings(value any, path string, out *[]candidate) {
	switch v := value.(type) {
	case string:
		if jsstr.UTF16Len(jsstr.Trim(v)) >= hardcodedMinChars {
			*out = append(*out, candidate{path, v})
		}
	case []any:
		for i, entry := range v {
			collectStrings(entry, fmt.Sprintf("%s[%d]", path, i), out)
		}
	case *jsonv.Object:
		v.Range(func(key string, entry any) bool {
			p := key
			if path != "" {
				p = path + "." + key
			}
			collectStrings(entry, p, out)
			return true
		})
	}
}

func hardcodedConfigValues(html string, parsed *pagedata.Managed) []HardcodedValue {
	findings := []HardcodedValue{}
	if parsed.ConfigBlock == nil || parsed.Config == nil {
		return findings
	}
	// Everything except the config block itself, so the value's own
	// declaration is not reported as a duplicate of itself.
	outside := html[:parsed.ConfigBlock.ElementStart] + html[parsed.ConfigBlock.ElementEnd:]
	var candidates []candidate
	collectStrings(parsed.Config, "", &candidates)
	seen := map[string]bool{}
	for _, c := range candidates {
		if len(findings) >= hardcodedMaxFindings {
			break
		}
		if seen[c.value] {
			continue
		}
		seen[c.value] = true
		occurrences := 0
		for rest := outside; ; {
			at := strings.Index(rest, c.value)
			if at < 0 {
				break
			}
			occurrences++
			if occurrences > 9 {
				break
			}
			rest = rest[at+len(c.value):]
		}
		if occurrences > 0 {
			findings = append(findings, HardcodedValue{Path: c.path, Value: c.value, Occurrences: occurrences})
		}
	}
	return findings
}

// ── promotion ───────────────────────────────────────────────────────────────

// PromoteInput is CreateFromPage's input. EmptyData must be a JSON object.
type PromoteInput struct {
	Slug                   any
	Name                   any
	EmptyData              any
	Title                  *string
	Description            *string
	Note                   *string
	ExampleFromCurrentData bool
}

// CreateFromPage promotes a page you already like into a reusable design.
//
// This is the flow that should have existed first: you build a dashboard, it
// is good, and you want the next client's version of it. Nothing is
// re-authored and NOTHING MOVES — the design is already on this server, so
// the promotion reads the published bytes in place. The alternative was
// pulling ~90 KB through the model and pushing it straight back.
//
// It requires only that the page already separates its per-instance values
// into #pages-config, which is the shape every page should be authored in
// anyway (a derived config schema means that costs no schema authoring). A
// page that hardcodes its identity is refused with the reason, because
// promoting it would produce a "template" whose every instance said the same
// client's name.
func CreateFromPage(ctx context.Context, d *db.DB, in PromoteInput, a versions.Actor) (*PromoteResult, error) {
	name, err := NormalizeTemplateName(in.Name)
	if err != nil {
		return nil, err
	}
	emptyData := asObject(in.EmptyData)
	if emptyData == nil {
		return nil, apierror.BadRequest("empty_data must be the object a page built from this shows before its first refresh", "empty_data_required", nil)
	}
	return inTx(ctx, d, func(q db.Querier) (*PromoteResult, error) {
		slug, err := versions.NormalizeSlug(in.Slug)
		if err != nil {
			return nil, err
		}
		var pageSlug, html string
		var versionID jsx.ID
		err = q.QueryRow(ctx,
			`SELECT p.slug, v.id AS version_id, v.html
			   FROM pages p
			   JOIN page_versions v ON v.id = p.published_version_id
			  WHERE p.slug = $1 AND p.deleted_at IS NULL`, slug).Scan(&pageSlug, &versionID, &html)
		if db.IsNoRows(err) {
			return nil, apierror.NotFound(fmt.Sprintf("no published version to promote for page: %s", jsonv.ToString(in.Slug)), "page_not_published", nil)
		}
		if err != nil {
			return nil, err
		}
		// The page's live data doubles as a ready-made example dataset — but it
		// is a real client's numbers, and a template is shared, so lifting it is
		// opt-in and the result says it happened.
		var exampleData any
		if in.ExampleFromCurrentData {
			m, err := pagedata.ParseManagedHTML(html)
			if err != nil {
				return nil, err
			}
			exampleData = m.Data()
		}
		assembled, err := pagedata.AssembleTemplate(html, emptyData, exampleData)
		if err != nil {
			return nil, err
		}
		result, err := RegisterWithTx(ctx, q, RegisterInput{Name: name, HTML: assembled.HTML, Title: in.Title,
			Description: in.Description, Note: in.Note, Source: SourceMCP}, a)
		if err != nil {
			return nil, err
		}
		reparsed, err := ParseTemplateHTML(assembled.HTML)
		if err != nil {
			return nil, err
		}
		return &PromoteResult{
			RegisterResult:         *result,
			FromPage:               pageSlug,
			FromVersionID:          versionID,
			ExampleFromCurrentData: exampleData != nil,
			HardcodedConfigValues:  hardcodedConfigValues(assembled.HTML, reparsed),
		}, nil
	})
}

// ── retirement ──────────────────────────────────────────────────────────────

// Remove retires a template and frees its name.
//
// This exists because registering is easy and a name is permanent otherwise:
// a typo'd template used to need SQL to undo, and the library now shows it,
// with a rendered preview, to everyone.
//
// Soft delete, like a page: the revisions stay, so pages that pinned one keep
// their provenance row and the deleted_at partial index frees the name for
// re-registration. Refused by default when pages were built from it, because
// that is nearly always the typo case being confused with a real retirement —
// force is how you say you meant a retirement. Those pages keep serving either
// way: they carry their own materialized HTML and never read the template at
// runtime.
func Remove(ctx context.Context, d *db.DB, template any, force bool, a versions.Actor) (*RemoveResult, error) {
	name, err := NormalizeTemplateName(template)
	if err != nil {
		return nil, err
	}
	return inTx(ctx, d, func(q db.Querier) (*RemoveResult, error) {
		var id jsx.ID
		err := q.QueryRow(ctx, `SELECT id FROM page_templates WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`, name).Scan(&id)
		if db.IsNoRows(err) {
			return nil, apierror.NotFound("template not found: "+name, "template_not_found", nil)
		}
		if err != nil {
			return nil, err
		}
		var built int
		if err := q.QueryRow(ctx,
			`SELECT count(DISTINCT pv.page_id) AS pages
			   FROM page_versions pv
			   JOIN page_template_versions tv ON tv.id = pv.template_version_id
			   JOIN pages p ON p.id = pv.page_id AND p.deleted_at IS NULL
			  WHERE tv.template_id = $1`, id).Scan(&built); err != nil {
			return nil, err
		}
		if built > 0 && !force {
			were := "s were"
			if built == 1 {
				were = " was"
			}
			return nil, apierror.Conflict(
				fmt.Sprintf("%d page%s built from %s; pass force to retire it anyway. ", built, were, name)+
					"Those pages keep serving, but they lose their design provenance and can no longer be re-rendered "+
					"from it. Call list_template_pages first to see which.",
				"template_has_pages", jsonv.ObjectOf("pages", float64(built)))
		}
		if _, err := q.Exec(ctx, `UPDATE page_templates SET deleted_at = now(), updated_at = now() WHERE id = $1`, id); err != nil {
			return nil, err
		}
		meta := jsonv.ObjectOf("template", name, "template_id", id.String(), "pages_built", float64(built), "forced", force)
		if err := audit.Write(ctx, q, auditEntry(a, "delete_template", meta)); err != nil {
			return nil, err
		}
		return &RemoveResult{Template: name, Deleted: true, PagesBuilt: built}, nil
	})
}
