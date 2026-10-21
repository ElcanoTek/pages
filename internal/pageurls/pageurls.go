// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package pageurls is lib/mcp-tools.js pageUrls: the three routing URLs every
// page-shaped REST and MCP result carries, so an agent can hand a human the
// admin view, the signed-in viewer and the public content-host address
// without guessing the hosts.
package pageurls

import "github.com/ElcanoTek/pages/internal/env"

// URLs are a page's routing addresses. Field order is the JavaScript object's.
type URLs struct {
	Admin string `json:"admin"`
	View  string `json:"view"`
	Live  string `json:"live"`
}

// For returns slug's URLs. The slug is interpolated verbatim — nested slugs
// keep their raw "/" separators (northwind/q2), exactly as the JavaScript
// template literal did — against DASHBOARD_ORIGIN and CONTENT_ORIGIN read at
// call time.
func For(slug string) URLs {
	dash := env.DashboardOrigin()
	return URLs{
		Admin: dash + "/admin/" + slug,
		View:  dash + "/view/" + slug,
		Live:  env.ContentOrigin() + "/" + slug,
	}
}
