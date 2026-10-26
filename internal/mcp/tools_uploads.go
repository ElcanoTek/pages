// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Staged uploads: start/append/cancel/ticket, and the three consumers that
// turn verified bytes into a page version, a template revision or a data
// version inside the upload's own transaction.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/uploads"
	"github.com/ElcanoTek/pages/internal/versions"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func startInput(a *jsonv.Object) uploads.StartInput {
	total, _ := argInt(a, "total_bytes")
	sha, _ := argStr(a, "content_sha256")
	in := uploads.StartInput{
		Slug:          argStrPtr(a, "slug"),
		Template:      argStrPtr(a, "template"),
		Kind:          argStrPtr(a, "kind"),
		TotalBytes:    total,
		ContentSHA256: sha,
	}
	if v, ok := a.Get("data_size"); ok {
		in.DataSize = versions.Arg(v, true)
	}
	return in
}

func startPageUpload(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	return uploads.Start(ctx, c.d, startInput(a), c.actor)
}

func createUploadTicket(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	created, err := uploads.CreateTicket(ctx, c.d, startInput(a), c.actor)
	if err != nil {
		return nil, err
	}
	uploadURL := uploads.UploadURL(created.UploadID)
	consumer := uploads.ConsumerFor(created.TargetKind)
	curl := fmt.Sprintf(`curl -fsS -X PUT --data-binary @<file> -H "Authorization: Bearer %s" %s`, created.Ticket, uploadURL)
	return spread(created, jsonv.ObjectOf(
		"upload_url", uploadURL,
		"curl", curl,
		"next_step", "Send the file with: "+curl+
			fmt.Sprintf(" — then call %s with upload_id %s. Do not base64 the file into a tool argument.", consumer, created.UploadID),
	))
}

func appendPageUpload(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	id, _ := argStr(a, "upload_id")
	seq, _ := argInt(a, "sequence")
	chunk, _ := argStr(a, "chunk_base64")
	return uploads.Append(ctx, c.d, uploads.AppendInput{UploadID: id, Sequence: seq, ChunkBase64: chunk}, c.actor)
}

func cancelPageUpload(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	id, _ := argStr(a, "upload_id")
	return uploads.Cancel(ctx, c.d, id, c.actor)
}

// ── commit keys ─────────────────────────────────────────────────────────────
//
// The commit key is what makes an exact retry idempotent and a DIFFERENT
// retry a conflict rather than a silent second publish. Every field that
// changes what gets written belongs in it. The JSON is the JavaScript's, so
// a retry spanning the Go migration still matches its receipt.

func orNull(a *jsonv.Object, key string) any {
	if v, ok := a.Get(key); ok && truthy(v) {
		return v
	}
	return nil
}

func pageUploadCommitKey(a *jsonv.Object) string {
	renderMode, ok := argStr(a, "render_mode")
	if !ok || renderMode == "" {
		renderMode = "themed"
	}
	title, _ := argStr(a, "title")
	o := jsonv.ObjectOf(
		"render_mode", renderMode,
		"note", orNull(a, "note"),
		"publish", argBoolOr(a, "publish", true),
		"expected_version", orNull(a, "expected_version"),
		"title", title,
		"require_approval", argBoolOr(a, "require_approval", false),
		"client_id", orNull(a, "client_id"),
	)
	if v := orNull(a, "workspace_id"); v != nil {
		o.Set("workspace_id", v)
	}
	return sha256Hex(jsonv.Stringify(o))
}

func templateUploadCommitKey(a *jsonv.Object) string {
	return sha256Hex(jsonv.Stringify(jsonv.ObjectOf(
		"title", orNull(a, "title"),
		"description", orNull(a, "description"),
		"note", orNull(a, "note"),
	)))
}

func dataUploadCommitKey(a *jsonv.Object) string {
	o := jsonv.NewObject()
	o.Set("slug", a.Value("slug"))
	if v, ok := a.Get("source_as_of"); ok {
		o.Set("source_as_of", v)
	}
	o.Set("expected_version", jsToString(a.Value("expected_version"), a.Has("expected_version")))
	o.Set("publish", argBoolOr(a, "publish", true))
	o.Set("note", orNull(a, "note"))
	o.Set("expect", a.Value("expect"))
	return sha256Hex(jsonv.Stringify(o))
}

