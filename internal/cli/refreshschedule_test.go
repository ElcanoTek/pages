// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/versions"
)

func runRefreshScheduleCLI(t *testing.T, args ...string) cliRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runRefreshSchedule(context.Background(), args, &stdout, &stderr, db.OpenFromEnv)
	return cliRun{code: code, stdout: stdout.String(), out: stdout.String() + stderr.String()}
}

// A malformed command line is refused before any database is opened.
func TestRefreshScheduleArgs(t *testing.T) {
	noDB := func() (*db.DB, error) { return nil, errors.New("must not open a database") }
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"set"},
		{"set", "a", "b", "--cron", "0 15 * * *"},
		{"set", "northwind"},
		{"set", "northwind", "--cron"},
		{"set", "northwind", "--schedule", "0 15 * * *"},
		{"clear"},
		{"clear", "northwind", "--owner", "a@example.com"},
		{"list", "northwind"},
		{"list", "--owner", "a@example.com"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runRefreshSchedule(context.Background(), args, &stdout, &stderr, noDB); code != 2 {
			t.Errorf("%q: exit %d (%s)", args, code, stderr.String())
		}
	}
	var stdout bytes.Buffer
	if code := runRefreshSchedule(context.Background(), []string{"--help"}, &stdout, &stdout, noDB); code != 0 || !strings.Contains(stdout.String(), "usage: pages refresh-schedule") {
		t.Fatal(code, stdout.String())
	}
}

func TestRefreshScheduleVerb(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	seed := versions.Actor{Actor: "seed", ActorType: "agent"}
	for _, slug := range []string{"cli/northwind", "cli/contoso"} {
		if _, err := versions.CreatePage(ctx, d, versions.CreatePageInput{Slug: slug, Title: slug}, seed); err != nil {
			t.Fatal(err)
		}
	}
	// A live page with no schedule whose data stopped ten days ago: the kind a
	// backfill is for.
	source := jsx.FormatTime(time.Now().Add(-10*24*time.Hour - time.Hour))
	html := `<!doctype html><html><head><title>Tailspin</title></head><body><main><h1>Tailspin</h1></main>` +
		`<script id="pages-data-schema" type="application/schema+json">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}</script>` +
		`<script id="pages-data" type="application/json">{"contract_version":1,"refreshed_at":"` + source + `","source_as_of":"` + source + `","data":{}}</script></body></html>`
	if _, err := versions.CreateAndDeploy(ctx, d, versions.CreateAndDeployInput{
		DeployInput: versions.DeployInput{Slug: "cli/tailspin", HTML: html, Source: versions.SourceMCP, Publish: true},
		Title:       "Tailspin",
	}, seed); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUDO_USER", "ops-person")

	set := runRefreshScheduleCLI(t, "set", "cli/northwind", "--cron", "30 14 * * 1-5", "--owner", "Trader@Example.com")
	if set.code != 0 || !strings.Contains(set.stdout, "updated cli/northwind: schedule 30 14 * * 1-5, owner trader@example.com") ||
		!regexp.MustCompile(`(?m)^health \w+ — .+$`).MatchString(set.stdout) || !strings.Contains(set.stdout, "next refresh due ") {
		t.Fatalf("%+v", set)
	}
	var actor, actorType string
	if err := d.QueryRow(ctx, `SELECT actor, actor_type FROM audit_log WHERE action = 'set_refresh_schedule' ORDER BY id DESC LIMIT 1`).Scan(&actor, &actorType); err != nil {
		t.Fatal(err)
	}
	if actor != "pages-cli:ops-person" || actorType != "system" {
		t.Fatal(actor, actorType)
	}
	if again := runRefreshScheduleCLI(t, "set", "cli/northwind", "--cron", "30  14 * * 1-5"); again.code != 0 || !strings.HasPrefix(again.stdout, "unchanged cli/northwind") {
		t.Fatalf("%+v", again)
	}
	if owner := runRefreshScheduleCLI(t, "set", "cli/northwind", "--owner", "desk@example.com"); owner.code != 0 ||
		!strings.Contains(owner.stdout, "schedule 30 14 * * 1-5, owner desk@example.com") {
		t.Fatalf("an owner alone leaves the schedule: %+v", owner)
	}

	list := runRefreshScheduleCLI(t, "list")
	if list.code != 0 || !regexp.MustCompile(`(?m)^SLUG\s+HEALTH\s+SCHEDULE \(UTC\)\s+OWNER\s+REASON$`).MatchString(list.stdout) ||
		!regexp.MustCompile(`(?m)^cli/northwind\s+\w+\s+30 14 \* \* 1-5\s+desk@example\.com\s+`).MatchString(list.stdout) ||
		strings.Contains(list.stdout, "cli/contoso") || strings.Contains(list.stdout, "cli/tailspin") || !strings.Contains(list.stdout, "1 shown") {
		t.Fatalf("%s", list.stdout)
	}
	// The counts always say how many live pages run without a schedule on old
	// data, and where to see them.
	if !strings.Contains(list.stdout, "· unscheduled 2 · unscheduled stale 1\n") ||
		!strings.Contains(list.stdout, "unscheduled stale: live, no refresh schedule, data over 7 days old (--all lists them) — set a schedule, or ignore if the campaign has ended") {
		t.Fatalf("%s", list.stdout)
	}
	all := runRefreshScheduleCLI(t, "list", "--all")
	if all.code != 0 || !regexp.MustCompile(`(?m)^cli/contoso\s+unscheduled\s+-\s+-\s+No refresh schedule is set`).MatchString(all.stdout) || !strings.Contains(all.stdout, "3 shown") {
		t.Fatalf("%s", all.stdout)
	}
	// The hinted page leads the unscheduled ones, with the hint as its reason.
	stale := regexp.MustCompile(`(?m)^cli/tailspin\s+unscheduled\s+-\s+-\s+No refresh schedule · data 10 days old — set a schedule or ignore if the campaign has ended$`)
	if loc := stale.FindStringIndex(all.stdout); loc == nil || loc[0] > strings.Index(all.stdout, "cli/contoso") ||
		strings.Contains(all.stdout, "(--all lists them)") {
		t.Fatalf("%s", all.stdout)
	}

	if bad := runRefreshScheduleCLI(t, "set", "cli/northwind", "--cron", "0 15 * * MON"); bad.code != 1 || !strings.Contains(bad.out, "names such as MON") {
		t.Fatalf("%+v", bad)
	}
	if missing := runRefreshScheduleCLI(t, "clear", "cli/nope"); missing.code != 1 || !strings.Contains(missing.out, "page not found") {
		t.Fatalf("%+v", missing)
	}
	cleared := runRefreshScheduleCLI(t, "clear", "cli/northwind")
	if cleared.code != 0 || !strings.Contains(cleared.stdout, "updated cli/northwind: schedule -, owner -") || !strings.Contains(cleared.stdout, "health unscheduled") {
		t.Fatalf("%+v", cleared)
	}
}
