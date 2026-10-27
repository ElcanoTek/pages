// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
)

// Earlier revisions of the shipped nwm-campaign-dashboard template wrote
// DATA.lastRefreshed into the page header with innerHTML, and their schema
// accepted any string. The template is fixed, but a page built from an
// earlier revision keeps that revision's source (stored page versions are
// append-only, and Pages never rewrites a client's markup), so a data
// refresh could still carry markup into it. Until such a page is rebuilt
// from the current revision, every data write to a page whose layout still
// has that sink is refused if lastRefreshed could form markup. Plain
// refresh stamps ("Aug 2, 2026", ISO timestamps) are unaffected.
var legacyStampSink = regexp.MustCompile(`\.innerHTML\s*=\s*CONFIG\.lastRefreshed\s*\?`)

// assertLegacyStampSafe applies that guard to proposed data for m.
func assertLegacyStampSafe(m *Managed, data any) error {
	o, ok := data.(*jsonv.Object)
	if !ok {
		return nil
	}
	stamp, ok := o.Value("lastRefreshed").(string)
	if !ok || !strings.ContainsAny(stamp, "<>&") || !legacyStampSink.MatchString(m.HTML) {
		return nil
	}
	return apierror.BadRequest("data does not satisfy the page's JSON Schema", "data_validation_failed", ValidationDetails{
		ValidationErrors: []BoundedError{{
			InstancePath: "/lastRefreshed", SchemaPath: "#/properties/lastRefreshed", Keyword: "pattern",
			Message: "must be plain text: this page's layout writes it as HTML, so <, > and & are refused; rebuild the page from the current template revision",
		}},
		TotalErrors: 1,
	})
}
