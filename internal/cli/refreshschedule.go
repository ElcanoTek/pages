// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/versions"
)

// `pages refresh-schedule` sets, clears and lists the refresh schedule a
// page's health is judged against (internal/versions/refreshhealth.go), for
// an operator backfilling existing pages over ssh:
//
//	pages refresh-schedule list [--all]
//	pages refresh-schedule set <slug> --cron '<cron>' [--owner <email>]
//	pages refresh-schedule set <slug> --owner <email>
//	pages refresh-schedule clear <slug>
//
// The cron is five fields in UTC ('30 14 * * *', '0 15 * * 1-5'); Pages runs
// nothing on it. `list` shows the scheduled pages and how each stands;
// `--all` adds the unscheduled ones, the unscheduled_stale ones first (live,
// no schedule, data over a week old: the pages a backfill is for), and the
// closing counts always say how many of those there are. `clear` removes
// both the schedule and the owner. Every change is audited under the
// operator's name, exactly as the admin screen and the
// set_page_refresh_schedule MCP tool are.
//
// Requires DATABASE_URL (or PG*); on an installed box it is read from the
// service environment file (serviceenv.go).

const refreshScheduleUsage = "usage: pages refresh-schedule list [--all] | set <slug> --cron '<cron>' [--owner <email>] | clear <slug>"

func init() {
	Register(&Command{
		Name:    "refresh-schedule",
		Summary: "list, set or clear the refresh schedule pages are judged against",
		Run: serviceVerb("refresh-schedule", func(args []string) int {
			return runRefreshSchedule(context.Background(), args, os.Stdout, os.Stderr, db.OpenFromEnv)
		}),
	})
}

func runRefreshSchedule(ctx context.Context, args []string, stdout, stderr io.Writer, open func() (*db.DB, error)) int {
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "list", "set", "clear":
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, refreshScheduleUsage)
		return 0
	default:
		fmt.Fprintln(stderr, refreshScheduleUsage)
		return 2
	}
	all := false
	var rest []string
	for _, a := range args {
		if a == "--all" && cmd == "list" {
			all = true
			continue
		}
		rest = append(rest, a)
	}
	flags, positional, err := parseTemplateFlags(rest)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if err := checkRefreshScheduleArgs(cmd, flags, positional); err != nil {
		fmt.Fprintln(stderr, err.Error())
		fmt.Fprintln(stderr, refreshScheduleUsage)
		return 2
	}
	d, err := open()
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	defer d.Close()
	if err := refreshScheduleVerb(ctx, d, cmd, all, flags, positional, stdout); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	return 0
}

// checkRefreshScheduleArgs refuses a malformed command line before a
// database is opened.
func checkRefreshScheduleArgs(cmd string, flags map[string]string, positional []string) error {
	allowed := map[string]bool{}
	switch cmd {
	case "list":
		if len(positional) != 0 {
			return errors.New("list takes no page")
		}
	case "set":
		if len(positional) != 1 {
			return errors.New("set takes exactly one page slug")
		}
		allowed["cron"], allowed["owner"] = true, true
		if flags["cron"] == "" && flags["owner"] == "" {
			return errors.New("set needs --cron, --owner or both")
		}
	case "clear":
		if len(positional) != 1 {
			return errors.New("clear takes exactly one page slug")
		}
	}
	for k := range flags {
		if !allowed[k] {
			return fmt.Errorf("unknown option --%s", k)
		}
	}
	return nil
}

func refreshScheduleVerb(ctx context.Context, d *db.DB, cmd string, all bool, flags map[string]string, positional []string, out io.Writer) error {
	switch cmd {
	case "list":
		report, err := versions.ListRefreshHealth(ctx, d, false)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SLUG\tHEALTH\tSCHEDULE (UTC)\tOWNER\tREASON")
		shown := 0
		for _, p := range report.Pages {
			if !all && p.RefreshSchedule == nil {
				continue
			}
			shown++
			reason := p.HealthReason
			if p.UnscheduledStale && p.DaysSinceSource != nil {
				reason = versions.UnscheduledStaleHint(*p.DaysSinceSource)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Slug, p.Health, orDash(p.RefreshSchedule), orDash(p.RefreshOwner), reason)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		c := report.Counts
		fmt.Fprintf(out, "\n%d shown · blocked %d · overdue %d · source stale %d · ok %d · unscheduled %d · unscheduled stale %d\n",
			shown, c.Blocked, c.Overdue, c.SourceStale, c.OK, c.Unscheduled, c.UnscheduledStale)
		if c.UnscheduledStale > 0 {
			where := ""
			if !all {
				where = " (--all lists them)"
			}
			fmt.Fprintf(out, "unscheduled stale: live, no refresh schedule, data over %d days old%s — set a schedule, or ignore if the campaign has ended\n",
				int(versions.UnscheduledStaleAfter/(24*time.Hour)), where)
		}
		return nil
	case "set", "clear":
		in := versions.RefreshScheduleInput{Slug: positional[0]}
		if cmd == "clear" {
			in.Schedule, in.Owner = versions.Null, versions.Null
		} else {
			if v, ok := flags["cron"]; ok {
				in.Schedule = v
			}
			if v, ok := flags["owner"]; ok {
				in.Owner = v
			}
		}
		res, err := versions.SetRefreshSchedule(ctx, d, in, templateActor())
		if err != nil {
			return err
		}
		h := res.RefreshHealth
		verb := "updated"
		if !res.Changed {
			verb = "unchanged"
		}
		fmt.Fprintf(out, "%s %s: schedule %s, owner %s\n", verb, res.Slug, orDash(h.RefreshSchedule), orDash(h.RefreshOwner))
		fmt.Fprintf(out, "health %s — %s\n", h.Health, h.HealthReason)
		if !h.NextDueAt.IsZero() {
			fmt.Fprintf(out, "next refresh due %s\n", h.NextDueAt.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

func orDash(s *string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return "-"
	}
	return *s
}
