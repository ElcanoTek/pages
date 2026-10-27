// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/tokens"
)

// Schema validation of a data refresh used to run only inside the write
// transaction, holding a pooled connection and the page's row lock for as
// long as validation took. A payload that fails the published schema is now
// refused before the transaction opens: here it is refused while another
// transaction holds the page's lock, which the old path would have waited on.
func TestInvalidDataIsRefusedBeforeTheTransaction(t *testing.T) {
	d, ctx := setup(t)
	minted := must(tokens.Mint(ctx, d, tokens.MintParams{Label: "precheck-agent"}))(t)
	bot := Actor{Actor: "precheck-agent", ActorType: "agent", TokenID: minted.ID, IP: "127.0.0.1"}
	base := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{
		Slug: "precheck", HTML: managedHTML(hoursAgo(4), `{"count":1,"label":"initial"}`), Source: SourceMCP, Publish: true,
	}, Title: "Precheck"}, bot))(t)
	v0 := base.Version.ID.String()

	tx, err := d.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := LockPage(ctx, tx, "precheck"); err != nil {
		t.Fatal(err)
	}

	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err = UpdatePageData(short, d, DataUpdateInput{Slug: "precheck", Data: jv(t, `{"count":-1,"label":4}`), SourceAsOf: hoursAgo(3), ExpectedVersion: v0}, bot)
	wantCode(t, err, 400, "data_validation_failed")
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("validation waited %v for the page lock", waited)
	}

	// Anything the precheck cannot decide falls through to the locked path
	// unchanged: a stale expected_version is still the conflict, not the
	// schema error.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = UpdatePageData(ctx, d, DataUpdateInput{Slug: "precheck", Data: jv(t, `{"count":-1,"label":4}`), SourceAsOf: hoursAgo(3), ExpectedVersion: "999999"}, bot)
	if err == nil {
		t.Fatal("stale expected_version accepted")
	}
	if e, ok := apierror.As(err); !ok || e.Code == "data_validation_failed" {
		t.Fatalf("stale expected_version reported as %v", err)
	}
	// Valid data still goes through.
	must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "precheck", Data: jv(t, `{"count":2,"label":"ok"}`), SourceAsOf: hoursAgo(3), ExpectedVersion: v0}, bot))(t)
}
