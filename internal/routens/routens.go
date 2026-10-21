// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package routens is the namespace contract for content routes, admin screens
// and page API actions. Any segment is reserved so nested slugs cannot be
// interpreted as actions. Creation and restoration enforce this; legacy reads
// retain their URLs. New reservations need a rollout collision check before
// their routes ship.
package routens

import (
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
)

// ReservedSlugSegments lists every reserved segment, in declaration order.
var ReservedSlugSegments = []string{
	"raw", "raw-template", "assets", "healthz", "readyz", "welcome",
	"versions", "publish", "rollback", "approve", "reject", "approval",
	"password", "title", "theme", "disable", "enable", "delete", "restore",
	"preview-token", "edit-token", "deploy-source", "preflight", "workspace",
	"templates", "portal", "portals",
}

var reserved = func() map[string]bool {
	m := make(map[string]bool, len(ReservedSlugSegments))
	for _, s := range ReservedSlugSegments {
		m[s] = true
	}
	return m
}()

// IsReserved reports whether segment is reserved.
func IsReserved(segment string) bool { return reserved[segment] }

// AssertSlugNotReserved returns a 400 reserved_slug *apierror.Error when any
// segment of slug is reserved, else nil.
func AssertSlugNotReserved(slug string) error {
	for _, segment := range strings.Split(slug, "/") {
		if reserved[segment] {
			return apierror.BadRequest("slug segment '"+segment+"' is reserved (collides with a route)", "reserved_slug", nil)
		}
	}
	return nil
}
