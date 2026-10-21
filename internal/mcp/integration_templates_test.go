// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Port of test/mcp.integration.js: out-of-band upload tickets, and the
// template lifecycle (register, build, config/data independence, design
// propagation, drift, previews, promotion, retirement).

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/versions"
)

func templateConfigSchema() *jsonv.Object {
	return o(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"type", "object",
		"additionalProperties", false,
		"required", arr("campaign", "channel"),
		"properties", o(
			"campaign", o("type", "string", "minLength", num(1), "maxLength", num(120)),
			"channel", o("type", "string", "enum", arr("display", "video", "olv")),
		),
	)
}

func templateDataSchema() *jsonv.Object {
	return o(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"type", "object",
		"additionalProperties", false,
		"required", arr("rows"),
		"properties", o(
			"rows", o(
				"type", "array",
				"maxItems", num(500),
				"items", o(
					"type", "object",
					"additionalProperties", false,
					"required", arr("date", "spend"),
					"properties", o("date", o("type", "string"), "spend", o("type", "number", "minimum", num(0))),
				),
			),
		),
	)
}

func envelopeJSON(data any) string {
	return jsonv.Stringify(o("contract_version", num(1), "refreshed_at", "2026-08-01T00:00:00.000Z", "source_as_of", "2026-08-01T00:00:00.000Z", "data", data))
}

// templateHTML is a minimal but REAL template: both managed pairs, a
// reference config and an empty-state envelope that satisfy their own
// schemas, and a render layer that reads them. n varies the design.
func templateHTML(n int) string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>Campaign</title></head><body>` +
		fmt.Sprintf(`<h1 id="hTitle">Campaign</h1><div id="rev">design v%d</div><table id="rows"></table>`, n) +
		`<script type="application/schema+json" id="pages-config-schema">` + jsonv.Stringify(templateConfigSchema()) + `</script>` +
		`<script type="application/json" id="pages-config">` + jsonv.Stringify(o("campaign", "Reference", "channel", "display")) + `</script>` +
		`<script type="application/schema+json" id="pages-data-schema">` + jsonv.Stringify(templateDataSchema()) + `</script>` +
		`<script type="application/json" id="pages-data">` + envelopeJSON(o("rows", arr())) + `</script>` +
		`<script type="application/json" id="pages-data-example">` +
		jsonv.Stringify(o("rows", arr(o("date", "2026-07-30", "spend", num(1111)), o("date", "2026-07-31", "spend", num(2222))))) + `</script>` +
		`<script>
const CONFIG = JSON.parse(document.getElementById('pages-config').textContent);
const DATA = JSON.parse(document.getElementById('pages-data').textContent).data;
document.getElementById('hTitle').textContent = CONFIG.campaign;
document.getElementById('rows').textContent = DATA.rows.length ? String(DATA.rows.length) : 'Awaiting first ingest';
</script></body></html>`
}

// stage creates a ticket for target ("slug" or "template" key) and PUTs b.
func (h *harness) stage(targetKey, target string, b []byte, extra ...any) map[string]any {
	h.t.Helper()
	args := o(targetKey, target, "total_bytes", num(float64(len(b))), "content_sha256", shaHex(b))
	for i := 0; i+1 < len(extra); i += 2 {
		args.Set(extra[i].(string), extra[i+1])
	}
	ticket := h.data(h.call("create_upload_ticket", args))
	put := h.request("PUT", "/upload/"+strAt(ticket, "upload_id"), reqOpts{rawBody: ptr(string(b)), token: strAt(ticket, "ticket"), contentType: ptr("application/octet-stream")})
	if put.status != 200 {
		h.t.Fatalf("staging PUT %d: %s", put.status, put.body)
	}
	return ticket
}

func TestUploadTicket(t *testing.T) {
	h := newHarness(t)
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>Ticketed</title></head><body><main>`)
	for i := range 2400 {
		fmt.Fprintf(&b, `<p data-r="%d">row %d ☀</p>`, i, i)
	}
	b.WriteString("</main></body></html>")
	ticketHTML := b.String()
	ticketBytes := []byte(ticketHTML)
	ticketSHA := shaHex(ticketBytes)
	if len(ticketBytes) <= 64*1024 {
		t.Fatal("fixture must exceed one Node-sized chunk")
	}
	ticketed := h.data(h.call("create_upload_ticket", o("slug", "mcp-ticket-upload", "total_bytes", num(float64(len(ticketBytes))), "content_sha256", ticketSHA)))
	mustMatch(t, `^pgu_`, strAt(ticketed, "ticket"))
	eq(t, ticketed["complete"], false)
	eq(t, ticketed["bytes_received"], 0)
	mustMatch(t, `/upload/`, strAt(ticketed, "upload_url"))
	mustMatch(t, `--data-binary`, strAt(ticketed, "curl"))
	path := "/upload/" + strAt(ticketed, "upload_id")
	put := func(body []byte, token string, noToken bool) response {
		return h.request("PUT", path, reqOpts{rawBody: ptr(string(body)), token: token, noToken: noToken, contentType: ptr("application/octet-stream")})
	}

	// A ticket is not an agent token: it opens exactly one door.
	noAuth := put(ticketBytes, "", true)
	eq(t, noAuth.status, 401)
	eq(t, noAuth.get("code"), "upload_ticket_missing")
	eq(t, put(ticketBytes, "", false).status, 401, "a deploy token is not an upload ticket")
	wrong := put(append(append([]byte{}, ticketBytes[:len(ticketBytes)-1]...), 'X'), strAt(ticketed, "ticket"), false)
	eq(t, wrong.status, 409)
	eq(t, wrong.get("code"), "upload_hash_mismatch")
	short := put(ticketBytes[:100], strAt(ticketed, "ticket"), false)
	eq(t, short.status, 400)
	eq(t, short.get("code"), "upload_size_mismatch")
	otherTicket := h.data(h.call("create_upload_ticket", o("slug", "mcp-ticket-other", "total_bytes", num(float64(len(ticketBytes))), "content_sha256", ticketSHA)))
	cross := put(ticketBytes, strAt(otherTicket, "ticket"), false)
	eq(t, cross.status, 401, "a ticket for another upload must not work here")
	eq(t, cross.get("code"), "upload_ticket_invalid")
	h.data(h.call("cancel_page_upload", o("upload_id", strAt(otherTicket, "upload_id"))))

	sent := put(ticketBytes, strAt(ticketed, "ticket"), false)
	eq(t, sent.status, 200, sent.body)
	eq(t, sent.get("complete"), true)
	eq(t, sent.get("bytes_received"), len(ticketBytes))
	eq(t, sent.get("deduped"), false)
	mustMatch(t, `deploy_page_upload`, strAt(sent.json, "next_step"))
	resent := put(ticketBytes, strAt(ticketed, "ticket"), false)
	eq(t, resent.status, 200, resent.body)
	eq(t, resent.get("deduped"), true)
	spent := put([]byte("<!doctype html><p>substituted</p>"), strAt(ticketed, "ticket"), false)
	eq(t, spent.status, 409)
	eq(t, spent.get("code"), "upload_ticket_used")

	deployed := h.data(h.call("deploy_page_upload", o("upload_id", strAt(ticketed, "upload_id"), "title", "Ticketed Dashboard", "render_mode", "raw", "publish", true)))
	eq(t, deployed["created"], true)
	eq(t, deployed["version_is_live"], true)
	eq(t, dig(deployed, "version", "content_sha256"), ticketSHA)
	eq(t, h.scalar("SELECT html FROM page_versions WHERE id = $1", strAt(deployed, "version", "id")), ticketHTML, "out-of-band bytes survive byte-for-byte")
	eq(t, dig(deployed, "preflight", "ok"), true, jsonText(dig(deployed, "preflight", "errors")))

	eq(t, h.request("GET", path, reqOpts{token: strAt(ticketed, "ticket")}).status, 405)
	after := put(ticketBytes, strAt(ticketed, "ticket"), false)
	eq(t, after.status, 409)
	eq(t, after.get("code"), "page_upload_committed")
}

