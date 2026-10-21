// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Port of test/data-limits.integration.js: with deliberately small capacity
// settings, the managed-data limits distinguish payload, escaped envelope and
// transport, and every refusal is atomic. Tools are called in process (as the
// JavaScript did), except the request-size boundary, which is checked
// through the real /mcp route.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/versions"
)

// tool runs a tool in process: arguments through the input schema, the
// handler, then the JSON round trip held to the output schema.
func (h *harness) tool(name string, args *jsonv.Object) (*jsonv.Object, error) {
	h.t.Helper()
	def := currentCatalog().byName[name]
	parsed, err := def.input.Parse(args)
	if err != nil {
		h.t.Fatalf("%s arguments: %v", name, err)
	}
	c := &call{d: h.d, agent: nil, actor: versions.Actor{Actor: "data-limit-fixture", ActorType: "agent", TokenID: h.tokenID}}
	out, err := def.handler(context.Background(), c, parsed.(*jsonv.Object))
	if err != nil {
		return nil, err
	}
	v, err := JSONValue(out)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := def.output.Parse(v); err != nil {
		h.t.Fatalf("%s output: %v", name, err)
	}
	return v.(*jsonv.Object), nil
}

func (h *harness) mustTool(name string, args *jsonv.Object) *jsonv.Object {
	h.t.Helper()
	out, err := h.tool(name, args)
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	return out
}

func escapedJSON(v any) string {
	return regexp.MustCompile("[<>&  ]").ReplaceAllStringFunc(jsonv.Stringify(v), func(c string) string {
		return fmt.Sprintf(`\u%04x`, []rune(c)[0])
	})
}

