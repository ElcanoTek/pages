// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package compose is DEV/TEST ONLY: the in-admin "compose a page with Cutlass"
// panel.
//
// This deliberately crosses the clean separation (Pages hosts; agents author —
// the real production create flow lives outside Pages). It exists for local
// testing of the text→agent→page loop and is GATED: only mounted when
// PAGES_COMPOSE=1, and admin-cookie + CSRF authenticated. Never enable it in
// production.
//
// Two pluggable drivers (COMPOSE_DRIVER), with the same in-memory job and log
// shape so the browser panel (welcome.js) is identical either way:
//
//   - spawn (default, dev) — run the local `cutlass` CLI (argument vector, NO
//     shell, so a prompt cannot inject a command), cwd=CUTLASS_DIR so it reads
//     its own .env (OpenRouter key + PAGES_MCP_*). It connects back to THIS
//     server's /mcp and deploys+publishes. Needs CUTLASS_BIN + CUTLASS_DIR.
//
//   - moc (prod-style) — POST {MOC_URL}/tasks with X-API-Key (a scoped
//     create-task key), then poll {MOC_URL}/tasks/{id} + /logs/{id}. A cutlass
//     fleet node picks the task up and deploys to Pages' MCP. Pages never holds
//     an OpenRouter key. Needs MOC_URL + MOC_API_KEY.
package compose

import (
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/ElcanoTek/pages/internal/env"
)

// Prefix is where the handler is mounted on the dashboard host.
const Prefix = "/api/v1/admin/compose"

// logCap keeps the last N lines per job.
const logCap = 400

// slugRE matches the canonical Pages slug shape so revise mode works for the
// same long, nested paths as the rest of the admin UI (client/q2-report).
var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*(?:/[a-z0-9]+(?:[-_][a-z0-9]+)*)*$`)

var (
	ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	// C0 control characters and DEL; keeps \n (the line split) and \t.
	ctrlRE = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)
)

func driver() string { return strings.ToLower(env.String("COMPOSE_DRIVER", "spawn")) }

func mocURL() string { return strings.TrimRight(env.String("MOC_URL", ""), "/") }

// Enabled reports whether the panel is switched on (PAGES_COMPOSE=1) and its
// driver is configured.
func Enabled() bool {
	if os.Getenv("PAGES_COMPOSE") != "1" {
		return false
	}
	if driver() == "moc" {
		return mocURL() != "" && env.String("MOC_API_KEY", "") != ""
	}
	return env.String("CUTLASS_BIN", "") != "" && env.String("CUTLASS_DIR", "") != ""
}

// DriverLabel names the configured driver for the startup warning.
func DriverLabel() string {
	if driver() == "moc" {
		return "moc (" + mocURL() + ")"
	}
	return "spawn (" + env.String("CUTLASS_BIN", "") + ")"
}

// ChildEnv is the ONLY environment the spawned cutlass CLI may inherit. The
// server process env carries every Pages secret (PAGE_COOKIE_SECRET,
// RAW_TOKEN_SECRET, API_TOKEN_PEPPER, DATABASE_URL, AUTH_*); handing the whole
// thing to an agent runtime that ships prompts to external LLM APIs would turn
// a child-side leak (crash dump, verbose log, sketchy skill) into a full
// server compromise. Cutlass reads its own .env from CUTLASS_DIR, so
// inheritance is convenience, not design: pass a minimal allowlist — shell
// basics, proxies, and the child's own config namespaces — and never the
// server's secret namespaces (the denylist wins).
func ChildEnv(environ map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range environ {
		if childEnvDeny.MatchString(key) {
			continue
		}
		if childEnvExact[key] || hasChildPrefix(key) {
			out[key] = value
		}
	}
	return out
}

var childEnvExact = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true, "TMPDIR": true, "NODE_ENV": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "no_proxy": true,
}

var childEnvPrefixes = []string{"PAGES_MCP_", "CUTLASS_", "OPENROUTER_"}

var childEnvDeny = regexp.MustCompile(`^(?:PAGE_COOKIE_SECRET|RAW_TOKEN_SECRET|API_TOKEN_PEPPER|ADMIN_CSRF_SECRET|DATABASE_URL|PG[A-Z0-9_]*|AUTH_[A-Z0-9_]*|MOC_API_KEY|DEV_ADMIN_COOKIE)$`)

func hasChildPrefix(key string) bool {
	for _, p := range childEnvPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// processEnv is os.Environ as a map.
func processEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

func clean(s string) string { return ctrlRE.ReplaceAllString(ansiRE.ReplaceAllString(s, ""), "") }

// job is one compose run. Every field is guarded by mu.
type job struct {
	mu     sync.Mutex
	id     string
	driver string
	mode   string
	status string // running | done | error
	code   *int
	slug   string
	title  string
	log    []string
}

// pushLog appends streamed text line by line, sanitized and capped.
func (j *job) pushLog(chunk string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, line := range strings.Split(clean(chunk), "\n") {
		if line == "" {
			continue
		}
		j.log = append(j.log, line)
		if len(j.log) > logCap {
			j.log = j.log[1:]
		}
	}
}

// setLog replaces the whole log (the moc driver re-renders it each poll).
func (j *job) setLog(lines []string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = clean(l)
	}
	if len(out) > logCap {
		out = out[len(out)-logCap:]
	}
	j.log = out
}

func (j *job) setStatus(s string) {
	j.mu.Lock()
	j.status = s
	j.mu.Unlock()
}

func buildTask(prompt, slug, title, mode string) string {
	if mode == "revise" {
		return strings.Join([]string{
			"Use the pages MCP server to AUDIT and REVISE an existing client dashboard page.",
			"Page slug: " + slug,
			"",
			`First call get_page("` + slug + `") to read the CURRENTLY PUBLISHED HTML. Audit it`,
			"against the reviewer feedback below — keep what already works, fix what's wrong.",
			"REVISE the existing page; do not rebuild from scratch unless it is unusable, and",
			"preserve its data and structure unless the feedback says otherwise. Keep it",
			"self-contained and themed (Flag design-system tokens/classes; no external CDNs).",
			"",
			"Reviewer feedback / what to change:",
			prompt,
			"",
			`Then call deploy_page with { slug, html: <the full improved HTML>, render_mode:`,
			`"themed", publish: true, note: "Revision per reviewer feedback" }. Call page_urls`,
			"and report the URLs. Make reasonable choices — do not ask follow-up questions.",
		}, "\n")
	}
	if title == "" {
		title = slug
	}
	return strings.Join([]string{
		"Use the pages MCP server to create and publish a client dashboard page.",
		"Page slug: " + slug,
		"Page title: " + title,
		"",
		"Write self-contained, themed HTML using the Flag design system (semantic CSS",
		"tokens/classes already served by the host — do NOT pull external CDNs).",
		"Page requirements from the requester:",
		prompt,
		"",
		`Steps: call deploy_page with { slug, title, html, render_mode: "themed",`,
		"publish: true }. Then call page_urls and report the resulting URLs. Do not ask",
		"follow-up questions — make reasonable choices and finish.",
	}, "\n")
}
