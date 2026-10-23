// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package updateprompts is read-only prompt preparation for generic MCP
// clients (prepare_dashboard_update). Pages does not own a scheduler or an
// agent runtime: it describes the safe, exact-slug workflow and lets the
// caller either execute it once or install it as the prompt of a recurring
// task. A recurring prompt describes one run; the task that runs it owns the
// cadence.
//
// The prompts are text agents read, so they are byte-identical to the
// JavaScript implementation's (lib/update-prompts.js); the testdata goldens
// were recorded from it.
package updateprompts

import (
	"context"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/versions"
)

// PrepareInput is Prepare's input. Instructions and Sources are decoded JSON
// (validated here). UpdateType "" means "auto"; Publish nil means true.
type PrepareInput struct {
	Slug         any
	Instructions any
	Recurring    bool
	UpdateType   string
	Publish      *bool
	Sources      any
	// AllowUnboundRecurring exempts a caller from the recurring-bindings gate.
	// Only the legacy configure_page_refresh alias sets it: those clients send
	// a `workflow` blob, and when its entries omit mcp_server there is no
	// binding to lift and no way for that client to supply one. Refusing them
	// would break a compatibility path rather than improve a prompt — their
	// source detail still travels inside the serialized workflow contract. It
	// also keeps the full roster (see RequirementOptions.KeepFullRoster).
	AllowUnboundRecurring bool
}

// Result is prepare_dashboard_update's result, in JavaScript key order.
type Result struct {
	Page         versions.Page `json:"page"`
	Mode         string        `json:"mode"`
	Recurring    bool          `json:"recurring"`
	Prompt       string        `json:"prompt"`
	PromptSHA256 string        `json:"prompt_sha256"`
	SchemaSHA256 *string       `json:"schema_sha256"`
	// Sources echoes the parsed bindings so the caller can see exactly what
	// Pages rendered into the prompt rather than trusting its own request
	// shape (null when none).
	Sources []Source `json:"sources"`
	// InstructionsRemoved is the scheduling and prompt-creation prose dropped
	// from a recurring request, verbatim, so the caller can see the task text
	// was changed and how.
	InstructionsRemoved []string `json:"instructions_removed"`
	// ExecutionRequirements is what a run of this prompt needs, in a shape a
	// scheduler can check before it accepts the task instead of discovering it
	// at dispatch.
	ExecutionRequirements Requirements `json:"execution_requirements"`
	LiveVersionID         string       `json:"live_version_id"`
	PageIsLive            bool         `json:"page_is_live"`
	NextStep              string       `json:"next_step"`
}

// ScheduleHandoffStep closes a recurring next_step. Pages judges a page's
// refresh health only against a schedule someone told it about, and the
// agent installing the task is the one that knows the cadence and who asked.
const ScheduleHandoffStep = " Once the task is installed, call set_page_refresh_schedule with this slug, the task's cadence as a five-field UTC cron and the requester's email as owner, so Pages can flag the page when a run is missed or blocked."

// reader is the page state Prepare reads; tests substitute it.
type reader interface {
	getPage(ctx context.Context, slug string) (*versions.PageView, error)
	getPageData(ctx context.Context, slug string) (*versions.PageDataView, error)
	binding(ctx context.Context, slug string) (*templates.Binding, error)
}

type dbReader struct{ q db.Querier }

func (r dbReader) getPage(ctx context.Context, slug string) (*versions.PageView, error) {
	return versions.GetPage(ctx, r.q, slug)
}

func (r dbReader) getPageData(ctx context.Context, slug string) (*versions.PageDataView, error) {
	return versions.GetPageData(ctx, r.q, slug)
}

func (r dbReader) binding(ctx context.Context, slug string) (*templates.Binding, error) {
	return templates.PageTemplateBinding(ctx, r.q, slug)
}

// Prepare builds the exact-slug update prompt for a published dashboard.
func Prepare(ctx context.Context, q db.Querier, in PrepareInput) (*Result, error) {
	return prepare(ctx, dbReader{q}, in)
}

