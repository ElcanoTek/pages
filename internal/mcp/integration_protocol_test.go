// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Port of test/mcp.integration.js, sections 1–9: the endpoint's HTTP and
// JSON-RPC boundaries, discovery, and the basic create-and-publish path,
// driven over the wire rather than by calling handlers.

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"testing"
)

var expectedTools = []string{
	"append_page_upload", "cancel_page_upload", "configure_page_refresh", "create_page_from_template",
	"create_template_from_page", "create_upload_ticket", "create_workspace", "delete_page", "delete_template",
	"deploy_page", "deploy_page_upload", "find_in_version", "get_guide", "get_page", "get_page_config",
	"get_page_data", "get_page_refresh", "get_template", "get_version", "list_pages", "list_refresh_health", "list_template_pages",
	"list_template_revisions", "list_templates", "list_themes", "list_versions", "list_workspaces", "page_urls",
	"patch_page", "preflight_page", "prepare_dashboard_update", "publish_page", "record_refresh_check",
	"register_template_upload", "rename_workspace", "rerender_page_from_template", "rollback_page",
	"set_page_refresh_schedule", "set_page_workspace", "set_password", "set_title", "start_page_upload", "template_urls", "update_page",
	"update_page_config", "update_page_data", "update_page_data_upload", "validate_template",
}

