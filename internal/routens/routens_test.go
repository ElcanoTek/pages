// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package routens

import (
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
)

func TestAssertSlugNotReserved(t *testing.T) {
	// any segment colliding with a real route is refused at creation
	for _, bad := range []string{"welcome", "raw", "raw/q2", "assets/logo", "healthz", "acme/versions", "acme/versions/3",
		"acme/publish", "acme/deploy-source", "acme/workspace",
		// `portal` is the partner entry point on the content host. The hazard is
		// a page created AT the portal route seizing a partner's bookmarked URL.
		"portal", "portal/nwm", "nwm/portal",
		// …and `portals` is the admin screen, registered before /admin/*slug.
		"portals", "acme/portals"} {
		err := AssertSlugNotReserved(bad)
		e, ok := apierror.As(err)
		if !ok || e.Status != 400 || e.Code != "reserved_slug" {
			t.Errorf("should reject %q: %v", bad, err)
		}
	}
	if e, _ := apierror.As(AssertSlugNotReserved("acme/versions")); e.Message != "slug segment 'versions' is reserved (collides with a route)" {
		t.Errorf("message %q", e.Message)
	}
	// reserved words as substrings (not whole segments) are fine
	for _, ok := range []string{"northwind", "northwind/q2", "rawhide", "my-assets/q2", "welcome-page", "versionsx",
		"my-portal", "portal-nwm", "portals-x", "northwind/quarterly-report"} {
		if err := AssertSlugNotReserved(ok); err != nil {
			t.Errorf("should accept %q: %v", ok, err)
		}
	}
	if len(ReservedSlugSegments) != 27 || !IsReserved("edit-token") || IsReserved("northwind") {
		t.Error("reserved set")
	}
}