// ── replays ─────────────────────────────────────────────────────────────────

func objectAt(v any, keys ...string) *jsonv.Object {
	o, _ := v.(*jsonv.Object)
	for _, k := range keys {
		if o == nil {
			return nil
		}
		o, _ = o.Value(k).(*jsonv.Object)
	}
	return o
}

func idFrom(v any) jsx.ID {
	id, err := jsx.ParseID(v)
	if err != nil {
		return 0
	}
	return id
}

// replayPageUpload: a committed upload keeps its original receipt. Only
// serving guidance is refreshed on replay, using the original entity ID so a
// reused slug can never turn an old receipt into a claim about its
// replacement.
func replayPageUpload(ctx context.Context, tx pgx.Tx, receiptValue any) (any, error) {
	receipt, _ := receiptValue.(*jsonv.Object)
	if receipt == nil {
		return receiptValue, nil
	}
	version := objectAt(receipt, "version")
	pageID := idFrom(version.Value("page_id"))
	versionID := idFrom(version.Value("id"))
	var disabled bool
	var deletedAt *time.Time
	var published jsx.ID
	err := tx.QueryRow(ctx, `SELECT disabled, deleted_at, published_version_id FROM pages WHERE id = $1`, pageID).
		Scan(&disabled, &deletedAt, &published)
	found := true
	if db.IsNoRows(err) {
		found = false
	} else if err != nil {
		return nil, err
	}
	deleted := !found || deletedAt != nil
	var live jsx.ID
	if !deleted {
		live = published
	}
	pageIsLive := !deleted && !disabled && live != 0
	versionIsLive := pageIsLive && sameID(versionID, live)
	prefix := fmt.Sprintf("Upload was already committed as version %s. This retry did not republish or change it. ", jsonv.ToString(version.Value("id")))
	var next string
	switch {
	case deleted:
		next = "The original page was deleted and is not serving. Do not share its live URL; a reused slug may belong to another page."
	case disabled:
		next = "The page is disabled by an admin and is not serving. A human admin must re-enable it before its live URL can be shared."
	case versionIsLive:
		next = "This version is live now — share urls.live with the user or client."
	default:
		if pageIsLive {
			next = fmt.Sprintf("Version %s is currently live; this upload's version is not. ", live)
		} else {
			next = "No version is currently live. "
		}
		if receipt.Has("envelope") {
			next += "Call get_page_data to read the current state before deciding on any new data update or publication."
		} else {
			next += "Call get_page and list_versions to read the current state before deciding on any new publication."
		}
	}
	out := receipt.Clone()
	out.Set("replayed", true)
	if receipt.Has("live") {
		out.Set("live", versionIsLive)
	}
	out.Set("version_is_live", versionIsLive)
	out.Set("page_is_live", pageIsLive)
	out.Set("live_version_id", idOrNull(live))
	out.Set("next_step", dataUpdateNextStep(prefix+next, warningsFromJSON(receipt.Value("data_warnings"))))
	return out, nil
}

func templateNextStep(result *jsonv.Object) string {
	name := jsonv.ToString(objectAt(result, "template").Value("name"))
	revision := jsonv.ToString(objectAt(result, "revision").Value("revision"))
	if pf := objectAt(result, "preflight"); pf != nil && pf.Value("ok") == false {
		return fmt.Sprintf("Registered %s revision %s, but preflight found errors in the template itself — ", name, revision) +
			"every page built from it inherits them. Fix the template and register again before creating pages."
	}
	if truthy(result.Value("deduped")) {
		return fmt.Sprintf("These exact bytes are already %s revision %s; no new revision was created.", name, revision)
	}
	return fmt.Sprintf("%s is at revision %s. Call create_page_from_template with a complete config to build a page; ", name, revision) +
		"call get_template to re-read the config and data schemas without pulling the HTML."
}