func prepare(ctx context.Context, r reader, in PrepareInput) (*Result, error) {
	slug, err := versions.NormalizeSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	instructions, err := NormalizeInstructions(in.Instructions)
	if err != nil {
		return nil, err
	}
	sources, err := NormalizeSources(in.Sources)
	if err != nil {
		return nil, err
	}
	removed := []string{}
	if in.Recurring {
		t := TaskOnlyInstructions(instructions)
		instructions, removed = t.Text, t.Removed
	}
	updateType := in.UpdateType
	if updateType == "" {
		updateType = "auto"
	}
	if updateType != "auto" && updateType != "data" && updateType != "layout" {
		return nil, apierror.BadRequest("update_type must be auto, data, or layout", "update_type_invalid", nil)
	}
	if in.Recurring && updateType == "layout" {
		return nil, apierror.BadRequest("recurring updates must be data-only", "recurring_layout_forbidden", nil)
	}
	// A recurring prompt is executed unattended, weeks later, by an agent that
	// shares no context with this conversation — and it writes to a live
	// client-visible dashboard. Prose is not a good enough handoff for that:
	// with `sources` omitted the prompt can only tell the executing agent to
	// re-derive its bindings from USER REQUEST, and against a real workspace
	// that is a guess. An exact-filename search for one report returns 107
	// hits across daily partitions, and two near-identically-named sibling
	// folders hold two different campaigns. A one-time update keeps the old
	// leniency — a human is watching that one.
	if in.Recurring && sources == nil && !in.AllowUnboundRecurring {
		return nil, apierror.BadRequest(
			"recurring updates require `sources`: bind each input to its MCP server (source_id + mcp_server, "+
				"plus path/partition when the data is a date-partitioned folder). An unattended run cannot safely "+
				"re-derive bindings from prose.",
			"update_sources_required", nil)
	}
	publish := in.Publish == nil || *in.Publish

	pv, err := r.getPage(ctx, slug)
	if err != nil {
		return nil, err
	}
	if pv.Published == nil || pv.Page.PublishedVersionID == 0 {
		return nil, apierror.Conflict("dashboard must have a published version before it can be updated", "update_page_not_published", nil)
	}
	liveVersionID := pv.Page.PublishedVersionID.String()
	managed, err := r.getPageData(ctx, slug)
	if err != nil {
		if e, ok := apierror.As(err); !ok || e.Code != "page_not_data_managed" {
			return nil, err
		}
		managed = nil
	}
	// A template-built page has a second contract, and a different safe answer
	// for anything that is not a data refresh.
	built, err := r.binding(ctx, slug)
	if err != nil {
		if _, ok := apierror.As(err); !ok {
			return nil, err
		}
		built = nil
	}

	var mode, prompt string
	dataOnly := in.Recurring || updateType == "data"
	switch {
	case dataOnly && managed == nil:
		mode = "migration_required"
		prompt = MigrationPrompt(MigrationInput{Slug: slug, Instructions: instructions, LiveVersionID: liveVersionID, Publish: publish, Recurring: in.Recurring, Sources: sources})
	case dataOnly:
		mode = "managed_data"
		prompt = ManagedPrompt(ManagedInput{Slug: slug, Instructions: instructions, SchemaSHA256: managed.SchemaSHA256, Publish: publish,
			Recurring: in.Recurring, Sources: sources, KeepFullRoster: in.AllowUnboundRecurring})
	case built != nil:
		mode = "managed_template"
		prompt = TemplatePrompt(TemplateInput{Slug: slug, Instructions: instructions, Template: built.Template, Revision: built.Revision,
			ConfigSchemaSHA256: built.ConfigSchemaSHA256, LiveVersionID: liveVersionID, Publish: publish})
	case updateType == "layout" || managed == nil:
		mode = "full_page"
		prompt = FullPagePrompt(FullPageInput{Slug: slug, Instructions: instructions, LiveVersionID: liveVersionID, Publish: publish, Sources: sources})
	default:
		mode = "adaptive"
		prompt = AdaptivePrompt(AdaptiveInput{Slug: slug, Instructions: instructions, SchemaSHA256: managed.SchemaSHA256, LiveVersionID: liveVersionID, Publish: publish, Sources: sources})
	}

	if len(prompt) > 90000 {
		return nil, apierror.BadRequest("prepared dashboard update prompt is too large", "update_prompt_too_large", nil)
	}
	var schemaSHA *string
	if managed != nil {
		s := managed.SchemaSHA256
		schemaSHA = &s
	}
	nextStep := "Follow this prompt now in the current conversation. Pages has not changed the dashboard yet."
	switch {
	case mode == "migration_required" && publish:
		nextStep = "Run this migration prompt once; after the verified migration is live, call prepare_dashboard_update again for the actual update prompt. Pages has not changed the dashboard."
	case mode == "migration_required":
		nextStep = "Run this migration prompt once and leave its version unpublished. The data-only follow-up depends on separately authorized publication of the migrated contract. Pages has not changed the dashboard."
	case in.Recurring:
		nextStep = "Use prompt verbatim, including its EXECUTION REQUIREMENTS line, as the prompt of the recurring task; set the cadence on the task itself, not in the prompt. Pages has not scheduled or executed anything." +
			ScheduleHandoffStep
	}
	return &Result{
		Page:                  pv.Page,
		Mode:                  mode,
		Recurring:             in.Recurring,
		Prompt:                prompt,
		PromptSHA256:          versions.SHA256(prompt),
		SchemaSHA256:          schemaSHA,
		Sources:               sources,
		InstructionsRemoved:   removed,
		ExecutionRequirements: ExecutionRequirements(sources, mode, RequirementOptions{Slug: slug, Recurring: in.Recurring, KeepFullRoster: in.AllowUnboundRecurring}),
		LiveVersionID:         liveVersionID,
		PageIsLive:            !pv.Page.Disabled,
		NextStep:              nextStep,
	}, nil
}
