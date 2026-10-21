// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package templates is one stored design, many pages (lib/templates.js).
//
// A template is a complete, self-validating managed page: it carries both
// managed pairs (see package pagedata), its #pages-config holds a real
// reference config, and its #pages-data holds the empty-state envelope. That
// is what lets registration validate a template against its own schemas
// instead of trusting the uploader, and it means a template can be rendered
// and inspected on its own before any client page exists.
//
// The point is cost. Building a second dashboard from a known design used to
// mean transmitting the whole design again — for a 62 KB campaign dashboard,
// ~21k output tokens of base64, or a hand-edited copy of the file. With a
// template stored server-side, the same page costs the config that actually
// differs (kilobytes) and get_template answers "what does this design need?"
// with two schemas instead of 62 KB of HTML.
//
// This package deliberately owns no rendering policy beyond block
// substitution: there is no placeholder or template language anywhere.
// Untrusted config and data can only ever land inside a JSON script block,
// escaped by pagedata, so they cannot become markup.
//
// Page writes compose the versions package's transaction pieces (LockPage,
// PrepareDeploy, DeployLocked, CreateAndDeployWithTx), so a template build is
// an ordinary version under the one state machine. Errors are *apierror.Error
// with the JavaScript messages and codes; database errors pass through (map
// them with apierror.FromDB).
package templates

import (
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/preflight"
	"github.com/ElcanoTek/pages/internal/versions"
)

// Template is a template row as every read returns it.
type Template struct {
	ID               jsx.ID   `json:"id"`
	Name             string   `json:"name"`
	Title            string   `json:"title"`
	Description      string   `json:"description"`
	CurrentRevision  *int     `json:"current_revision"`
	CurrentVersionID jsx.ID   `json:"current_version_id"`
	CreatedAt        jsx.Time `json:"created_at"`
	UpdatedAt        jsx.Time `json:"updated_at"`
}

// Revision is one template revision (revisionRow). HasSampleData says
// whether, not what: an example dataset is kilobytes of rows and the callers
// that matter (agents reading get_template) are the ones paying for context.
type Revision struct {
	VersionID          jsx.ID   `json:"version_id"`
	Revision           int      `json:"revision"`
	ContentSHA256      string   `json:"content_sha256"`
	ConfigSchemaSHA256 string   `json:"config_schema_sha256"`
	DataSchemaSHA256   string   `json:"data_schema_sha256"`
	Author             string   `json:"author"`
	Source             string   `json:"source"`
	Note               *string  `json:"note"`
	HasSampleData      bool     `json:"has_sample_data"`
	CreatedAt          jsx.Time `json:"created_at"`
}

// ListedRevision is a revisions() row.
type ListedRevision struct {
	Revision
	IsCurrent bool `json:"is_current"`
}

// RegisterResult is Register's result.
type RegisterResult struct {
	Created         bool             `json:"created"`
	Deduped         bool             `json:"deduped"`
	MetadataUpdated bool             `json:"metadata_updated"`
	Template        Template         `json:"template"`
	Revision        Revision         `json:"revision"`
	ConfigSchema    *jsonv.Object    `json:"config_schema"`
	DataSchema      *jsonv.Object    `json:"data_schema"`
	ReferenceConfig *jsonv.Object    `json:"reference_config"`
	HasSampleData   bool             `json:"has_sample_data"`
	Preflight       preflight.Report `json:"preflight"`
}

// HardcodedValue is a config string still written into the design.
type HardcodedValue struct {
	Path        string `json:"path"`
	Value       string `json:"value"`
	Occurrences int    `json:"occurrences"`
}

// PromoteResult is CreateFromPage's result.
type PromoteResult struct {
	RegisterResult
	FromPage               string           `json:"from_page"`
	FromVersionID          jsx.ID           `json:"from_version_id"`
	ExampleFromCurrentData bool             `json:"example_from_current_data"`
	HardcodedConfigValues  []HardcodedValue `json:"hardcoded_config_values"`
}

// ErrorInfo is a reported (not thrown) error in a ValidateReport.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// ValidateReport is ValidateHTML's report. Every field is present; the ones
// that do not apply are null.
type ValidateReport struct {
	Name                  *string           `json:"name"`
	NameError             *ErrorInfo        `json:"name_error"`
	ContractOK            bool              `json:"contract_ok"`
	ContractError         *ErrorInfo        `json:"contract_error"`
	ConfigSchema          *jsonv.Object     `json:"config_schema"`
	DataSchema            *jsonv.Object     `json:"data_schema"`
	ReferenceConfig       *jsonv.Object     `json:"reference_config"`
	DataKeys              []string          `json:"data_keys"`
	ShipsEmpty            *bool             `json:"ships_empty"`
	HasSampleData         *bool             `json:"has_sample_data"`
	SampleDataKeys        []string          `json:"sample_data_keys"`
	HardcodedConfigValues []HardcodedValue  `json:"hardcoded_config_values"`
	Bytes                 *int              `json:"bytes"`
	Preflight             *preflight.Report `json:"preflight"`
}