func replayTemplateUpload(ctx context.Context, tx pgx.Tx, receiptValue any) (any, error) {
	receipt, _ := receiptValue.(*jsonv.Object)
	if receipt == nil {
		return receiptValue, nil
	}
	templateID := idFrom(objectAt(receipt, "template").Value("id"))
	var deletedAt *time.Time
	var current *int
	err := tx.QueryRow(ctx,
		`SELECT t.deleted_at, v.revision
		   FROM page_templates t LEFT JOIN page_template_versions v ON v.id = t.current_version_id
		  WHERE t.id = $1`, templateID).Scan(&deletedAt, &current)
	found := true
	if db.IsNoRows(err) {
		found = false
	} else if err != nil {
		return nil, err
	}
	name := jsonv.ToString(objectAt(receipt, "template").Value("name"))
	revisionValue := objectAt(receipt, "revision").Value("revision")
	prefix := fmt.Sprintf("Upload was already registered as %s revision %s. This retry did not register or change it. ", name, jsonv.ToString(revisionValue))
	var next string
	switch {
	case !found || deletedAt != nil:
		next = "The original template was retired. A reused name may belong to another template; call list_templates before selecting a design."
	case current == nil || float64(*current) != revisionValue:
		currentText := "null"
		if current != nil {
			currentText = fmt.Sprint(*current)
		}
		next = fmt.Sprintf("Its current revision is %s. Call get_template to read the current design before creating or rerendering a page.", currentText)
	default:
		next = templateNextStep(receipt)
	}
	out := receipt.Clone()
	out.Set("replayed", true)
	out.Set("next_step", prefix+next)
	return out, nil
}

// ── consumers ───────────────────────────────────────────────────────────────

func lockedPageState(ctx context.Context, tx pgx.Tx, slug string) (pageState, error) {
	var p pageState
	err := tx.QueryRow(ctx, `SELECT disabled, published_version_id FROM pages WHERE slug = $1 AND deleted_at IS NULL`, slug).
		Scan(&p.Disabled, &p.PublishedVersionID)
	return p, err
}

// deployPageUpload: a losing race here is uniquely cheap to recover from
// and uniquely easy to misread as expensive. uploads.Deploy writes the
// receipt and drops the chunks only after the callback returns, so an error
// rolls the transaction back and leaves the staged bytes intact and
// re-deployable — but the agent sees only "stale_version" and re-does the
// whole job. Say so in the error, or the retry costs a full turn instead of
// one call.
func deployPageUpload(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	id, _ := argStr(a, "upload_id")
	out, err := deployPageUploadLocked(ctx, c, a)
	if err == nil {
		return out, nil
	}
	if e, ok := apierror.As(err); ok && e.Code == "stale_version" && e.Details != nil {
		details, derr := spread(e.Details, jsonv.ObjectOf("upload_id", id, "upload_still_staged", true))
		if derr != nil {
			return nil, err
		}
		return nil, &apierror.Error{
			Status: e.Status,
			Message: e.Message + fmt.Sprintf(" Upload %s is untouched and still deployable — retry deploy_page_upload with", id) +
				" the same upload_id; do not re-upload the file.",
			Code:    e.Code,
			Details: details,
		}
	}
	return nil, err
}

func deployPageUploadLocked(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	id, _ := argStr(a, "upload_id")
	return uploads.Deploy(ctx, c.d, id, c.actor, pageUploadCommitKey(a), func(ctx context.Context, tx pgx.Tx, staged uploads.Staged) (any, error) {
		if _, err := AssertMcpHTML(staged.HTML); err != nil {
			return nil, err
		}
		in := createdOnlyInput(a, deployInput(a, staged.Slug, staged.HTML))
		result, err := versions.CreateAndDeployWithTx(ctx, tx, in, c.actor, versions.CreateAndDeployOptions{})
		if err != nil {
			return nil, err
		}
		page, err := lockedPageState(ctx, tx, staged.Slug)
		if err != nil {
			return nil, err
		}
		mode, _ := argStr(a, "render_mode")
		out, err := spread(
			jsonv.ObjectOf("upload_id", staged.UploadID, "created", result.Created),
			deployGuidance(&result.DeployResult, page, staged.Slug),
		)
		if err != nil {
			return nil, err
		}
		out.Set("preflight", analyze(staged.HTML, mode))
		return out, nil
	}, uploads.DeployOptions{ReplayCommitted: replayPageUpload})
}

