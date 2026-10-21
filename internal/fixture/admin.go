// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/render"
)

var (
	renameWorkspaceRE = regexp.MustCompile(`^/workspaces/([0-9]+)/rename$`)
	deleteWorkspaceRE = regexp.MustCompile(`^/workspaces/([0-9]+)/delete$`)
	templateOneRE     = regexp.MustCompile(`^/templates/[^/]+$`)
	templatePreviewRE = regexp.MustCompile(`^/templates/[^/]+/preview-token$`)
	versionReadRE     = regexp.MustCompile(`^/versions/([0-9]+)$`)
	versionActionRE   = regexp.MustCompile(`^/versions/([0-9]+)/(approve|reject)$`)
)

func isDeck(html any) bool {
	s, ok := html.(string)
	return ok && render.IsBentoDeck(s)
}

func decodeURIComponent(s string) (string, error) { return httpx.DecodeURIComponent(s) }

func jsLen(s string) int { return jsstr.UTF16Len(s) }

func errorBody(msg string) obj { return o("error", msg) }

// adminAPI is app.use("/api/v1/admin", …): pages, workspaces, the template
// library and compose. It returns true to fall through (next()).
func (a *App) adminAPI(w http.ResponseWriter, r *req) bool {
	route := r.rest
	method := r.Method
	body := r.bodyOr()

	if method == http.MethodGet && route == "/pages" {
		a.mu.Lock()
		if a.st.failNextPages {
			a.st.failNextPages = false
			a.mu.Unlock()
			sendJSON(w, r.Request, http.StatusServiceUnavailable, errorBody("fixture page list unavailable"))
			return false
		}
		delay := a.st.delayNextPages
		a.st.delayNextPages = 0
		a.mu.Unlock()
		if truthy(delay) {
			sleep(delay)
		}
		a.mu.Lock()
		payload := marshal(o("pages", a.st.pageRows(), "workspaces", a.st.workspaceRows()))
		a.mu.Unlock()
		httpx.Send(w, r.Request, http.StatusOK, "application/json; charset=utf-8", payload)
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.st
	send := func(status int, v any) bool {
		sendJSON(w, r.Request, status, v)
		return false
	}

	if method == http.MethodGet && route == "/workspaces" {
		return send(http.StatusOK, o("workspaces", st.workspaceRows()))
	}

	// ── template library ────────────────────────────────────────────────────
	if method == http.MethodGet && route == "/templates" {
		return send(http.StatusOK, o("templates", st.templates))
	}
	if method == http.MethodGet && strings.HasPrefix(route, "/templates/") {
		name, err := decodeURIComponent(route[len("/templates/"):])
		if err != nil {
			sendError(w, r.Request, http.StatusInternalServerError, "URIError: URI malformed")
			return false
		}
		found := st.templates.find(func(t obj) bool { return get(t, "name") == any(name) })
		if found == nil {
			return send(http.StatusNotFound, o("error", "template not found: "+name, "code", "template_not_found"))
		}
		return send(http.StatusOK, templateDetail(found))
	}
	if method == http.MethodPost && route == "/templates/validate" {
		return send(http.StatusOK, templateReport(body))
	}
	if method == http.MethodPost && route == "/templates" {
		report := templateReport(body)
		if !truthy(get(report, "contract_ok")) {
			ce := get(report, "contract_error")
			return send(422, o("error", get(ce, "message"), "code", get(ce, "code")))
		}
		name := get(report, "name")
		if existing := st.templates.find(func(t obj) bool { return get(t, "name") == name }); existing != nil {
			set(existing, "current_revision", num(get(existing, "current_revision"))+1)
			return send(http.StatusOK, o("created", false, "deduped", false, "template", existing,
				"revision", o("revision", get(existing, "current_revision"))))
		}
		id := str(float64(st.templates.len() + 1))
		created := o(
			"id", id,
			"name", name,
			"title", or(get(body, "title"), ""),
			"description", or(get(body, "description"), ""),
			"current_revision", 1,
			"current_version_id", id,
			"created_at", NOW,
			"updated_at", NOW,
			"config_schema_sha256", strings.Repeat("c", 64),
			"data_schema_sha256", strings.Repeat("d", 64),
			"page_count", 0,
		)
		st.templates.push(created)
		return send(http.StatusCreated, o("created", true, "deduped", false, "template", created, "revision", o("revision", 1)))
	}
	if method == http.MethodDelete && templateOneRE.MatchString(route) {
		name, err := decodeURIComponent(strings.Split(route, "/")[2])
		if err != nil {
			sendError(w, r.Request, http.StatusInternalServerError, "URIError: URI malformed")
			return false
		}
		template := st.templates.find(func(t obj) bool { return get(t, "name") == any(name) })
		if template == nil {
			return send(http.StatusNotFound, errorBody("template not found"))
		}
		built := get(templateDetail(template), "pages").(*list).len()
		force, single := httpx.QueryParam(r.Request, "force").String()
		if built > 0 && (!single || force != "true") {
			return send(http.StatusConflict, o("error", str(float64(built))+" pages were built from "+name+"; pass force", "code", "template_has_pages"))
		}
		st.templates = st.templates.filter(func(t obj) bool { return get(t, "name") != any(name) })
		return send(http.StatusOK, o("template", name, "deleted", true, "pages_built", built))
	}
	if method == http.MethodPost && templatePreviewRE.MatchString(route) {
		name, err := decodeURIComponent(strings.Split(route, "/")[2])
		if err != nil {
			sendError(w, r.Request, http.StatusInternalServerError, "URIError: URI malformed")
			return false
		}
		template := st.templates.find(func(t obj) bool { return get(t, "name") == any(name) })
		if template == nil {
			return send(http.StatusNotFound, errorBody("template not found"))
		}
		// Echo back the revision that was asked for, and that revision's own
		// example-data state — the shell labels the frame from both, so a
		// fixture that always answered "revision 1, has data" would hide a
		// mislabelled frame.
		asked := get(body, "revision")
		var revision any
		if _, missing := asked.(undefined); missing || asked == nil {
			revision = get(template, "current_revision")
		} else {
			revision = num(asked)
		}
		known := get(templateDetail(template), "revisions").(*list).find(func(e obj) bool {
			rv, ok := revision.(float64)
			return ok && get(e, "revision") == any(rv)
		})
		mode := "themed"
		if get(body, "render_mode") == any("raw") {
			mode = "raw"
		}
		return send(http.StatusOK, o(
			"template", name,
			"revision", revision,
			"render_mode", mode,
			"content_sha256", strings.Repeat("e", 64),
			"has_sample_data", known != nil && truthy(get(known, "has_sample_data")),
			"url", a.origin+"/preview/tpl-"+httpx.EncodeURIComponent(name)+"-r"+str(revision),
			"expires_in_seconds", 300,
		))
	}

	if method == http.MethodPost && route == "/pages" {
		slug := strings.ToLower(jsstr.Trim(str(or(get(body, "slug"), ""))))
		if slug == "" {
			return send(http.StatusBadRequest, errorBody("slug is required"))
		}
		if st.pages.find(func(p obj) bool { return get(p, "slug") == any(slug) }) != nil {
			return send(http.StatusConflict, errorBody("slug already exists"))
		}
		workspace := st.workspaces.find(func(ws obj) bool { return sameString(get(ws, "id"), get(body, "workspace_id")) })
		var workspaceID, workspaceName any
		if workspace != nil {
			workspaceID, workspaceName = or(get(workspace, "id"), nil), or(get(workspace, "name"), nil)
		}
		page := o(
			"id", st.nextPage, "slug", slug, "title", or(get(body, "title"), slug), "client_id", nil,
			"workspace_id", workspaceID, "workspace_name", workspaceName,
			"theme_id", nil, "theme_name", "flag", "require_approval", false, "disabled", false,
			"published_version_id", nil, "has_password", false, "is_live", false,
			"created_at", NOW, "updated_at", NOW,
		)
		st.nextPage++
		st.pages.unshift(page)
		st.details[slug] = newList()
		st.record(r, page)
		return send(http.StatusCreated, o("page", page))
	}

	if method == http.MethodPost && route == "/workspaces" {
		workspace := o("id", st.nextWorkspace, "name", jsstr.Trim(str(or(get(body, "name"), ""))), "created_at", NOW, "updated_at", NOW)
		st.nextWorkspace++
		st.workspaces.push(workspace)
		st.events.push(o("method", method, "path", route, "body", body))
		return send(http.StatusCreated, o("workspace", workspace))
	}
	if m := renameWorkspaceRE.FindStringSubmatch(route); method == http.MethodPost && m != nil {
		workspace := st.workspaces.find(func(ws obj) bool { return str(get(ws, "id")) == m[1] })
		if workspace == nil {
			return send(http.StatusNotFound, errorBody("workspace not found"))
		}
		set(workspace, "name", jsstr.Trim(str(or(get(body, "name"), ""))))
		for _, p := range st.pages.items {
			if sameString(get(p, "workspace_id"), get(workspace, "id")) {
				set(asObj(p), "workspace_name", get(workspace, "name"))
			}
		}
		st.events.push(o("method", method, "path", route, "body", body))
		return send(http.StatusOK, o("workspace", workspace))
	}
	if m := deleteWorkspaceRE.FindStringSubmatch(route); method == http.MethodPost && m != nil {
		index := st.workspaces.findIndex(func(ws obj) bool { return str(get(ws, "id")) == m[1] })
		if index < 0 {
			return send(http.StatusNotFound, errorBody("workspace not found"))
		}
		workspace := asObj(st.workspaces.splice(index))
		for _, p := range st.pages.items {
			if sameString(get(p, "workspace_id"), get(workspace, "id")) {
				set(asObj(p), "workspace_id", nil)
				set(asObj(p), "workspace_name", nil)
			}
		}
		st.events.push(o("method", method, "path", route, "body", body))
		return send(http.StatusOK, o("workspace", workspace))
	}

	if method == http.MethodPost && route == "/compose" {
		st.events.push(o("method", method, "path", route, "body", body))
		return send(http.StatusOK, o("jobId", "fixture-job"))
	}
	if method == http.MethodGet && route == "/compose/fixture-job" {
		return send(http.StatusOK, o("status", "done", "slug", "cutlass-fixture", "title", "Cutlass fixture", "log", "audit complete\npublished"))
	}

	if !strings.HasPrefix(route, "/pages/") {
		return true
	}
	page, suffix := st.findPageFromPath(route[len("/pages/"):])
	if page == nil {
		return send(http.StatusNotFound, errorBody("page not found"))
	}
	versions := st.versionsOf(str(get(page, "slug")))
	if method == http.MethodGet && suffix == "" {
		return send(http.StatusOK, st.detailFor(page))
	}
	// One version's full row, html included — what the source editor reads to
	// open a version that is not the live one.
	if m := versionReadRE.FindStringSubmatch(suffix); method == http.MethodGet && m != nil {
		version := versions.find(func(v obj) bool { return str(get(v, "id")) == m[1] })
		if version == nil {
			return send(http.StatusNotFound, errorBody("version not found"))
		}
		return send(http.StatusOK, o("version", version))
	}
	if method != http.MethodPost {
		return send(http.StatusMethodNotAllowed, errorBody("method not allowed"))
	}
	st.record(r, page)

	switch suffix {
	case "/preview-token":
		return send(http.StatusOK, o("url", a.origin+"/preview/"+str(get(body, "version_id"))))
	case "/edit-token":
		return send(http.StatusOK, o("url", a.origin+"/bento/edit"))
	}
	if m := versionActionRE.FindStringSubmatch(suffix); m != nil {
		version := versions.find(func(v obj) bool { return str(get(v, "id")) == m[1] })
		if version == nil {
			return send(http.StatusNotFound, errorBody("version not found"))
		}
		if m[2] == "approve" {
			set(version, "status", "approved")
			set(version, "reviewed_by", "qa-admin@elcanotek.com")
			set(version, "reviewed_at", NOW)
			set(page, "published_version_id", get(version, "id"))
			set(page, "is_live", !truthy(get(page, "disabled")))
			if !truthy(get(version, "html")) {
				set(version, "html", replaceFirst(versionHTML, "Published", "Approved "+str(get(version, "id"))))
			}
		} else {
			set(version, "status", "rejected")
			set(version, "reviewed_by", "qa-admin@elcanotek.com")
			set(version, "reviewed_at", NOW)
		}
		return send(http.StatusOK, o("version", version))
	}
	switch suffix {
	case "/publish", "/rollback":
		version := versions.find(func(v obj) bool { return sameString(get(v, "id"), get(body, "version_id")) })
		if version == nil {
			return send(http.StatusNotFound, errorBody("version not found"))
		}
		set(version, "status", "approved")
		if !truthy(get(version, "html")) {
			set(version, "html", replaceFirst(versionHTML, "Published", "Version "+str(get(version, "id"))))
		}
		set(page, "published_version_id", get(version, "id"))
		set(page, "is_live", !truthy(get(page, "disabled")))
		return send(http.StatusOK, o("version", version))
	case "/workspace":
		workspace := st.workspaces.find(func(ws obj) bool { return sameString(get(ws, "id"), get(body, "workspace_id")) })
		var id, name any
		if workspace != nil {
			id, name = or(get(workspace, "id"), nil), or(get(workspace, "name"), nil)
		}
		set(page, "workspace_id", id)
		set(page, "workspace_name", name)
		return send(http.StatusOK, o("page", page))
	case "/title":
		set(page, "title", str(or(get(body, "title"), "")))
		return send(http.StatusOK, page)
	case "/theme":
		var themeID any
		if get(body, "theme") == any("client-brand") {
			themeID = float64(2)
		}
		set(page, "theme_id", themeID)
		set(page, "theme_name", get(body, "theme"))
		return send(http.StatusOK, page)
	case "/approval":
		set(page, "require_approval", truthy(get(body, "require_approval")))
		return send(http.StatusOK, page)
	case "/disable":
		set(page, "disabled", true)
		set(page, "is_live", false)
		return send(http.StatusOK, page)
	case "/enable":
		set(page, "disabled", false)
		set(page, "is_live", truthy(get(page, "published_version_id")))
		return send(http.StatusOK, page)
	case "/password":
		set(page, "has_password", truthy(get(body, "password")))
		return send(http.StatusOK, page)
	case "/reveal-password":
		hasPassword := truthy(get(page, "has_password"))
		var password any
		if hasPassword {
			password = "fixture-client-pass"
		}
		return send(http.StatusOK, o("slug", get(page, "slug"), "has_password", hasPassword, "available", hasPassword, "password", password))
	case "/delete":
		st.pages = st.pages.filter(func(p obj) bool { return p != page })
		return send(http.StatusOK, o("deleted", true))
	case "/deploy-source":
		status := "draft"
		if truthy(get(page, "require_approval")) {
			status = "pending"
		}
		mode := "themed"
		if get(body, "render_mode") == any("raw") {
			mode = "raw"
		}
		version := o(
			"id", st.nextVersion, "page_id", get(page, "id"),
			"status", status,
			"render_mode", mode,
			"author", "qa-admin@elcanotek.com", "source", "admin", "note", or(get(body, "note"), nil),
			"reviewed_by", nil, "reviewed_at", nil, "created_at", NOW, "html", get(body, "html"),
		)
		st.nextVersion++
		versions.unshift(version)
		return send(http.StatusOK, o("version", version, "deduped", false, "published", false, "gated", get(page, "require_approval")))
	}
	return send(http.StatusNotFound, errorBody("fixture action not found"))
}
