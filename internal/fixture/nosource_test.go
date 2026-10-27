// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"strings"
	"testing"
)

// A page created without data reads as freshness with a null source_as_of
// beside its refresh. The fixture judges it as the server does: no source
// data yet, stale from its creation, and never "since Jan 1, 1970".
func TestFixtureHealthNoSourceYet(t *testing.T) {
	page := o("slug", "client-new", "is_live", true, "created_at", iso(10*1440),
		"freshness", o("source_as_of", nil, "refreshed_at", iso(10*1440), "last_check_at", iso(30),
			"last_check_outcome", "source_not_updated", "days_since_source", nil))
	h := healthFor(page, "0 9 * * *", "desk@northwind.example")
	if get(h, "health") != "source_stale" || !strings.HasPrefix(str(get(h, "health_reason")), "No source data yet since Jul 12") ||
		get(h, "days_since_source") != nil {
		t.Fatalf("%v", h)
	}
	// Unscheduled: no age, so no hint.
	if h := healthFor(page, "", ""); get(h, "unscheduled_stale") != false || get(h, "days_since_source") != nil {
		t.Fatalf("%v", h)
	}
}