// registerTemplateUpload is the same verified-bytes seam deploy_page_upload
// uses, pointed at the template tables instead of a page. Verification,
// chunk accounting, ticket single-use, and commit idempotency are untouched.
func registerTemplateUpload(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	id, _ := argStr(a, "upload_id")
	return uploads.Deploy(ctx, c.d, id, c.actor, templateUploadCommitKey(a), func(ctx context.Context, tx pgx.Tx, staged uploads.Staged) (any, error) {
		if _, err := AssertMcpHTML(staged.HTML); err != nil {
			return nil, err
		}
		result, err := templates.RegisterWithTx(ctx, tx, templates.RegisterInput{
			Name:        staged.Template,
			HTML:        staged.HTML,
			Title:       argStrPtr(a, "title"),
			Description: argStrPtr(a, "description"),
			Note:        argStrPtr(a, "note"),
			Source:      templates.SourceMCP,
		}, c.actor)
		if err != nil {
			return nil, err
		}
		out, err := spread(jsonv.ObjectOf("upload_id", staged.UploadID), result)
		if err != nil {
			return nil, err
		}
		out.Set("next_step", templateNextStep(out))
		return out, nil
	}, uploads.DeployOptions{ExpectKind: uploads.KindTemplate, ReplayCommitted: replayTemplateUpload})
}

type slugDetails struct {
	Slug string `json:"slug"`
}

// updatePageDataUpload is the staged counterpart of update_page_data. The
// payload arrived through a ticket, so it never entered the model's context
// and there is no inline transport ceiling to hit; data/envelope limits still
// apply. From the parse onward it is the same write, inside the upload's own
// transaction so a spent upload can never be left with no version behind it.
func updatePageDataUpload(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	id, _ := argStr(a, "upload_id")
	argSlug, _ := argStr(a, "slug")
	return uploads.Deploy(ctx, c.d, id, c.actor, dataUploadCommitKey(a), func(ctx context.Context, tx pgx.Tx, staged uploads.Staged) (any, error) {
		normalized, err := versions.NormalizeSlug(argSlug)
		if err != nil {
			return nil, err
		}
		if staged.Slug != normalized {
			return nil, apierror.Conflict(
				fmt.Sprintf("this upload was staged for %s, not %s; the target is fixed when the upload starts", staged.Slug, argSlug),
				"page_upload_target_mismatch", slugDetails{Slug: staged.Slug})
		}
		// The upload already proved the bytes are complete, hash-matched and
		// valid UTF-8. What it cannot know is that they are a JSON OBJECT,
		// which is the contract update_page_data has always enforced.
		data, err := jsonv.Parse([]byte(staged.HTML))
		if err != nil {
			return nil, apierror.BadRequest(
				fmt.Sprintf("staged upload is not valid JSON: %s. Stage the exact file your generator wrote; do not hand-edit it.", err.Error()),
				"data_upload_not_json", nil)
		}
		if _, ok := data.(*jsonv.Object); !ok {
			return nil, apierror.BadRequest("staged managed data must be a JSON object", "data_invalid", nil)
		}
		result, err := versions.UpdatePageDataWithTx(ctx, tx, dataUpdateInput(a, staged.Slug, data), c.actor)
		if err != nil {
			return nil, err
		}
		page, err := lockedPageState(ctx, tx, staged.Slug)
		if err != nil {
			return nil, err
		}
		shaped, err := dataUpdateResult(result, page, staged.Slug, publishArg(a))
		if err != nil {
			return nil, err
		}
		return spread(jsonv.ObjectOf("upload_id", staged.UploadID), shaped)
	}, uploads.DeployOptions{ExpectKind: uploads.KindData, ReplayCommitted: replayPageUpload})
}