func TestEndpointBoundariesAndDiscovery(t *testing.T) {
	h := newHarness(t)

	// 1. Bearer auth is explicit, non-cacheable, and RFC-shaped.
	noAuth := h.initialize(protocol, rpcOpts{reqOpts: reqOpts{noToken: true}})
	eq(t, noAuth.status, 401, "no token -> 401")
	mustMatch(t, `^Bearer\b`, noAuth.header.Get("WWW-Authenticate"))
	mustMatch(t, `no-store`, noAuth.header.Get("Cache-Control"))
	eq(t, noAuth.get("error", "code"), -32001)

	// 2. Current stable and legacy clients both negotiate truthfully.
	init := h.initialize(protocol, rpcOpts{})
	eq(t, init.status, 200, init.body)
	eq(t, init.get("result", "protocolVersion"), "2025-11-25")
	eq(t, init.get("result", "serverInfo", "name"), "pages")
	eq(t, init.get("result", "serverInfo", "title"), "Elcano Pages")
	instructions := strAt(init.json, "result", "instructions")
	for _, p := range []string{`list_workspaces`, `Otherwise prefer create_upload_ticket`, `If outbound HTTP is unavailable, use start_page_upload`,
		`(?i)never pass a path, \$\(cat`, `(?i)update <slug> dashboard with`, `prepare_dashboard_update`, `(?i)do not execute it now`} {
		mustMatch(t, p, instructions)
	}
	eq(t, init.get("result", "capabilities", "tools", "listChanged"), false)
	legacy := h.initialize("2024-11-05", rpcOpts{})
	eq(t, legacy.status, 200, legacy.body)
	eq(t, legacy.get("result", "protocolVersion"), "2024-11-05")
	initialized := h.rpc("notifications/initialized", o(), rpcOpts{notification: true})
	eq(t, initialized.status, 202)
	eq(t, initialized.body, "")

	// 3. Static tool discovery is complete and richly described.
	listed := h.rpc("tools/list", o(), rpcOpts{})
	eq(t, listed.status, 200, listed.body)
	advertised, _ := listed.get("result", "tools").([]any)
	var names []string
	for _, tool := range advertised {
		name := strAt(tool, "name")
		names = append(names, name)
		if strAt(tool, "title") == "" || strAt(tool, "description") == "" {
			t.Errorf("%s lacks a title or description", name)
		}
		eq(t, dig(tool, "inputSchema", "type"), "object", name)
		eq(t, dig(tool, "outputSchema", "type"), "object", name)
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := dig(tool, "annotations", hint).(bool); !ok {
				t.Errorf("%s does not declare %s", name, hint)
			}
		}
	}
	sort.Strings(names)
	eq(t, names, expectedTools)
	if slices.Contains(names, "delete_workspace") {
		t.Fatal("bulk delete remains human-only")
	}
	eq(t, h.rpc("tools/list", o("cursor", "never-issued"), rpcOpts{}).get("error", "code"), -32602, "static tools/list rejects unissued cursors")

	// 4. Streamable HTTP security and media-type boundaries fail closed.
	badOrigin := h.rpc("ping", o(), rpcOpts{reqOpts: reqOpts{origin: ptr("https://evil.example")}})
	eq(t, badOrigin.status, 403)
	eq(t, badOrigin.get("error", "code"), -32000)
	eq(t, h.rpc("ping", o(), rpcOpts{reqOpts: reqOpts{origin: ptr("http://localhost:3100/path")}}).status, 403)
	normalized := h.rpc("ping", o(), rpcOpts{reqOpts: reqOpts{origin: ptr("HTTP://LOCALHOST:3100")}})
	eq(t, normalized.status, 200, "normalized-equivalent allowed origin succeeds")
	eq(t, normalized.header.Get("Access-Control-Allow-Origin"), "http://localhost:3100")
	eq(t, h.rpc("ping", o(), rpcOpts{reqOpts: reqOpts{host: "evil.example"}}).status, 403)
	eq(t, h.rpc("ping", o(), rpcOpts{reqOpts: reqOpts{host: "evil@localhost"}}).status, 403, "Host userinfo is rejected")
	eq(t, h.rpc("ping", o(), rpcOpts{reqOpts: reqOpts{accept: ptr("")}}).status, 406)
	wrongType := h.request("POST", "/mcp", reqOpts{contentType: ptr("text/plain"), headers: map[string]string{"MCP-Protocol-Version": protocol},
		body: o("jsonrpc", "2.0", "id", num(900), "method", "ping")})
	eq(t, wrongType.status, 415, wrongType.body)
	eq(t, h.rpc("ping", o(), rpcOpts{protocolHeader: ptr("2099-01-01")}).status, 400)

	// 5. Malformed JSON, batches, invalid envelopes and request ids get
	// protocol errors.
	pvh := map[string]string{"MCP-Protocol-Version": protocol}
	malformed := h.request("POST", "/mcp", reqOpts{rawBody: ptr("{"), headers: pvh})
	eq(t, malformed.status, 400)
	eq(t, malformed.get("error", "code"), -32700)
	batch := h.request("POST", "/mcp", reqOpts{body: arr(o("jsonrpc", "2.0", "id", num(1), "method", "ping")), headers: pvh})
	eq(t, batch.status, 400)
	eq(t, batch.get("error", "code"), -32600)
	for _, envelope := range []any{
		o("jsonrpc", "2.0", "id", nil, "method", "ping"),
		o("jsonrpc", "2.0", "id", o("bad", true), "method", "ping"),
		o("jsonrpc", "1.0", "id", num(71), "method", "ping"),
		o("jsonrpc", "2.0", "id", num(72)),
	} {
		eq(t, h.request("POST", "/mcp", reqOpts{body: envelope, headers: pvh}).get("error", "code"), -32600, "invalid envelope rejected")
	}
	eq(t, h.initialize("2099-01-01", rpcOpts{}).get("result", "protocolVersion"), protocol, "unsupported version -> latest")
	notificationBatch := h.request("POST", "/mcp", reqOpts{body: arr(o("jsonrpc", "2.0", "method", "notifications/initialized")), headers: pvh})
	eq(t, notificationBatch.status, 400, "a notification-only batch is still a batch")
	eq(t, notificationBatch.get("error", "code"), -32600)
	emptyBody := h.request("POST", "/mcp", reqOpts{rawBody: ptr(""), headers: pvh})
	eq(t, emptyBody.status, 400)
	eq(t, emptyBody.get("error", "code"), -32600)
	clientResponse := h.request("POST", "/mcp", reqOpts{body: o("jsonrpc", "2.0", "id", num(4242), "result", o()), headers: pvh})
	eq(t, clientResponse.status, 202, "a client's JSON-RPC response is accepted without a reply")
	eq(t, clientResponse.body, "")
	eq(t, h.rpc("notifications/frobnicated", o(), rpcOpts{notification: true}).status, 202)
	unknownNoParams := h.rpc("resources/list", nil, rpcOpts{})
	eq(t, unknownNoParams.get("error", "code"), -32601)
	eq(t, unknownNoParams.get("error", "message"), "Method not found")
	eq(t, h.rpc("ping", o("task", o()), rpcOpts{}).get("error", "code"), -32603, "task augmentation is refused: no tasks capability")
	pinged := h.rpc("ping", o(), rpcOpts{})
	eq(t, pinged.json, map[string]any{"result": map[string]any{}, "jsonrpc": "2.0", "id": pinged.get("id")})

	// 6. A request-only method shaped as a notification cannot mutate.
	notificationCall := h.call("deploy_page", o("slug", "notification-must-not-run", "html", pageHTML(99)), rpcOpts{notification: true})
	eq(t, notificationCall.status, 202)
	eq(t, notificationCall.body, "")
	eq(t, h.fails(h.call("get_page", o("slug", "notification-must-not-run"))).code(), "page_not_found")

	// 7. Unknown and prototype-named tools are protocol Invalid Params.
	for _, name := range []string{"no_such_tool", "constructor", "__proto__"} {
		eq(t, h.call(name, o()).get("error", "code"), -32602, name)
	}
	eq(t, h.rpc("frobnicate", o(), rpcOpts{}).get("error", "code"), -32601)

	// 8. GET/DELETE are authenticated but unsupported.
	for _, method := range []string{"GET", "DELETE"} {
		eq(t, h.request(method, "/mcp", reqOpts{noToken: true}).status, 401, method+" requires auth")
		authed := h.request(method, "/mcp", reqOpts{})
		eq(t, authed.status, 405, method+" -> 405 after auth")
		eq(t, authed.header.Get("Allow"), "POST, OPTIONS")
		eq(t, authed.get("error", "code"), -32000)
	}
	preflight := h.request("OPTIONS", "/mcp", reqOpts{noToken: true, origin: ptr("http://localhost:3100"), headers: map[string]string{
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "authorization,content-type,mcp-protocol-version",
	}})
	eq(t, preflight.status, 204, "allowed CORS preflight does not require bearer auth")
	eq(t, preflight.header.Get("Access-Control-Allow-Origin"), "http://localhost:3100")
	eq(t, preflight.header.Get("Access-Control-Allow-Methods"), "POST, OPTIONS")
}

