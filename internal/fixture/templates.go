// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/auth/wire"
)

// initialTemplates is a template the library can list, inspect and preview.
// Only the fields the screen reads — the fixture stands in for the API, not
// for the templates package.
func initialTemplates() *list {
	return newList(o(
		"id", "1", "name", "nwm-campaign-dashboard", "title", "NWM Campaign Dashboard",
		"description", "Per-campaign delivery, revenue, margin and KPI pacing.",
		"current_revision", 2, "current_version_id", "2", "created_at", NOW, "updated_at", NOW,
		"config_schema_sha256", strings.Repeat("a", 64), "data_schema_sha256", strings.Repeat("b", 64), "page_count", 2,
		"has_sample_data", true,
	))
}

func schema(required, prop, typ string) obj {
	return o(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"type", "object",
		"required", newList(required),
		"properties", o(prop, o("type", typ)),
	)
}

func revisionRow(versionID string, revision int, content string, author, source string, note any, sample, current bool) obj {
	return o("version_id", versionID, "revision", revision, "content_sha256", strings.Repeat(content, 64),
		"config_schema_sha256", strings.Repeat("a", 64), "data_schema_sha256", strings.Repeat("b", 64),
		"author", author, "source", source, "note", note, "created_at", NOW, "has_sample_data", sample, "is_current", current)
}

func builtPage(slug, title, live string, revision int, behind bool, sha string) obj {
	return o("slug", slug, "title", title, "live_version_id", live, "revision", revision, "behind", behind,
		"page_is_live", true, "config_sha256", strings.Repeat(sha, 64))
}

func templateDetail(template obj) obj {
	return o(
		"template", template,
		"revision", o(
			"version_id", get(template, "current_version_id"), "revision", get(template, "current_revision"),
			"content_sha256", strings.Repeat("f", 64), "config_schema_sha256", get(template, "config_schema_sha256"),
			"data_schema_sha256", get(template, "data_schema_sha256"), "author", "pages-cli:qa", "source", "cli",
			"note", "fixture revision", "created_at", NOW, "has_sample_data", get(template, "has_sample_data"),
		),
		"config_schema", schema("campaign", "campaign", "string"),
		"data_schema", schema("rows", "rows", "array"),
		"reference_config", o("campaign", "Reference Campaign", "accountCode", "ACCT00000"),
		"revisions", newList(
			revisionRow("2", 2, "f", "pages-cli:qa", "cli", "chart fix", true, true),
			revisionRow("1", 1, "0", "qa-admin@elcanotek.com", "admin", nil, false, false),
		),
		"pages", newList(
			builtPage("contoso-fabrikam-example", "Contoso", "10", 2, false, "1"),
			builtPage("nwm-vandelay-example", "Vandelay", "11", 1, true, "2"),
		),
	)
}

var templateNameRE = regexp.MustCompile(`^[a-z0-9]+([-_][a-z0-9]+)*$`)

// templateReport is the format check the screen depends on: a report, never a
// throw. "not a template" is keyed off the fixture HTML lacking the managed
// blocks, which is what the real validator decides too.
func templateReport(body any) obj {
	html, _ := get(body, "html").(string)
	rawName := ""
	if s, ok := get(body, "name").(string); ok {
		rawName = wire.TrimJS(s)
	}
	lower := strings.ToLower(rawName)
	validName := templateNameRE.MatchString(lower) && rawName != ""
	looks := strings.Contains(html, "pages-config-schema") && strings.Contains(html, "pages-data-schema")
	sample := strings.Contains(html, "pages-data-example")

	var name, nameError any
	if validName {
		name = lower
	}
	if rawName != "" && !validName {
		nameError = o("code", "bad_template_name", "message", "template must be url-safe with no slashes (a-z 0-9 - _)")
	}
	var contractError, configSchema, dataSchema, reference, dataKeys, shipsEmpty, hasSample, sampleKeys any
	if looks {
		configSchema = o("type", "object")
		dataSchema = o("type", "object")
		reference = o("campaign", "Reference")
		dataKeys = newList("rows")
		shipsEmpty = true
		hasSample = sample
		if sample {
			sampleKeys = newList("rows")
		}
	} else {
		contractError = o("code", "template_contract_invalid", "message", "page is not data-managed; add exactly one #pages-data-schema block and one #pages-data block")
	}
	return o(
		"name", name,
		"name_error", nameError,
		"contract_ok", looks,
		"contract_error", contractError,
		"config_schema", configSchema,
		"data_schema", dataSchema,
		"reference_config", reference,
		"data_keys", dataKeys,
		"ships_empty", shipsEmpty,
		"has_sample_data", hasSample,
		"sample_data_keys", sampleKeys,
		"bytes", len(html),
		"preflight", o("ok", true, "render_mode", "themed", "errors", newList(), "warnings", newList(),
			"errors_omitted", 0, "warnings_omitted", 0, "checks", newList(), "summary", "Preflight found no problems."),
	)
}