var payloadBlockRE = regexp.MustCompile(`<script (?:type="application/json" )?id="pages-(?:config|data)"[^>]*>[^<]*</script>`)

func versionHTML(h *harness, id string) string {
	h.t.Helper()
	return h.scalar("SELECT html FROM page_versions WHERE id = $1", id).(string)
}

func servingHTML(h *harness, slug string) string {
	h.t.Helper()
	return h.scalar("SELECT html FROM page_versions WHERE id = (SELECT published_version_id FROM pages WHERE slug = $1)", slug).(string)
}

func findTemplate(list map[string]any, name string) map[string]any {
	for _, e := range toAnySlice(list["templates"]) {
		if strAt(e, "name") == name {
			m, _ := e.(map[string]any)
			return m
		}
	}
	return nil
}

func TestTemplates(t *testing.T) {
	h := newHarness(t)
	h.data(h.call("deploy_page", o("slug", "mcpdemo", "title", "MCP Demo", "html", pageHTML(1), "publish", true)))

	// ── register once, build many ──
	templateBytes := []byte(templateHTML(1))
	templateSHA := shaHex(templateBytes)
	tt := h.stage("template", "mcp-campaign", templateBytes)
	eq(t, tt["target_kind"], "template")
	eq(t, tt["template"], "mcp-campaign")
	eq(t, tt["slug"], nil, "a template upload reports no slug")
	ttID := strAt(tt, "upload_id")

	// A template upload is not deployable as a page.
	wrongConsumer := h.fails(h.call("deploy_page_upload", o("upload_id", ttID)))
	eq(t, wrongConsumer.code(), "page_upload_target_mismatch")
	mustMatch(t, `(?i)staged for a template`, strAt(wrongConsumer.data, "error"))
	eq(t, dig(wrongConsumer.data, "details", "target_kind"), "template")

	// A dry run over the STAGED bytes that must not spend them.
	dry := h.data(h.call("validate_template", o("upload_id", ttID)))
	eq(t, dry["contract_ok"], true, jsonText(dry["contract_error"]))
	eq(t, dry["name"], "mcp-campaign", "the upload already names the template")
	eq(t, dry["bytes"], len(templateBytes))
	eq(t, dry["ships_empty"], true)
	eq(t, dry["has_sample_data"], true)
	eq(t, dry["sample_data_keys"], []any{"rows"})
	eq(t, dig(dry, "preflight", "ok"), true, jsonText(dig(dry, "preflight", "errors")))
	mustMatch(t, `register_template_upload`, strAt(dry, "next_step"))
	if findTemplate(h.data(h.call("list_templates", o())), "mcp-campaign") != nil {
		t.Fatal("a dry run must not register anything")
	}
	badDry := h.data(h.call("validate_template", o("html", "<p>not a template</p>", "name", "nope")))
	eq(t, badDry["contract_ok"], false)
	eq(t, dig(badDry, "contract_error", "code"), "template_contract_invalid")
	if badDry["preflight"] == nil {
		t.Fatal("preflight still runs so both problems surface in one pass")
	}
	eq(t, h.fails(h.call("validate_template", o("html", "<p>x</p>", "upload_id", ttID))).code(), "template_source_ambiguous")
	eq(t, h.fails(h.call("validate_template", o())).code(), "template_source_ambiguous")

	registered := h.data(h.call("register_template_upload", o("upload_id", ttID, "title", "MCP Campaign Dashboard", "description", "Integration fixture for the template contract.")))
	eq(t, registered["created"], true)
	eq(t, registered["deduped"], false)
	eq(t, dig(registered, "template", "name"), "mcp-campaign")
	eq(t, dig(registered, "revision", "revision"), 1)
	eq(t, dig(registered, "revision", "content_sha256"), templateSHA)
	eq(t, registered["reference_config"], map[string]any{"campaign": "Reference", "channel": "display"})
	eq(t, dig(registered, "config_schema", "type"), "object")
	eq(t, dig(registered, "data_schema", "type"), "object")
	eq(t, dig(registered, "preflight", "ok"), true)

	retry := h.stage("template", "mcp-campaign", []byte(templateHTML(1)))
	reRegistered := h.data(h.call("register_template_upload", o("upload_id", strAt(retry, "upload_id"))))
	eq(t, reRegistered["deduped"], true)
	eq(t, reRegistered["created"], false)
	eq(t, dig(reRegistered, "revision", "revision"), 1, "identical bytes do not invent a revision")

	fetched := h.data(h.call("get_template", o("template", "mcp-campaign")))
	eq(t, dig(fetched, "template", "current_revision"), 1)
	if _, has := fetched["html"]; has {
		t.Fatal("HTML must be omitted unless asked for")
	}
	eq(t, fetched["config_schema"], registered["config_schema"])
	if len(jsonText(fetched)) >= len(templateBytes) {
		t.Fatal("reading the contract must cost less than reading the design")
	}
	eq(t, h.data(h.call("get_template", o("template", "mcp-campaign", "include_html", true)))["html"], templateHTML(1))
	mine := findTemplate(h.data(h.call("list_templates", o())), "mcp-campaign")
	eq(t, mine["title"], "MCP Campaign Dashboard")
	eq(t, mine["current_revision"], 1)
	eq(t, mine["page_count"], 0)
	revisions := h.data(h.call("list_template_revisions", o("template", "mcp-campaign")))
	eq(t, len(toAnySlice(revisions["revisions"])), 1)
	eq(t, dig(revisions, "revisions", 0, "is_current"), true)

	notATemplate := h.stage("template", "mcp-not-a-template", []byte(pageHTML(9)))
	eq(t, h.fails(h.call("register_template_upload", o("upload_id", strAt(notATemplate, "upload_id")))).code(), "template_contract_invalid")
	eq(t, h.fails(h.call("get_template", o("template", "mcp-not-a-template"))).code(), "template_not_found", "a rejected registration leaves nothing behind")
	for _, args := range []any{
		o("slug", "mcp-both", "template", "mcp-both", "total_bytes", num(10), "content_sha256", templateSHA),
		o("total_bytes", num(10), "content_sha256", templateSHA),
	} {
		eq(t, h.fails(h.call("create_upload_ticket", args)).code(), "upload_target_required")
	}

	// ── building pages from one template ──
	configA := o("campaign", "Acme Spring", "channel", "display")
	configB := o("campaign", "Globex Q3", "channel", "video")
	builtA := h.data(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-acme", "config", configA, "title", "Acme Spring", "render_mode", "raw", "publish", true)))
	eq(t, builtA["created"], true)
	eq(t, builtA["version_is_live"], true)
	eq(t, builtA["template"], "mcp-campaign")
	eq(t, builtA["template_revision"], 1)
	eq(t, jsonText(builtA["config"]), jsonv.Stringify(configA))
	eq(t, dig(builtA, "preflight", "ok"), true)
	eq(t, dig(builtA, "envelope", "data"), map[string]any{"rows": []any{}})
	eq(t, dig(builtA, "envelope", "source_as_of"), "1970-01-01T00:00:00.000Z", "a page created without data claims no source coverage")
	for _, needle := range []string{"pages-data-example", "1111", "2222"} {
		eq(t, h.data(h.call("find_in_version", o("slug", "mcp-tpl-acme", "query", needle)))["total_matches"], 0,
			"a page built from the template still contains "+needle)
	}
	eq(t, dig(h.data(h.call("get_page_data", o("slug", "mcp-tpl-acme"))), "envelope", "data"), map[string]any{"rows": []any{}})
	eq(t, dig(h.data(h.call("get_template", o("template", "mcp-campaign"))), "revision", "has_sample_data"), true)

	builtB := h.data(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-globex", "config", configB,
		"data", o("rows", arr(o("date", "2026-08-01", "spend", 125.5))), "source_as_of", "2026-08-01T12:00:00Z", "render_mode", "raw", "publish", true)))
	eq(t, builtB["created"], true)
	eq(t, dig(builtB, "envelope", "data"), map[string]any{"rows": []any{map[string]any{"date": "2026-08-01", "spend": 125.5}}})
	htmlA := versionHTML(h, strAt(builtA, "version", "id"))
	htmlB := versionHTML(h, strAt(builtB, "version", "id"))
	if !strings.Contains(htmlA, `<div id="rev">design v1</div>`) {
		t.Fatal("the stored design is what got rendered")
	}
	eq(t, payloadBlockRE.ReplaceAllString(htmlA, "«payload»"), payloadBlockRE.ReplaceAllString(htmlB, "«payload»"),
		"two pages from one revision differ ONLY in their config and data blocks")
	createCost := len(jsonv.Stringify(o("template", "mcp-campaign", "slug", "mcp-tpl-acme", "config", configA)))
	if createCost*8 >= len(htmlA) {
		t.Fatalf("building a page must cost far less than its design (%d vs %d bytes)", createCost, len(htmlA))
	}
	eq(t, findTemplate(h.data(h.call("list_templates", o())), "mcp-campaign")["page_count"], 2)

	eq(t, h.fails(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-partial", "config", o("campaign", "No Channel")))).code(), "config_validation_failed")
	eq(t, h.fails(h.call("get_page", o("slug", "mcp-tpl-partial"))).code(), "page_not_found")
	retried := h.data(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-acme", "config", configA, "title", "Acme Spring", "render_mode", "raw", "publish", true)))
	eq(t, retried["deduped"], true)
	eq(t, dig(retried, "version", "id"), dig(builtA, "version", "id"), "an identical retry returns the same immutable version")
	clobber := h.fails(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-acme", "config", o("campaign", "Hostile Takeover", "channel", "olv"))))
	eq(t, clobber.code(), "page_exists")
	mustMatch(t, `update_page_config`, strAt(clobber.data, "error"))

	// ── config and data are independent ──
	readConfig := h.data(h.call("get_page_config", o("slug", "mcp-tpl-acme")))
	eq(t, jsonText(readConfig["config"]), jsonv.Stringify(configA))
	eq(t, dig(readConfig, "template", "name"), "mcp-campaign")
	eq(t, dig(readConfig, "template", "is_current_revision"), true)
	eq(t, dig(readConfig, "template", "revision"), dig(readConfig, "template", "current_revision"))
	builtHTML := versionHTML(h, strAt(readConfig, "live_version_id"))
	h.data(h.call("deploy_page", o("slug", "mcp-tpl-acme-copy", "title", "Copy", "html", builtHTML, "publish", true)))
	copyConfig := h.data(h.call("get_page_config", o("slug", "mcp-tpl-acme-copy")))
	eq(t, jsonText(copyConfig["config"]), jsonv.Stringify(configA), "the copy still has its config block")
	eq(t, copyConfig["template"], nil, "…but it is not template-built")
	eq(t, dig(readConfig, "config_schema", "type"), "object")
	eq(t, readConfig["live_version_id"], dig(builtA, "version", "id"))

	dataState := h.data(h.call("get_page_data", o("slug", "mcp-tpl-acme")))
	refreshedResp := h.call("update_page_data", o("slug", "mcp-tpl-acme", "data", o("rows", arr(o("date", "2026-08-02", "spend", num(42)))),
		"source_as_of", "2026-08-02T00:00:00Z", "expected_version", dataState["live_version_id"]))
	refreshed := h.data(refreshedResp)
	eq(t, refreshed["version_is_live"], true, "update_page_data works unchanged on a template-built page")
	if !strings.Contains(versionHTML(h, strAt(refreshed, "version", "id")), jsonv.Stringify(configA)) {
		t.Fatal("a refresh leaves config bytes alone")
	}
	beforeEdit := h.data(h.call("get_page_config", o("slug", "mcp-tpl-acme")))
	renamed := o("campaign", "Acme Spring (renamed)", "channel", "olv")
	configUpdated := h.data(h.call("update_page_config", o("slug", "mcp-tpl-acme", "config", renamed, "expected_version", beforeEdit["live_version_id"])))
	eq(t, configUpdated["version_is_live"], true)
	eq(t, jsonText(configUpdated["config"]), jsonv.Stringify(renamed))
	eq(t, configUpdated["envelope"], refreshed["envelope"], "the envelope is carried across untouched")
	eq(t, configUpdated["data_sha256"], refreshed["data_sha256"], "numbers cannot move during a config edit")
	if configUpdated["template_sha256"] == refreshed["template_sha256"] {
		t.Fatal("config is part of the template identity")
	}
	edited := versionHTML(h, strAt(configUpdated, "version", "id"))
	if !strings.Contains(edited, jsonv.Stringify(renamed)) {
		t.Fatal("the config edit is in the document")
	}
	if !strings.Contains(edited, strings.ReplaceAll(jsonv.Stringify(ordered(t, refreshedResp).Value("envelope")), "<", `\u003c`)) {
		t.Fatal("the data block survives a config edit byte-for-byte")
	}
	prov := h.query(`SELECT v.template_version_id, v.config_sha256, t.revision
	                   FROM page_versions v JOIN page_template_versions t ON t.id = v.template_version_id
	                  WHERE v.id = ANY($1::bigint[]) ORDER BY v.id`,
		[]string{strAt(builtA, "version", "id"), strAt(refreshed, "version", "id"), strAt(configUpdated, "version", "id")})
	eq(t, len(prov), 3, "create, refresh and config edit all stay attributable")
	for _, r := range prov {
		eq(t, r["revision"], 1)
	}
	eq(t, h.fails(h.call("update_page_config", o("slug", "mcp-tpl-acme", "config", configA, "expected_version", dig(builtA, "version", "id")))).code(), "stale_version")

	// The retry that must NOT be a no-op: replaying a superseded build would
	// drag the live pointer back to the empty state.
	eq(t, h.data(h.call("get_page_config", o("slug", "mcp-tpl-acme")))["live_version_id"], dig(configUpdated, "version", "id"))
	staleReplay := h.fails(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-acme", "config", configA, "title", "Acme Spring", "render_mode", "raw", "publish", true)))
	eq(t, staleReplay.code(), "page_exists", "replaying a superseded build is a conflict, not a no-op")
	mustMatch(t, `rollback_page`, strAt(staleReplay.data, "error"))
	afterReplay := h.data(h.call("get_page_config", o("slug", "mcp-tpl-acme")))
	eq(t, afterReplay["live_version_id"], dig(configUpdated, "version", "id"), "the live pointer did not move")
	eq(t, jsonText(afterReplay["config"]), jsonv.Stringify(renamed))
	eq(t, h.data(h.call("get_page_data", o("slug", "mcp-tpl-acme")))["data_sha256"], refreshed["data_sha256"])

	eq(t, h.fails(h.call("get_page_config", o("slug", "mcpdemo"))).code(), "page_not_template_managed")
	liveDemo := h.data(h.call("get_page", o("slug", "mcpdemo")))
	eq(t, h.fails(h.call("update_page_config", o("slug", "mcpdemo", "config", o("campaign", "x", "channel", "display"), "expected_version", dig(liveDemo, "published", "id")))).code(), "page_not_template_managed")

	// ── propagating a design fix, one page at a time ──
	rev2 := h.stage("template", "mcp-campaign", []byte(templateHTML(2)))
	revTwo := h.data(h.call("register_template_upload", o("upload_id", strAt(rev2, "upload_id"), "note", "chart fix")))
	eq(t, dig(revTwo, "revision", "revision"), 2)
	if !strings.Contains(servingHTML(h, "mcp-tpl-globex"), "design v1") {
		t.Fatal("a new revision must not touch a deployed page")
	}
	behind := h.data(h.call("list_template_pages", o("template", "mcp-campaign")))
	eq(t, behind["current_revision"], 2)
	eq(t, len(toAnySlice(behind["pages"])), 2)
	var slugs []string
	for _, p := range toAnySlice(behind["pages"]) {
		if numAt(p, "revision") != 1 || dig(p, "behind") != true {
			t.Fatalf("page %v is not behind on revision 1", p)
		}
		slugs = append(slugs, strAt(p, "slug"))
	}
	eq(t, slugs, []string{"mcp-tpl-acme", "mcp-tpl-globex"})

	canary := h.data(h.call("rerender_page_from_template", o("slug", "mcp-tpl-globex")))
	eq(t, canary["from_revision"], 1)
	eq(t, canary["template_revision"], 2)
	eq(t, canary["published"], false, "publish defaults to false")
	eq(t, canary["version_is_live"], false)
	eq(t, canary["page_is_live"], true)
	eq(t, jsonText(canary["config"]), jsonv.Stringify(configB))
	eq(t, dig(canary, "envelope", "data"), map[string]any{"rows": []any{map[string]any{"date": "2026-08-01", "spend": 125.5}}})
	eq(t, dig(canary, "preflight", "ok"), true)
	if !strings.Contains(versionHTML(h, strAt(canary, "version", "id")), "design v2") || !strings.Contains(servingHTML(h, "mcp-tpl-globex"), "design v1") {
		t.Fatal("the canary carries the new design while the client still sees the reviewed one")
	}
	mustMatch(t, `publish_page`, strAt(canary, "next_step"))
	eq(t, h.data(h.call("publish_page", o("slug", "mcp-tpl-globex", "version_id", dig(canary, "version", "id"))))["version_is_live"], true)
	var states [][]any
	for _, p := range toAnySlice(h.data(h.call("list_template_pages", o("template", "mcp-campaign")))["pages"]) {
		states = append(states, []any{strAt(p, "slug"), dig(p, "revision"), dig(p, "behind")})
	}
	eq(t, states, [][]any{{"mcp-tpl-acme", 1, true}, {"mcp-tpl-globex", 2, false}}, "propagation is per page")

	// The write path refuses a document whose managed blocks do not parse.
	bricking := h.fails(h.call("patch_page", o("slug", "mcp-tpl-globex", "edits", arr(o("find", `"contract_version":1`, "replace", `"contract_version":1,,`)), "note", "the edit that used to brick a page")))
	eq(t, bricking.code(), "data_contract_invalid")
	mustMatch(t, `refusing to publish`, strAt(bricking.data, "error"))
	if dig(h.data(h.call("get_page_data", o("slug", "mcp-tpl-globex"))), "envelope", "data") == nil {
		t.Fatal("the managed-data toolchain still works on the page")
	}
	rawBrick := h.fails(h.call("deploy_page", o("slug", "mcp-brick", "render_mode", "raw", "html", `<!doctype html><html><body>`+
		`<script type="application/schema+json" id="pages-data-schema">`+jsonv.Stringify(templateDataSchema())+`</script>`+
		`<script type="application/json" id="pages-data">{ "contract_version": nope }</script>`+`</body></html>`)))
	eq(t, rawBrick.code(), "data_contract_invalid")
	eq(t, h.fails(h.call("get_page", o("slug", "mcp-brick"))).code(), "page_not_found", "a refused deploy creates no page")
	h.data(h.call("deploy_page", o("slug", "mcp-plain-ok", "html", "<!doctype html><html><body><h1>No managed blocks here</h1></body></html>", "render_mode", "raw", "publish", true)))
	h.data(h.call("delete_page", o("slug", "mcp-plain-ok")))

	// Drift: a patched page is visible as drifted and can be pulled back.
	driftConfig := o("campaign", "Drift Co", "channel", "display")
	h.data(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-drift", "config", driftConfig, "publish", true)))
	h.data(h.call("patch_page", o("slug", "mcp-tpl-drift", "edits", arr(o("find", "design v2", "replace", "design v2 (hand-edited)")), "note", "the edit that forks a page off its design", "publish", true)))
	drifted := h.data(h.call("list_template_pages", o("template", "mcp-campaign")))
	eq(t, drifted["drifted_count"], 1)
	eq(t, drifted["serving_count"], 2)
	var forked any
	for _, p := range toAnySlice(drifted["pages"]) {
		if strAt(p, "slug") == "mcp-tpl-drift" {
			forked = p
		}
	}
	eq(t, dig(forked, "drifted"), true)
	eq(t, dig(forked, "revision"), nil)
	eq(t, dig(forked, "behind"), false)
	eq(t, dig(forked, "last_revision"), 2)
	eq(t, dig(forked, "config_sha256"), nil)
	counted := findTemplate(h.data(h.call("list_templates", o())), "mcp-campaign")
	eq(t, counted["page_count"], 3)
	eq(t, counted["serving_count"], 2)
	eq(t, counted["drifted_count"], 1)
	eq(t, h.fails(h.call("rerender_page_from_template", o("slug", "mcp-tpl-drift"))).code(), "page_not_template_managed")
	pulledBack := h.data(h.call("rerender_page_from_template", o("slug", "mcp-tpl-drift", "template", "mcp-campaign", "publish", true)))
	eq(t, pulledBack["reattached"], true)
	eq(t, pulledBack["from_revision"], nil)
	eq(t, pulledBack["template_revision"], 2)
	eq(t, jsonText(pulledBack["config"]), jsonv.Stringify(driftConfig))
	healed := h.data(h.call("list_template_pages", o("template", "mcp-campaign")))
	eq(t, healed["drifted_count"], 0)
	eq(t, healed["serving_count"], 3)
	h.data(h.call("deploy_page", o("slug", "mcp-tpl-drift", "html", "<!doctype html><html><head><title>Forked</title></head><body><h1>Nothing left</h1></body></html>", "render_mode", "raw", "publish", true)))
	unrecoverable := h.fails(h.call("rerender_page_from_template", o("slug", "mcp-tpl-drift", "template", "mcp-campaign")))
	eq(t, unrecoverable.code(), "page_not_template_managed")
	mustMatch(t, `cannot be recovered from what is serving`, strAt(unrecoverable.data, "error"))
	mustMatch(t, `create_page_from_template`, strAt(unrecoverable.data, "error"))
	h.data(h.call("delete_page", o("slug", "mcp-tpl-drift")))
	eq(t, h.fails(h.call("rerender_page_from_template", o("slug", "mcp-tpl-globex"))).code(), "template_revision_unchanged")

	// A revision that tightens its contract fails loudly.
	strictSchema := templateConfigSchema()
	strictSchema.Set("required", arr("campaign", "channel", "accountCode"))
	props := strictSchema.Value("properties").(*jsonv.Object)
	props.Set("accountCode", o("type", "string", "minLength", num(1)))
	strictHTML := strings.Replace(templateHTML(3), jsonv.Stringify(templateConfigSchema()), jsonv.Stringify(strictSchema), 1)
	strictHTML = strings.Replace(strictHTML, jsonv.Stringify(o("campaign", "Reference", "channel", "display")),
		jsonv.Stringify(o("campaign", "Reference", "channel", "display", "accountCode", "ACCT00000")), 1)
	strictTicket := h.stage("template", "mcp-campaign", []byte(strictHTML))
	eq(t, dig(h.data(h.call("register_template_upload", o("upload_id", strAt(strictTicket, "upload_id")))), "revision", "revision"), 3)
	eq(t, h.fails(h.call("rerender_page_from_template", o("slug", "mcp-tpl-acme", "revision", num(3)))).code(), "config_validation_failed")
	eq(t, h.data(h.call("get_page_config", o("slug", "mcp-tpl-acme")))["live_version_id"], dig(configUpdated, "version", "id"), "a refused rerender writes nothing")
	eq(t, h.fails(h.call("rerender_page_from_template", o("slug", "mcpdemo"))).code(), "page_not_template_managed")

	templateAdvice := h.data(h.call("prepare_dashboard_update", o("slug", "mcp-tpl-globex", "instructions", "Make the spend chart taller and rename the campaign to Globex Q4.")))
	eq(t, templateAdvice["mode"], "managed_template")
	for _, p := range []string{`update_page_config`, `rerender_page_from_template`, `(?i)forks it off`, `revision 2`} {
		mustMatch(t, p, strAt(templateAdvice, "prompt"))
	}
	eq(t, h.data(h.call("prepare_dashboard_update", o("slug", "mcp-tpl-globex", "instructions", "Refresh with yesterday's complete spend.", "update_type", "data")))["mode"], "managed_data")

	// ── template_urls ──
	urls := h.data(h.call("template_urls", o("template", "mcp-campaign")))
	eq(t, urls["template"], "mcp-campaign")
	eq(t, urls["revision"], dig(h.data(h.call("get_template", o("template", "mcp-campaign"))), "template", "current_revision"))
	eq(t, urls["has_sample_data"], true)
	if !strings.HasSuffix(strAt(urls, "library"), "/admin/templates") {
		t.Fatal("library URL")
	}
	mustMatch(t, `/raw-template/\d+\?t=`, strAt(urls, "preview"))
	if strings.HasPrefix(strAt(urls, "preview"), strings.TrimSuffix(strAt(urls, "library"), "/admin/templates")) {
		t.Fatal("the preview must not be on the dashboard origin")
	}
	eq(t, urls["expires_in_seconds"], 300)
	eq(t, h.data(h.call("template_urls", o("template", "mcp-campaign", "revision", num(1))))["revision"], 1)
	eq(t, h.fails(h.call("template_urls", o("template", "mcp-campaign", "revision", num(99)))).code(), "template_revision_not_found")

	// The retry allowance covers a gated build...
	gatedConfig := o("campaign", "Gated Co", "channel", "display", "accountCode", "ACCT99999")
	gated := h.data(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-gated", "config", gatedConfig, "require_approval", true)))
	eq(t, gated["created"], true)
	eq(t, gated["version_is_live"], false)
	eq(t, gated["live_version_id"], nil)
	gatedRetry := h.data(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-gated", "config", gatedConfig, "require_approval", true)))
	eq(t, gatedRetry["deduped"], true)
	eq(t, dig(gatedRetry, "version", "id"), dig(gated, "version", "id"))
	// ...but not a restored page with history.
	admin := versions.Actor{Actor: "admin@elcanotek.com", ActorType: audit.User, IP: "127.0.0.1"}
	restoredConfig := o("campaign", "Restored Co", "channel", "display", "accountCode", "ACCT88888")
	restoredBuild := h.data(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-restored", "config", restoredConfig)))
	restoredRefresh := h.data(h.call("update_page_data", o("slug", "mcp-tpl-restored", "data", o("rows", arr(o("date", "2026-08-03", "spend", num(77)))),
		"source_as_of", "2026-08-03T00:00:00Z", "expected_version", dig(restoredBuild, "version", "id"))))
	ctx := context.Background()
	if _, err := versions.DeletePage(ctx, h.d, "mcp-tpl-restored", admin); err != nil {
		t.Fatal(err)
	}
	if _, err := versions.RestorePage(ctx, h.d, "mcp-tpl-restored", admin); err != nil {
		t.Fatal(err)
	}
	eq(t, h.scalar("SELECT published_version_id FROM pages WHERE slug = 'mcp-tpl-restored'"), nil, "fixture: restore leaves the pointer NULL")
	eq(t, h.fails(h.call("create_page_from_template", o("template", "mcp-campaign", "slug", "mcp-tpl-restored", "config", restoredConfig))).code(), "page_exists")
	eq(t, h.scalar("SELECT published_version_id FROM pages WHERE slug = 'mcp-tpl-restored'"), nil, "nothing was republished behind the refresh")
	if strAt(restoredRefresh, "version", "id") == "" {
		t.Fatal("fixture sanity: the refresh exists in history")
	}

	// ── "I like this page, make more like it" ──
	authoredHTML := `<!doctype html><html><head><meta charset="utf-8"><title>Acme Corp</title>
<style>:root{--brand:#333}h1{color:var(--brand)}</style></head><body>
<h1 id="who"></h1><p id="total"></p>
<script type="application/json" id="pages-config">` + jsonv.Stringify(o("client", "Acme Corp", "brand", o("primary", "#0a3d62"), "kpiTarget", 3.5, "flightEnd", nil)) + `</script>
<script type="application/schema+json" id="pages-data-schema">` + jsonv.Stringify(templateDataSchema()) + `</script>
<script type="application/json" id="pages-data">` + jsonv.Stringify(o("contract_version", num(1), "refreshed_at", "2026-08-01T00:00:00.000Z", "source_as_of", "2026-08-01T00:00:00.000Z",
		"data", o("rows", arr(o("date", "2026-07-31", "spend", 940.25))))) + `</script>
<script>
const CONFIG = JSON.parse(document.getElementById('pages-config').textContent);
const DATA = JSON.parse(document.getElementById('pages-data').textContent).data;
document.documentElement.style.setProperty('--brand', CONFIG.brand.primary);
document.getElementById('who').textContent = CONFIG.client;
document.getElementById('total').textContent = DATA.rows.length ? String(DATA.rows.length) : 'Awaiting first ingest';
</script></body></html>`
	authored := h.data(h.call("deploy_page", o("slug", "acme-live", "html", authoredHTML, "title", "Acme", "publish", true)))
	eq(t, authored["config_schema_generated"], true, "a config-carrying page gets a derived schema")
	authoredConfig := h.data(h.call("get_page_config", o("slug", "acme-live")))
	eq(t, dig(authoredConfig, "config", "client"), "Acme Corp")
	eq(t, dig(authoredConfig, "config_schema", "type"), "object")
	eq(t, h.fails(h.call("update_page_config", o("slug", "acme-live", "config", o("clientt", "Acme Corp", "brand", o("primary", "#0a3d62"), "kpiTarget", 3.5),
		"expected_version", authoredConfig["live_version_id"]))).code(), "config_validation_failed")
	withoutOptional := h.data(h.call("update_page_config", o("slug", "acme-live", "config", o("client", "Acme Corp", "brand", o("primary", "#123456"), "kpiTarget", 3.5),
		"expected_version", authoredConfig["live_version_id"])))
	if strAt(withoutOptional, "version", "id") == "" {
		t.Fatal("a config omitting a null-valued key is accepted")
	}
	promoted := h.data(h.call("create_template_from_page", o("slug", "acme-live", "template", "acme-family", "empty_data", o("rows", arr()),
		"example_from_current_data", true, "title", "Acme Family")))
	eq(t, promoted["created"], true)
	eq(t, promoted["from_page"], "acme-live")
	eq(t, dig(promoted, "revision", "revision"), 1)
	eq(t, promoted["example_from_current_data"], true)
	eq(t, promoted["has_sample_data"], true)
	eq(t, dig(promoted, "reference_config", "client"), "Acme Corp")
	eq(t, dig(promoted, "preflight", "ok"), true)
	nextClient := h.data(h.call("create_page_from_template", o("template", "acme-family", "slug", "globex-live",
		"config", o("client", "Globex", "brand", o("primary", "#b71540"), "kpiTarget", num(4)), "title", "Globex", "publish", true)))
	eq(t, nextClient["created"], true)
	eq(t, dig(nextClient, "envelope", "data"), map[string]any{"rows": []any{}}, "it starts empty, not with Acme's numbers")
	for _, needle := range []string{"pages-data-example", "940.25"} {
		eq(t, h.data(h.call("find_in_version", o("slug", "globex-live", "query", needle)))["total_matches"], 0, needle)
	}
	eq(t, h.data(h.call("find_in_version", o("slug", "globex-live", "query", "<title>Acme Corp")))["total_matches"], 1,
		"a literal in the design's markup survives promotion — that is what the warning is for")
	var flagged any
	for _, e := range toAnySlice(promoted["hardcoded_config_values"]) {
		if strAt(e, "path") == "client" {
			flagged = e
		}
	}
	if flagged == nil {
		t.Fatalf("the promotion must flag client: %s", jsonText(promoted["hardcoded_config_values"]))
	}
	eq(t, dig(flagged, "value"), "Acme Corp")
	if numAt(flagged, "occurrences") < 1 {
		t.Fatal("occurrences")
	}
	if numAt(h.data(h.call("find_in_version", o("slug", "globex-live", "query", "#b71540"))), "total_matches") < 1 {
		t.Fatal("…and it carries its own brand colour")
	}
	eq(t, h.fails(h.call("create_template_from_page", o("slug", "mcpdemo", "template", "not-reusable", "empty_data", o("rows", arr())))).code(), "page_not_data_managed")
	dataOnly := `<!doctype html><html><head><title>Data only</title></head><body><table id="r"></table>
<script type="application/schema+json" id="pages-data-schema">` + jsonv.Stringify(templateDataSchema()) + `</script>
<script type="application/json" id="pages-data">` + envelopeJSON(o("rows", arr())) + `</script></body></html>`
	h.call("deploy_page", o("slug", "data-only", "html", dataOnly, "title", "Data only", "publish", true))
	noConfig := h.fails(h.call("create_template_from_page", o("slug", "data-only", "template", "not-reusable", "empty_data", o("rows", arr()))))
	eq(t, noConfig.code(), "page_not_template_managed")
	mustMatch(t, `pages-config`, strAt(noConfig.data, "error"))

	// ── delete_template ──
	guarded := h.fails(h.call("delete_template", o("template", "mcp-campaign")))
	eq(t, guarded.code(), "template_has_pages")
	eq(t, dig(guarded.data, "details", "pages"), 4)
	mustMatch(t, `list_template_pages`, strAt(guarded.data, "error"))
	if findTemplate(h.data(h.call("list_templates", o())), "mcp-campaign") == nil {
		t.Fatal("a refused delete writes nothing")
	}
	typo := []byte(templateHTML(9))
	typoTicket := h.stage("template", "mcp-typoo", typo)
	h.call("register_template_upload", o("upload_id", strAt(typoTicket, "upload_id"), "title", "Typo"))
	retired := h.data(h.call("delete_template", o("template", "mcp-typoo")))
	eq(t, retired["deleted"], true)
	eq(t, retired["pages_built"], 0)
	if findTemplate(h.data(h.call("list_templates", o())), "mcp-typoo") != nil {
		t.Fatal("it leaves the library")
	}
	eq(t, h.fails(h.call("get_template", o("template", "mcp-typoo"))).code(), "template_not_found")
	reuse := h.stage("template", "mcp-typoo", typo)
	reused := h.data(h.call("register_template_upload", o("upload_id", strAt(reuse, "upload_id"), "title", "Reused")))
	eq(t, reused["created"], true, "the retired name is available again")
	eq(t, dig(reused, "revision", "revision"), 1, "and starts a fresh revision series")
	h.call("delete_template", o("template", "mcp-typoo"))
	forced := h.data(h.call("delete_template", o("template", "mcp-campaign", "force", true)))
	eq(t, forced["deleted"], true)
	eq(t, forced["pages_built"], 4)
	stillLive := h.data(h.call("get_page", o("slug", "mcp-tpl-acme")))
	if strAt(stillLive, "page", "published_version_id") == "" || dig(stillLive, "page", "disabled") != false || stillLive["published"] == nil {
		t.Fatal("a page outlives the template it was built from")
	}
	eq(t, h.fails(h.call("rerender_page_from_template", o("slug", "mcp-tpl-acme"))).code(), "template_not_found")
	_ = slices.Contains[[]string]
}

// The JavaScript suite drove the endpoint with the official SDK Client. There
// is no Go port of that client; this replays its lifecycle (initialize,
// notifications/initialized, tools/list, ping, tools/call) as it sends it,
// including a validation error and an unknown tool.
func TestClientLifecycleInterop(t *testing.T) {
	h := newHarness(t)
	h.data(h.call("deploy_page", o("slug", "mcpdemo", "title", "MCP Demo", "html", pageHTML(1), "publish", true)))
	init := h.rpc("initialize", o("protocolVersion", protocol, "capabilities", o(),
		"clientInfo", o("name", "pages-official-sdk-integration", "title", "Pages SDK Integration", "version", "1.0.0")), rpcOpts{})
	eq(t, init.get("result", "protocolVersion"), protocol)
	eq(t, init.get("result", "serverInfo", "name"), "pages")
	eq(t, h.rpc("notifications/initialized", nil, rpcOpts{notification: true}).status, 202)
	listed := toAnySlice(h.rpc("tools/list", nil, rpcOpts{}).get("result", "tools"))
	var names []string
	var chunk any
	for _, tool := range listed {
		names = append(names, strAt(tool, "name"))
		if strAt(tool, "name") == "append_page_upload" {
			chunk = dig(tool, "inputSchema", "properties", "chunk_base64")
		}
	}
	slices.Sort(names)
	eq(t, names, expectedTools)
	eq(t, dig(chunk, "type"), "string")
	eq(t, dig(chunk, "contentEncoding"), "base64", "file-capable clients can forward original bytes")
	eq(t, h.rpc("ping", nil, rpcOpts{}).get("result"), map[string]any{})
	invalid := h.call("get_page", o("slug", "mcpdemo", "bogus", num(1)))
	eq(t, invalid.get("result", "isError"), true, "argument validation reaches a client as a tool error")
	mustMatch(t, `Invalid arguments for tool get_page`, strAt(invalid.json, "result", "content", 0, "text"))
	eq(t, h.call("no_such_tool", o()).get("error", "code"), -32602)
	read := h.call("get_page", o("slug", "mcpdemo"))
	eq(t, read.get("result", "isError"), nil)
	eq(t, read.get("result", "structuredContent", "page", "slug"), "mcpdemo")
	h.data(read)
}
