// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Port of test/mcp.integration.js: preflight, patch_page, and the staged
// upload path (chunked appends, tickets, races, atomic commits).

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/uploads"
	"github.com/ElcanoTek/pages/internal/versions"
)

// brokenHTML reproduces a bug that shipped to a live client dashboard:
// onclick="togglePopover()" resolves to HTMLElement.togglePopover via the
// inline-handler scope chain, throws on click, and the date-range picker
// silently never opens.
const brokenHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Broken</title></head><body>` +
	`<button id="t" onclick="togglePopover()">Date range</button>` +
	`<img src="https://cdn.example.com/logo.svg">` +
	"<script>function togglePopover(){document.getElementById('t').dataset.open=1;}</script>" +
	"</body></html>"

func errorCodes(v any) []string {
	var out []string
	for _, e := range toAnySlice(v) {
		out = append(out, strAt(e, "code"))
	}
	slices.Sort(out)
	return out
}

func toAnySlice(v any) []any {
	a, _ := v.([]any)
	return a
}

func TestPreflightAndPatch(t *testing.T) {
	h := newHarness(t)

	brokenDeploy := h.data(h.call("deploy_page", o("slug", "mcp-preflight", "title", "Preflight", "html", brokenHTML, "render_mode", "raw", "publish", true)))
	eq(t, brokenDeploy["version_is_live"], true, "preflight is advisory — it must never block a deploy")
	eq(t, dig(brokenDeploy, "preflight", "ok"), false, "deploy results carry the findings without a second call")
	eq(t, errorCodes(dig(brokenDeploy, "preflight", "errors")), []string{"inline_handler_shadowed", "remote_subresource_blocked"})

	checked := h.data(h.call("preflight_page", o("slug", "mcp-preflight")))
	eq(t, checked["version_id"], dig(brokenDeploy, "version", "id"), "defaults to the published version")
	eq(t, dig(checked, "preflight", "ok"), false)
	var shadowed any
	for _, e := range toAnySlice(dig(checked, "preflight", "errors")) {
		if strAt(e, "code") == "inline_handler_shadowed" {
			shadowed = e
		}
	}
	eq(t, strAt(shadowed, "identifier"), "togglePopover")
	if strAt(shadowed, "fix") == "" {
		t.Fatal("every finding carries a concrete fix")
	}
	if strings.Contains(jsonText(checked), "<!doctype html>") {
		t.Fatal("preflight reports findings, never the document")
	}

	fixed := strings.ReplaceAll(brokenHTML, "togglePopover", "drTogglePopover")
	fixed = regexp.MustCompile(`<img[^>]*>`).ReplaceAllString(fixed, "")
	fixed = strings.Replace(fixed, "</body>", `<script>const nav = document.getElementById("pages-nav"); if (nav) JSON.parse(nav.textContent);</script></body>`, 1)
	fixedDeploy := h.data(h.call("update_page", o("slug", "mcp-preflight", "html", fixed, "render_mode", "raw", "publish", true)))
	eq(t, dig(fixedDeploy, "preflight", "ok"), true, jsonText(dig(fixedDeploy, "preflight", "errors")))
	eq(t, dig(fixedDeploy, "version", "render_mode"), "raw", "a supported navigation reader keeps its design mode")
	for _, src := range []map[string]any{fixedDeploy, h.data(h.call("preflight_page", o("slug", "mcp-preflight")))} {
		for _, w := range toAnySlice(dig(src, "preflight", "warnings")) {
			if strings.HasPrefix(strAt(w, "code"), "nav_block_") {
				t.Fatalf("unexpected navigation warning %v", w)
			}
		}
	}

	// patch_page: locate the anchor, send a few bytes of edit, get a new
	// immutable version — instead of reading the document back and
	// re-emitting all of it.
	patchTarget := h.data(h.call("deploy_page", o("slug", "mcp-patch", "title", "Patch Target", "html", brokenHTML, "render_mode", "raw", "publish", true)))
	eq(t, dig(patchTarget, "preflight", "ok"), false)
	located := h.data(h.call("find_in_version", o("slug", "mcp-patch", "query", "togglePopover")))
	eq(t, located["total_matches"], 2, "the onclick attribute and the declaration")
	if numAt(located, "matches", 0, "line") < 1 {
		t.Fatal("matches carry a line")
	}
	for _, m := range toAnySlice(located["matches"]) {
		if len([]rune(strAt(m, "excerpt"))) > 260 {
			t.Fatal("excerpts stay bounded")
		}
	}
	patched := h.data(h.call("patch_page", o("slug", "mcp-patch",
		"edits", arr(o("find", "togglePopover", "replace", "drTogglePopover", "count", num(2))),
		"note", "rename the shadowed inline handler")))
	eq(t, patched["version_is_live"], true)
	eq(t, dig(patched, "patch", "edits_applied", 0, "count"), 2)
	eq(t, numAt(patched, "patch", "bytes_after")-numAt(patched, "patch", "bytes_before"), 4)
	if slices.Contains(errorCodes(dig(patched, "preflight", "errors")), "inline_handler_shadowed") {
		t.Fatal("the patch actually cleared the defect preflight reported")
	}
	patchedHTML := h.scalar("SELECT html FROM page_versions WHERE id = $1", strAt(patched, "version", "id")).(string)
	if !strings.Contains(patchedHTML, `onclick="drTogglePopover()"`) || regexp.MustCompile(`[^r]togglePopover`).MatchString(patchedHTML) {
		t.Fatal("no stale references left behind")
	}

	// An anchor that no longer matches is a hard error, never a no-op.
	stale := h.fails(h.call("patch_page", o("slug", "mcp-patch", "edits", arr(o("find", "togglePopover(", "replace", "x(")))))
	eq(t, stale.code(), "patch_anchor_mismatch")
	eq(t, dig(stale.data, "details", "actual_count"), 0)

	conflicting := h.fails(h.call("patch_page", o("slug", "mcp-patch",
		"edits", arr(o("find", "drTogglePopover", "replace", "zz", "count", num(2))), "expected_version", "1")))
	eq(t, conflicting.code(), "stale_version")
	eq(t, dig(conflicting.data, "details", "expected_version"), "1")
	eq(t, dig(conflicting.data, "details", "retry_with_expected_version"), strAt(patched, "version", "id"))
	mustMatch(t, `confirm version`, strAt(conflicting.data, "error"))
	eq(t, dig(h.data(h.call("get_page", o("slug", "mcp-patch"))), "page", "published_version_id"), dig(patched, "version", "id"),
		"a rejected patch leaves the live pointer exactly where it was")

	// base_version_id names the version to patch FROM; the concurrency
	// default is the live pointer, so an explicit base must not fail
	// stale_version against a page nobody else touched.
	beforeRebase := h.data(h.call("get_page", o("slug", "mcp-patch")))
	livePointer := strAt(beforeRebase, "page", "published_version_id")
	var older string
	for _, v := range toAnySlice(h.data(h.call("list_versions", o("slug", "mcp-patch")))["versions"]) {
		if strAt(v, "id") != livePointer {
			older = strAt(v, "id")
			break
		}
	}
	if older == "" {
		t.Fatal("fixture: the page has an earlier version to patch from")
	}
	olderHTML := strAt(h.data(h.call("get_version", o("slug", "mcp-patch", "version_id", older))), "version", "html")
	anchor := "<body>"
	if strings.Contains(olderHTML, "<main>") {
		anchor = "<main>"
	}
	rebased := h.data(h.call("patch_page", o("slug", "mcp-patch", "base_version_id", older,
		"edits", arr(o("find", anchor, "replace", anchor+"<!-- rebased -->")), "publish", false)))
	eq(t, dig(rebased, "patch", "base_version_id"), older, "it patched the version it was told to")
	eq(t, rebased["version_is_live"], false, "publish:false leaves the pointer alone")
	eq(t, dig(h.data(h.call("get_page", o("slug", "mcp-patch"))), "page", "published_version_id"), livePointer, "the live pointer really did not move")
	publishedRebase := h.data(h.call("patch_page", o("slug", "mcp-patch", "base_version_id", older,
		"edits", arr(o("find", anchor, "replace", anchor+"<!-- rebased-live -->")), "publish", true)))
	eq(t, publishedRebase["version_is_live"], true, "a published rebase goes live")
	if strAt(publishedRebase, "version", "id") == livePointer {
		t.Fatal("a published rebase is a new version")
	}
	stalePatch := h.fails(h.call("patch_page", o("slug", "mcp-patch", "base_version_id", older,
		"edits", arr(o("find", anchor, "replace", anchor+"<!-- nope -->")), "expected_version", older)))
	eq(t, stalePatch.code(), "stale_version", "an explicitly stale expected_version is still refused")
}

func largeDashboard() string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>Large MCP</title></head><body><main>`)
	for i := range 1800 {
		fmt.Fprintf(&b, `<section data-row="%d">Dashboard row %d: complete source data ☀</section>`, i, i)
	}
	b.WriteString(`</main><script>window.dashboardReady=true;</script></body></html>`)
	return b.String()
}