// GetResult is Get's result. HTML is present only when requested.
type GetResult struct {
	Template        Template      `json:"template"`
	Revision        Revision      `json:"revision"`
	ConfigSchema    *jsonv.Object `json:"config_schema"`
	DataSchema      *jsonv.Object `json:"data_schema"`
	ReferenceConfig *jsonv.Object `json:"reference_config"`
	HTML            *string       `json:"html,omitempty"`
}

// RevisionHTML is GetRevisionHTML's result: the bytes for a preview, with the
// revision's identity so the caller can bind a signed token to exactly these
// bytes.
type RevisionHTML struct {
	Template          string `json:"template"`
	TemplateVersionID jsx.ID `json:"template_version_id"`
	Revision          int    `json:"revision"`
	ContentSHA256     string `json:"content_sha256"`
	// HasSampleData lets the caller minting the token say which of the two
	// things the preview will show before it is opened.
	HasSampleData bool   `json:"has_sample_data"`
	HTML          string `json:"html"`
}

// Preview is PreviewBytes's result.
type Preview struct {
	Template       string `json:"template"`
	Revision       int    `json:"revision"`
	HTML           string `json:"html"`
	UsedSampleData bool   `json:"used_sample_data"`
}

// ListedTemplate is one List row. page_count is every live page whose
// history touches this design, serving_count only those whose PUBLISHED
// version still comes from it; the difference is the drift.
type ListedTemplate struct {
	Template
	ConfigSchemaSHA256 *string `json:"config_schema_sha256"`
	DataSchemaSHA256   *string `json:"data_schema_sha256"`
	PageCount          int     `json:"page_count"`
	ServingCount       int     `json:"serving_count"`
	DriftedCount       int     `json:"drifted_count"`
}

// ListResult is List's result.
type ListResult struct {
	Templates []ListedTemplate `json:"templates"`
}

// RevisionsResult is Revisions's result.
type RevisionsResult struct {
	Template  string           `json:"template"`
	Revisions []ListedRevision `json:"revisions"`
}

// RemoveResult is Remove's result.
type RemoveResult struct {
	Template   string `json:"template"`
	Deleted    bool   `json:"deleted"`
	PagesBuilt int    `json:"pages_built"`
}

// CreatePageResult is CreatePage's result: the create-or-deploy result plus
// the build's identity.
type CreatePageResult struct {
	versions.CreateDeployResult
	Template           string        `json:"template"`
	TemplateRevision   int           `json:"template_revision"`
	TemplateVersionID  jsx.ID        `json:"template_version_id"`
	Config             *jsonv.Object `json:"config"`
	ConfigSHA256       string        `json:"config_sha256"`
	ConfigSchemaSHA256 string        `json:"config_schema_sha256"`
	Envelope           *jsonv.Object `json:"envelope"`
	DataSHA256         string        `json:"data_sha256"`
	SchemaSHA256       string        `json:"schema_sha256"`
	TemplateSHA256     string        `json:"template_sha256"`
	HTML               string        `json:"html"`
}

// RerenderResult is RerenderPage's result. FromRevision is null when the page
// was drifted: it was serving no revision of any design.
type RerenderResult struct {
	versions.DeployResult
	Template          string        `json:"template"`
	FromRevision      *int          `json:"from_revision"`
	Reattached        bool          `json:"reattached"`
	TemplateRevision  int           `json:"template_revision"`
	TemplateVersionID jsx.ID        `json:"template_version_id"`
	Config            *jsonv.Object `json:"config"`
	ConfigSHA256      string        `json:"config_sha256"`
	Envelope          *jsonv.Object `json:"envelope"`
	DataSHA256        string        `json:"data_sha256"`
	TemplateSHA256    string        `json:"template_sha256"`
	HTML              string        `json:"html"`
}

// Binding is which template revision produced the page currently being
// served (PageTemplateBinding).
type Binding struct {
	Template           string  `json:"template"`
	Revision           int     `json:"revision"`
	TemplateVersionID  jsx.ID  `json:"template_version_id"`
	ConfigSchemaSHA256 string  `json:"config_schema_sha256"`
	DataSchemaSHA256   string  `json:"data_schema_sha256"`
	ConfigSHA256       *string `json:"config_sha256"`
	IsCurrentRevision  bool    `json:"is_current_revision"`
	CurrentRevision    *int    `json:"current_revision"`
}

// TemplatePage is one ListTemplatePages row.
type TemplatePage struct {
	Slug          string `json:"slug"`
	Title         string `json:"title"`
	LiveVersionID jsx.ID `json:"live_version_id"`
	// Revision is the revision a viewer is served, null when drifted.
	Revision *int `json:"revision"`
	// Behind is about the DESIGN a viewer is currently served, so it is
	// measured on the published version only — and a drifted page is not
	// "behind" the current revision, it is off the design entirely.
	Behind       bool    `json:"behind"`
	Drifted      bool    `json:"drifted"`
	LastRevision *int    `json:"last_revision"`
	PageIsLive   bool    `json:"page_is_live"`
	ConfigSHA256 *string `json:"config_sha256"`
}

// TemplatePages is ListTemplatePages's result.
type TemplatePages struct {
	Template        string         `json:"template"`
	CurrentRevision int            `json:"current_revision"`
	ServingCount    int            `json:"serving_count"`
	DriftedCount    int            `json:"drifted_count"`
	Pages           []TemplatePage `json:"pages"`
}
