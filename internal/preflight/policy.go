// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package preflight

import "strings"

// DefaultContentPolicy is the content host's /raw Content-Security-Policy as
// the csp package serves it (lib/csp.js rawHeaders), without the trailing
// frame-ancestors directive, which depends on the dashboard origin and which no
// rule here reads. It exists so the package has a correct answer on its own;
// the server passes the live header value as Options.ContentPolicy, so
// preflight judges a document against exactly the policy it will be served
// under and the two can never drift.
const DefaultContentPolicy = "sandbox allow-scripts allow-downloads allow-modals; " +
	"default-src 'none'; " +
	"script-src 'self' 'unsafe-inline' blob:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; " +
	"media-src data: blob:; " +
	"font-src 'self' data:; " +
	"connect-src 'none'; " +
	"form-action 'none'; " +
	"base-uri 'none'; " +
	"object-src 'none'"

// policy is a parsed CSP: directive → sources, later duplicates winning (as a
// JS Map built from the list did).
type policy map[string][]string

func parsePolicy(csp string) policy {
	p := policy{}
	for _, part := range strings.Split(csp, ";") {
		fields := splitJSWhitespace(jsTrim(part))
		p[fields[0]] = fields[1:]
	}
	return p
}

// sources is RESOURCE_POLICY.get(directive) || RESOURCE_POLICY.get("default-src") || [].
func (p policy) sources(directive string) []string {
	if s, ok := p[directive]; ok {
		return s
	}
	return p["default-src"]
}

// sandboxTokens are the tokens of the policy's sandbox directive — the same
// list the csp package builds that directive from, so reading it here cannot
// drift from what is served.
func (p policy) sandboxTokens() map[string]bool {
	out := map[string]bool{}
	for _, t := range p["sandbox"] {
		out[t] = true
	}
	return out
}
