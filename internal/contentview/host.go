// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package contentview

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/env"
)

var portSuffix = regexp.MustCompile(`:\d+$`)

// IsContentHost reports whether r is addressed to the content host:
// CONTENT_HOST, or in local dev the extra CONTENT_HOST_ALSO. It reads the Host
// header itself, never X-Forwarded-Host: client attribution behind a trusted
// proxy must not change the two-host route split.
func IsContentHost(r *http.Request) bool {
	hostname := strings.ToLower(portSuffix.ReplaceAllString(r.Host, ""))
	if hostname == "" {
		return false
	}
	if hostname == strings.ToLower(env.ContentHost()) {
		return true
	}
	also := strings.ToLower(env.String("CONTENT_HOST_ALSO", ""))
	return also != "" && hostname == also
}