func TestManagedDataLimits(t *testing.T) {
	t.Setenv("PAGES_DATA_MAX_BYTES", "2048")
	t.Setenv("PAGES_MCP_MAX_INLINE_DATA_BYTES", "1024")
	t.Setenv("MAX_HTML_BYTES", "4kb")
	t.Setenv("PAGES_UPLOAD_MAX_BYTES", "2097152")
	h := newHarness(t)
	const slug = "data-size-contract"
	const sourceAsOf = "2026-08-01T00:00:00.000Z"
	envelope := func(data any) *jsonv.Object {
		return o("contract_version", num(1), "refreshed_at", sourceAsOf, "source_as_of", sourceAsOf, "data", data)
	}
	measure := func(data any) *jsonv.Object {
		return o("payload_bytes", num(float64(len(jsonv.Stringify(data)))), "escaped_payload_bytes", num(float64(len(escapedJSON(data)))))
	}
	overhead := len(escapedJSON(envelope(o()))) - len(escapedJSON(o()))
	limits := fmt.Sprintf(`{"max_payload_bytes":2048,"max_envelope_bytes":2048,"max_inline_bytes":1024,"max_staged_bytes":2097152,"max_request_bytes":4096,"min_envelope_overhead_bytes":%d}`, overhead)
	payloadOfBytes := func(n int) *jsonv.Object {
		v := o("value", "")
		v.Set("value", strings.Repeat("a", n-len(jsonv.Stringify(v))))
		if len(jsonv.Stringify(v)) != n {
			t.Fatal("payload fixture")
		}
		return v
	}
	schema := o("$schema", "https://json-schema.org/draft/2020-12/schema", "type", "object", "required", arr("value"), "additionalProperties", false,
		"properties", o("value", o("type", "string")))
	html := "<!doctype html><html><head><title>Northwind</title></head><body><h1>Northwind</h1>" +
		`<script id="pages-data-schema" type="application/schema+json">` + escapedJSON(schema) + `</script>` +
		`<script id="pages-data" type="application/json">` + escapedJSON(envelope(o("value", "initial"))) + `</script></body></html>`
	snapshot := func() string {
		return jsonv.Stringify(h.query(`SELECT
		  (SELECT count(*)::int FROM page_content_uploads) AS uploads,
		  (SELECT count(*)::int FROM page_versions) AS versions,
		  (SELECT count(*)::int FROM audit_log) AS audit,
		  (SELECT coalesce(sum(starts),0)::int FROM page_upload_attempts) AS starts,
		  (SELECT published_version_id::text FROM pages WHERE slug=$1) AS live`, slug)[0])
	}
	domainError := func(err error, boundary string, n int) {
		t.Helper()
		e, ok := apierror.As(err)
		if !ok {
			t.Fatalf("not an API error: %v", err)
		}
		eq(t, e.Status, 400)
		eq(t, e.Code, "data_validation_failed")
		d, _ := toValue(e.Details)
		details := d.(*jsonv.Object)
		eq(t, details.Value("size_limit"), boundary)
		eq(t, details.Value("bytes"), n)
		eq(t, details.Value("max_bytes"), 2048)
		eq(t, details.Value("changing_transport_can_help"), false)
		eq(t, jsonv.Stringify(details.Value("limits")), limits)
		mustMatch(t, `(?i)limit|exceed|large`, e.Message)
	}

	live := h.mustTool("deploy_page", o("slug", slug, "html", html)).Value("version").(*jsonv.Object).Value("id")
	update := func(data any) (*jsonv.Object, error) {
		return h.tool("update_page_data", o("slug", slug, "data", data, "source_as_of", sourceAsOf, "expected_version", live))
	}
	startArgs := func(text string, dataSize any) *jsonv.Object {
		a := o("slug", slug, "kind", "data", "total_bytes", num(float64(len(text))), "content_sha256", shaHex([]byte(text)))
		if dataSize != nil {
			a.Set("data_size", dataSize)
		}
		return a
	}
	stage := func(text string, dataSize any) *jsonv.Object {
		up := h.mustTool("start_page_upload", startArgs(text, dataSize))
		eq(t, jsonv.Stringify(up.Value("data_limits")), limits)
		appended := h.mustTool("append_page_upload", o("upload_id", up.Value("upload_id"), "sequence", num(0), "chunk_base64", base64.StdEncoding.EncodeToString([]byte(text))))
		eq(t, appended.Value("complete"), true)
		eq(t, jsonv.Stringify(appended.Value("data_limits")), limits)
		return up
	}
	consume := func(up *jsonv.Object) (*jsonv.Object, error) {
		return h.tool("update_page_data_upload", o("upload_id", up.Value("upload_id"), "slug", slug, "source_as_of", sourceAsOf, "expected_version", live))
	}
	mustConsume := func(up *jsonv.Object) *jsonv.Object {
		out, err := consume(up)
		if err != nil {
			t.Fatal(err)
		}
		live = out.Value("version").(*jsonv.Object).Value("id")
		return out
	}
	storedData := func() string {
		r, err := versions.GetPageData(context.Background(), h.d, slug)
		if err != nil {
			t.Fatal(err)
		}
		return jsonv.Stringify(r.Envelope.Value("data"))
	}

	// The request cap counts the entire HTTP JSON document.
	readRequest := jsonv.Stringify(o("jsonrpc", "2.0", "id", num(1), "method", "tools/call", "params", o("name", "get_page", "arguments", o("slug", slug))))
	exact := readRequest + strings.Repeat(" ", 4096-len(readRequest))
	accepted := h.request("POST", "/mcp", reqOpts{rawBody: &exact})
	eq(t, accepted.status, 200)
	if accepted.get("result", "isError") == true {
		t.Fatal(accepted.body)
	}
	before := snapshot()
	over := exact + " "
	refused := h.request("POST", "/mcp", reqOpts{rawBody: &over})
	eq(t, refused.status, http.StatusRequestEntityTooLarge)
	eq(t, refused.get("error", "code"), -32000)
	mustMatch(t, `(?i)body too large`, strAt(refused.json, "error", "message"))
	eq(t, snapshot(), before)

	// Direct PUT has the staged-file limit; whitespace keeps the compact size.
	ticketData := o("value", "Northwind direct file upload")
	ticketText := strings.Repeat(" ", 5000) + "{\n  \"value\": \"Northwind direct file upload\"\n}\n"
	ticket := h.mustTool("create_upload_ticket", startArgs(ticketText, measure(ticketData)))
	eq(t, jsonv.Stringify(ticket.Value("data_limits")), limits)
	sent := h.request("PUT", "/upload/"+ticket.Value("upload_id").(string), reqOpts{rawBody: &ticketText, token: ticket.Value("ticket").(string), contentType: ptr("application/octet-stream")})
	eq(t, sent.status, 200, sent.body)
	eq(t, sent.get("complete"), true)
	eq(t, sent.get("bytes_received"), len(ticketText))
	mustConsume(ticket)
	eq(t, storedData(), jsonv.Stringify(ticketData))

	tooMuchPayload := payloadOfBytes(2049)
	tooMuchEnvelope := payloadOfBytes(2048 - overhead + 1)
	escapedOverflow := o("value", strings.Repeat("<>&  é", 90))
	if len(jsonv.Stringify(escapedOverflow)) >= 2048 || len(escapedJSON(envelope(escapedOverflow))) <= 2048 {
		t.Fatal("escaped overflow fixture")
	}
	domainCases := []struct {
		data     *jsonv.Object
		boundary string
		n        int
	}{
		{tooMuchPayload, "payload", 2049},
		{tooMuchEnvelope, "envelope", 2049},
		{escapedOverflow, "envelope", len(escapedJSON(envelope(escapedOverflow)))},
	}
	for _, c := range domainCases {
		before := snapshot()
		_, err := update(c.data)
		domainError(err, c.boundary, c.n)
		for _, name := range []string{"start_page_upload", "create_upload_ticket"} {
			_, err := h.tool(name, startArgs(jsonv.Stringify(c.data), measure(c.data)))
			domainError(err, c.boundary, c.n)
		}
		eq(t, snapshot(), before, "known-impossible data must not reserve uploads, versions or audit rows")
	}

	// Only the inline transport too small: staging still publishes it.
	exactInline, err := update(payloadOfBytes(1024))
	if err != nil {
		t.Fatal(err)
	}
	live = exactInline.Value("version").(*jsonv.Object).Value("id")
	oneOver := payloadOfBytes(1025)
	before = snapshot()
	_, err = update(oneOver)
	e, _ := apierror.As(err)
	if e == nil || e.Code != "data_too_large_for_inline" {
		t.Fatalf("inline refusal: %v", err)
	}
	d, _ := toValue(e.Details)
	eq(t, d.(*jsonv.Object).Value("changing_transport_can_help"), true)
	eq(t, jsonv.Stringify(d.(*jsonv.Object).Value("limits")), limits)
	eq(t, snapshot(), before)
	mustConsume(stage(jsonv.Stringify(oneOver), measure(oneOver)))

	prettyData := o("value", "Northwind complete data")
	prettyText := strings.Repeat(" ", 2100) + "{\n  \"value\": \"Northwind complete data\"\n}\n"
	mustConsume(stage(prettyText, measure(prettyData)))
	eq(t, storedData(), jsonv.Stringify(prettyData))

	for _, envelopeBytes := range []int{2047, 2048} {
		data := payloadOfBytes(envelopeBytes - overhead)
		saved := mustConsume(stage(jsonv.Stringify(data), measure(data)))
		// Issue #109: a write at the exact limit reports zero headroom.
		usage := saved.Value("data_usage").(*jsonv.Object)
		eq(t, usage.Value("envelope_bytes"), envelopeBytes)
		eq(t, usage.Value("headroom_bytes"), 2048-envelopeBytes)
		stored := h.scalar("SELECT html FROM page_versions WHERE id=$1", live).(string)
		m, err := pagedata.ParseManagedHTML(stored)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, len(escapedJSON(m.Envelope)), envelopeBytes)
		eq(t, jsonv.Stringify(m.Data()), jsonv.Stringify(data))
	}

	// Omitted or understated local measurements never bypass consume-time
	// validation, nor do failed commits spend the upload handle.
	for _, c := range domainCases {
		for _, claims := range []any{nil, o("payload_bytes", num(2), "escaped_payload_bytes", num(2))} {
			up := stage(jsonv.Stringify(c.data), claims)
			before := snapshot()
			_, err := consume(up)
			domainError(err, c.boundary, c.n)
			eq(t, snapshot(), before, "failed materialization is atomic")
			row := h.query("SELECT committed_at, commit_result, bytes_received FROM page_content_uploads WHERE id=$1", up.Value("upload_id"))[0]
			eq(t, row["committed_at"], nil)
			eq(t, row["commit_result"], nil)
			eq(t, row["bytes_received"], len(jsonv.Stringify(c.data)))
			h.mustTool("cancel_page_upload", o("upload_id", up.Value("upload_id")))
		}
	}
}
