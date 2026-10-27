// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/jackc/pgx/v5"
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

// The template read and validated before the transaction is reused under
// the lock when the published pointer has not moved, and read again when it
// has.
func TestPrecheckedSnapshotIsReusedOnlyWhileCurrent(t *testing.T) {
	d, ctx := setup(t)
	minted := must(tokens.Mint(ctx, d, tokens.MintParams{Label: "snapshot-agent"}))(t)
	bot := Actor{Actor: "snapshot-agent", ActorType: "agent", TokenID: minted.ID, IP: "127.0.0.1"}
	base := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{DeployInput: DeployInput{
		Slug: "snapshot", HTML: managedHTML(hoursAgo(4), `{"count":1,"label":"initial"}`), Source: SourceMCP, Publish: true,
	}, Title: "Snapshot"}, bot))(t)

	parses := 0
	orig := parsePublishedTemplate
	parsePublishedTemplate = func(html string) (*pagedata.Managed, error) { parses++; return orig(html) }
	t.Cleanup(func() { parsePublishedTemplate = orig })

	// Unchanged pointer: no second read or parse.
	v1 := must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "snapshot", Data: jv(t, `{"count":2,"label":"two"}`), SourceAsOf: hoursAgo(3), ExpectedVersion: base.Version.ID.String()}, bot))(t)
	if parses != 0 {
		t.Fatalf("the locked path parsed the template %d times despite a current snapshot", parses)
	}

	// A snapshot of a version that is no longer published is not used.
	stale, _, ok := precheckSnapshot(ctx, d, DataUpdateInput{Slug: "snapshot", Data: jv(t, `{"count":3,"label":"three"}`), ExpectedVersion: v1.Version.ID.String()})
	if !ok {
		t.Fatal("no snapshot")
	}
	stale.versionID = base.Version.ID // as if read before v1 was published
	in := DataUpdateInput{Slug: "snapshot", Data: jv(t, `{"count":3,"label":"three"}`), SourceAsOf: hoursAgo(2), ExpectedVersion: v1.Version.ID.String(), snapshot: stale}
	must(db.WithTxResult(ctx, d, func(tx pgx.Tx) (*DataUpdateResult, error) { return UpdatePageDataWithTx(ctx, tx, in, bot) }))(t)
	if parses != 1 {
		t.Fatalf("a stale snapshot must be replaced by the locked read: %d parses", parses)
	}
}