func TestDeployReadAndGuide(t *testing.T) {
	h := newHarness(t)

	// 9. Create-and-publish returns serving truth and exact URLs.
	deployed := h.data(h.call("deploy_page", o("slug", "mcpdemo", "title", "MCP Demo", "html", pageHTML(1), "publish", true)))
	eq(t, deployed["created"], true)
	eq(t, deployed["published"], true)
	eq(t, dig(deployed, "version", "status"), "approved")
	if _, ok := dig(deployed, "version", "id").(string); !ok {
		t.Fatal("BIGINT ids are canonical decimal strings")
	}
	eq(t, deployed["version_is_live"], true)
	eq(t, deployed["page_is_live"], true)
	v1 := strAt(deployed, "version", "id")
	eq(t, deployed["live_version_id"], v1)
	eq(t, dig(deployed, "urls", "admin"), "http://localhost:3100/admin/mcpdemo")
	eq(t, dig(deployed, "urls", "view"), "http://localhost:3100/view/mcpdemo")
	eq(t, dig(deployed, "urls", "live"), "http://content.localhost:3100/mcpdemo")

	page := h.data(h.call("get_page", o("slug", "mcpdemo")))
	eq(t, dig(page, "page", "published_version_id"), v1)
	eq(t, dig(page, "published", "id"), v1)
	if _, has := dig(page, "published").(map[string]any)["html"]; has {
		t.Fatal("get_page omits HTML by default")
	}
	mustMatch(t, `v1`, strAt(h.data(h.call("get_page", o("slug", "mcpdemo", "include_html", true))), "published", "html"))
	eq(t, h.data(h.call("page_urls", o("slug", "mcpdemo")))["live"], dig(deployed, "urls", "live"))
	themes := h.data(h.call("list_themes", o()))["themes"].([]any)
	if !slices.ContainsFunc(themes, func(th any) bool { return strAt(th, "name") == "flag" }) {
		t.Fatal("the flag theme is listed")
	}

	// get_guide is the one tool whose text is not the JSON of its result:
	// hosts that pass only text to the model get the Markdown itself.
	guideIndex := h.call("get_guide", o())
	if guideIndex.get("result", "isError") != nil {
		t.Fatal(guideIndex.body)
	}
	mustMatch(t, `^# Pages agent guide`, strAt(guideIndex.json, "result", "content", 0, "text"))
	var topics []string
	for _, e := range guideIndex.get("result", "structuredContent", "entries").([]any) {
		topics = append(topics, strAt(e, "topic"))
	}
	eq(t, topics, []string{"workflow", "authoring", "templates", "data_updates", "motion"})
	section := h.call("get_guide", o("topic", "workflow", "section", "changing-a-live-page"))
	text := strAt(section.json, "result", "content", 0, "text")
	content := strAt(section.json, "result", "structuredContent", "content")
	if !strings.HasPrefix(text, content) {
		t.Fatal("a section's text starts with its content")
	}
	mustMatch(t, `Other sections in workflow: `, text)
	sum := sha256.Sum256([]byte(content))
	eq(t, hex.EncodeToString(sum[:]), strAt(section.json, "result", "structuredContent", "content_sha256"))
	missing := h.fails(h.call("get_guide", o("topic", "workflow", "section", "no-such-section")))
	eq(t, missing.code(), "guide_section_not_found")
	if !slices.Contains(toStrings(dig(missing.data, "details", "available")), "changing-a-live-page") {
		t.Fatal("a miss names the valid sections")
	}
}

func toStrings(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, e := range a {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}