func TestStagedUploads(t *testing.T) {
	h := newHarness(t)
	// The Node release's chunk size, so the ~140 KB fixture spans several
	// chunks (the raised default would carry it in one).
	t.Setenv("PAGE_UPLOAD_MAX_CHUNK_BYTES", "49152")

	largeHTML := largeDashboard()
	large := []byte(largeHTML)
	if len(large) <= 100_000 {
		t.Fatal("fixture is representative of the failed production dashboards")
	}
	largeSHA := shaHex(large)
	upload := h.data(h.call("start_page_upload", o("slug", "mcp-large-upload", "total_bytes", num(float64(len(large))), "content_sha256", largeSHA)))
	eq(t, upload["slug"], "mcp-large-upload")
	eq(t, upload["bytes_received"], 0)
	eq(t, upload["next_sequence"], 0)
	eq(t, upload["complete"], false)
	eq(t, upload["max_chunk_bytes"], uploads.MaxChunkBytes())
	mustMatch(t, `append_page_upload`, strAt(upload, "next_step"))
	uploadID := strAt(upload, "upload_id")
	chunkBytes := int(numAt(upload, "max_chunk_bytes"))
	// Several full chunks, so the sequence logic is exercised at the raised
	// chunk size too.
	if len(large) <= 2*chunkBytes {
		t.Fatalf("fixture (%d bytes) needs at least three %d-byte chunks", len(large), chunkBytes)
	}

	eq(t, h.fails(h.call("deploy_page_upload", o("upload_id", uploadID, "render_mode", "raw"))).code(), "page_upload_incomplete")
	firstChunk := base64.StdEncoding.EncodeToString(large[:chunkBytes])
	outOfOrder := h.fails(h.call("append_page_upload", o("upload_id", uploadID, "sequence", num(1), "chunk_base64", firstChunk)))
	eq(t, outOfOrder.code(), "upload_sequence_conflict")
	eq(t, dig(outOfOrder.data, "details", "expected_sequence"), 0)
	// The cancels in an observed 10M-token session were all recovery
	// attempts from exactly this error; it now says the cheap recovery
	// exists.
	eq(t, dig(outOfOrder.data, "details", "resumable"), true)
	eq(t, dig(outOfOrder.data, "details", "bytes_received"), 0)
	eq(t, dig(outOfOrder.data, "details", "total_bytes"), len(large))
	mustMatch(t, `Do NOT cancel`, strAt(outOfOrder.data, "error"))
	mustMatch(t, `A FAILED append does not advance the sequence`, strAt(outOfOrder.data, "error"))
	resumed := h.data(h.call("append_page_upload", o("upload_id", uploadID, "sequence", num(0), "chunk_base64", firstChunk)))
	eq(t, resumed["next_sequence"], 1, "the conflict cost nothing but the one call")
	eq(t, resumed["deduped"], false)
	replayed := h.data(h.call("append_page_upload", o("upload_id", uploadID, "sequence", num(0), "chunk_base64", firstChunk)))
	eq(t, replayed["deduped"], true)
	eq(t, replayed["next_sequence"], 1)
	contradicted := h.fails(h.call("append_page_upload", o("upload_id", uploadID, "sequence", num(0),
		"chunk_base64", base64.StdEncoding.EncodeToString([]byte("different")))))
	eq(t, contradicted.code(), "upload_sequence_conflict")
	eq(t, dig(contradicted.data, "details", "resumable"), true)
	eq(t, dig(contradicted.data, "details", "expected_sequence"), 1)
	mustMatch(t, `DIFFERENT bytes`, strAt(contradicted.data, "error"))

	other := h.mint("mcp-upload-other", tokens.ScopeDeploy)
	eq(t, h.fails(h.call("append_page_upload", o("upload_id", uploadID, "sequence", num(0), "chunk_base64", firstChunk), as(other.Token))).code(),
		"page_upload_not_found", "upload handles are token-bound")

	var final map[string]any
	sequence := 1
	for offset := chunkBytes; offset < len(large); offset += chunkBytes {
		chunk := base64.StdEncoding.EncodeToString(large[offset:min(offset+chunkBytes, len(large))])
		final = h.data(h.call("append_page_upload", o("upload_id", uploadID, "sequence", num(float64(sequence)), "chunk_base64", chunk)))
		if sequence == 1 {
			replay := h.data(h.call("append_page_upload", o("upload_id", uploadID, "sequence", num(float64(sequence)), "chunk_base64", chunk)))
			eq(t, replay["deduped"], true, "ambiguous chunk retries are idempotent")
			eq(t, replay["next_sequence"], 2)
		}
		sequence++
	}
	eq(t, final["complete"], true)
	eq(t, final["bytes_received"], len(large))
	eq(t, final["next_sequence"], sequence)
	mustMatch(t, `deploy_page_upload`, strAt(final, "next_step"))

	stagedArgs := func() any {
		return o("upload_id", uploadID, "title", "Large MCP Dashboard", "render_mode", "raw", "publish", true, "note", "Staged exact-byte integration deploy")
	}
	staged := h.data(h.call("deploy_page_upload", stagedArgs()))
	eq(t, staged["upload_id"], uploadID)
	eq(t, staged["created"], true)
	eq(t, staged["version_is_live"], true)
	eq(t, staged["page_is_live"], true)
	eq(t, dig(staged, "version", "content_sha256"), largeSHA)
	stagedReplay := h.data(h.call("deploy_page_upload", stagedArgs()))
	eq(t, stagedReplay["replayed"], true)
	eq(t, stagedReplay["version"], staged["version"], "committed upload retries preserve the original version receipt")
	eq(t, stagedReplay["version_is_live"], true)
	mustMatch(t, `did not republish`, strAt(stagedReplay, "next_step"))
	changed := o("upload_id", uploadID, "title", "Large MCP Dashboard", "render_mode", "themed", "publish", true, "note", "Staged exact-byte integration deploy")
	eq(t, h.fails(h.call("deploy_page_upload", changed)).code(), "page_upload_commit_conflict", "retry options are bound to the commit")
	stagedID := strAt(staged, "version", "id")
	eq(t, h.scalar("SELECT html FROM page_versions WHERE id = $1", stagedID), largeHTML, "UTF-8 dashboard bytes survive chunking exactly")
	eq(t, h.count("SELECT count(*)::integer FROM audit_log WHERE action = 'deploy' AND version_id = $1", stagedID), 1, "commit retry does not duplicate the version audit")
	eq(t, h.count("SELECT count(*)::integer FROM page_content_upload_chunks WHERE upload_id = $1", uploadID), 0, "committed chunk bytes are released")
	eq(t, h.fails(h.call("deploy_page_upload", o("upload_id", uploadID), as(other.Token))).code(), "page_upload_not_found")

	// A host that stages writes for human approval freezes expected_version
	// when the card is staged; by the time a human approves, another write
	// has moved the pointer. The bytes already uploaded are still there.
	race := []byte(strings.Replace(largeHTML, "dashboardReady", "raceReady", 1))
	raceUpload := h.data(h.call("create_upload_ticket", o("slug", "mcp-large-upload", "total_bytes", num(float64(len(race))), "content_sha256", shaHex(race))))
	raceSent := h.request("PUT", "/upload/"+strAt(raceUpload, "upload_id"), reqOpts{rawBody: ptr(string(race)), token: strAt(raceUpload, "ticket"), contentType: ptr("application/octet-stream")})
	eq(t, raceSent.status, 200, raceSent.body)
	eq(t, raceSent.get("complete"), true)
	overtaking := h.data(h.call("patch_page", o("slug", "mcp-large-upload",
		"edits", arr(o("find", "<title>Large MCP</title>", "replace", "<title>Overtaken</title>")), "note", "the write that wins the race")))
	lost := h.fails(h.call("deploy_page_upload", o("upload_id", strAt(raceUpload, "upload_id"), "render_mode", "raw", "expected_version", stagedID)))
	eq(t, lost.code(), "stale_version")
	eq(t, dig(lost.data, "details", "expected_version"), stagedID)
	eq(t, dig(lost.data, "details", "retry_with_expected_version"), strAt(overtaking, "version", "id"))
	eq(t, dig(lost.data, "details", "upload_id"), strAt(raceUpload, "upload_id"))
	eq(t, dig(lost.data, "details", "upload_still_staged"), true)
	mustMatch(t, `still deployable`, strAt(lost.data, "error"))
	mustMatch(t, `do not re-upload`, strAt(lost.data, "error"))
	won := h.data(h.call("deploy_page_upload", o("upload_id", strAt(raceUpload, "upload_id"), "render_mode", "raw", "expected_version", strAt(overtaking, "version", "id"))))
	eq(t, dig(won, "version", "content_sha256"), shaHex(race))

	// Upload commit and the version/audit transaction are one unit.
	atomic := []byte("<!doctype html><html><body>atomic staged content</body></html>")
	atomicUpload := h.data(h.call("start_page_upload", o("slug", "mcp-upload-atomic", "total_bytes", num(float64(len(atomic))), "content_sha256", shaHex(atomic))))
	h.data(h.call("append_page_upload", o("upload_id", strAt(atomicUpload, "upload_id"), "sequence", num(0), "chunk_base64", base64.StdEncoding.EncodeToString(atomic))))
	actor := versions.Actor{Actor: "mcp-agent", ActorType: "agent", TokenID: h.tokenID, IP: "127.0.0.1"}
	simulated := errors.New("simulated failure after version creation")
	_, err := uploads.Deploy(context.Background(), h.d, strAt(atomicUpload, "upload_id"), actor, strings.Repeat("a", 64),
		func(ctx context.Context, tx pgx.Tx, s uploads.Staged) (any, error) {
			in := versions.CreateAndDeployInput{DeployInput: versions.DeployInput{Slug: s.Slug, HTML: s.HTML, RenderMode: "raw", Source: versions.SourceMCP, Publish: true}, Title: "Must Roll Back"}
			if _, err := versions.CreateAndDeployWithTx(ctx, tx, in, actor, versions.CreateAndDeployOptions{}); err != nil {
				return nil, err
			}
			return nil, simulated
		}, uploads.DeployOptions{})
	if !errors.Is(err, simulated) {
		t.Fatalf("deploy error %v", err)
	}
	eq(t, h.count("SELECT count(*)::integer FROM pages WHERE slug = 'mcp-upload-atomic'"), 0, "failed commit cannot strand a page/version")
	row := h.query("SELECT bytes_received, commit_result FROM page_content_uploads WHERE id = $1", strAt(atomicUpload, "upload_id"))[0]
	eq(t, row["bytes_received"], len(atomic))
	eq(t, row["commit_result"], nil, "verified upload remains retryable")
	atomicRetry := h.data(h.call("deploy_page_upload", o("upload_id", strAt(atomicUpload, "upload_id"), "title", "Atomic Upload", "render_mode", "raw")))
	eq(t, atomicRetry["created"], true)
	eq(t, atomicRetry["version_is_live"], true)

	eq(t, h.fails(h.call("deploy_page", o("slug", "mcp-placeholder", "html", "$(cat dashboard.html)"))).code(), "html_placeholder")
	eq(t, h.count("SELECT count(*)::integer FROM pages WHERE slug = 'mcp-placeholder'"), 0, "literal file placeholders never create pages")

	intended := []byte("good")
	badHashUpload := h.data(h.call("start_page_upload", o("slug", "mcp-upload-bad-hash", "total_bytes", num(float64(len(intended))), "content_sha256", shaHex(intended))))
	badID := strAt(badHashUpload, "upload_id")
	eq(t, h.fails(h.call("append_page_upload", o("upload_id", badID, "sequence", num(0), "chunk_base64", base64.StdEncoding.EncodeToString([]byte("evil"))))).code(), "upload_hash_mismatch")
	badRow := h.query("SELECT bytes_received, next_sequence FROM page_content_uploads WHERE id = $1", badID)[0]
	eq(t, badRow["bytes_received"], 0, "bad final hash rolls back its chunk")
	eq(t, badRow["next_sequence"], 0)
	cancelled := h.data(h.call("cancel_page_upload", o("upload_id", badID)))
	eq(t, cancelled["cancelled"], true)
	eq(t, h.data(h.call("cancel_page_upload", o("upload_id", badID)))["cancelled"], false, "cancellation retries are harmless")
	mustMatch(t, `re-emits the whole document|create_upload_ticket`, strAt(cancelled, "next_step"))

	// A caller stuck in the start/cancel loop is told so.
	churn := []byte(pageHTML(60))
	churnStart := func() map[string]any {
		return h.data(h.call("start_page_upload", o("slug", "mcp-upload-churn", "total_bytes", num(float64(len(churn))), "content_sha256", shaHex(churn))))
	}
	churn1 := churnStart()
	mustNotMatch(t, `does not converge`, strAt(churn1, "next_step"), "the first attempt is not scolded")
	h.data(h.call("cancel_page_upload", o("upload_id", strAt(churn1, "upload_id"))))
	churn2 := churnStart()
	h.data(h.call("cancel_page_upload", o("upload_id", strAt(churn2, "upload_id"))))
	churn3 := churnStart()
	mustMatch(t, `does not converge`, strAt(churn3, "next_step"))
	mustMatch(t, `create_upload_ticket`, strAt(churn3, "next_step"))
	mustMatch(t, `started 3 chunked uploads for this target and cancelled 2`, strAt(churn3, "next_step"))
	h.data(h.call("cancel_page_upload", o("upload_id", strAt(churn3, "upload_id"))))
	churnTicket := h.data(h.call("create_upload_ticket", o("slug", "mcp-upload-churn", "total_bytes", num(float64(len(churn))), "content_sha256", shaHex(churn))))
	mustNotMatch(t, `does not converge`, strAt(churnTicket, "next_step"), "taking the advice is not itself an attempt")
	h.data(h.call("cancel_page_upload", o("upload_id", strAt(churnTicket, "upload_id"))))
}
