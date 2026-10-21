// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/tokens"
)

// `pages token` mints, lists and revokes agent API tokens (PLAN.md §9). It
// needs DATABASE_URL (or PG*) and the SERVICE's API_TOKEN_PEPPER: a token
// minted with a different or absent pepper hashes to something the server
// never matches. On an installed box it runs as the service account with the
// service environment file (serviceenv.go), so `sudo pages token add` works.
//
//	pages token add <label> [scope] [slug ...]
//	  scope: deploy (default) | admin | data_update (may start with no grants)
//	pages token list
//	pages token revoke <id>
//
// The raw token is printed ONCE by `add` — copy it; it is not recoverable.

const tokenUsage = "usage: pages token add <label> [scope] [slug ...] | list | revoke <id>" //nolint:gosec // G101: usage text, not a credential

func init() {
	Register(&Command{
		Name:    "token",
		Summary: "add, list or revoke agent API tokens",
		Run:     serviceVerb("token", func(args []string) int { return runToken(context.Background(), args, os.Stdout, os.Stderr) }),
	})
}

func runToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "add", "list", "revoke":
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, tokenUsage)
		return 0
	default:
		fmt.Fprintln(stdout, tokenUsage)
		return 1
	}
	if err := tokens.Validate(); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	d, err := db.OpenFromEnv()
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	defer d.Close()
	if err := tokenVerb(ctx, d, cmd, args, stdout); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	return 0
}

func tokenVerb(ctx context.Context, d *db.DB, cmd string, args []string, out io.Writer) error {
	switch cmd {
	case "add":
		if len(args) == 0 || args[0] == "" {
			return errors.New("usage: pages token add <label> [scope] [slug ...]")
		}
		scope := "deploy"
		if len(args) > 1 && args[1] != "" {
			scope = args[1]
		}
		var slugs []string
		if len(args) > 2 {
			slugs = args[2:]
		}
		t, err := tokens.Mint(ctx, d, tokens.MintParams{Label: args[0], Scope: scope, AllowedSlugs: slugs})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "created token #%s  label=%s  scope=%s  prefix=%s\n", t.ID, t.Label, t.Scope, t.Prefix)
		if len(t.AllowedSlugs) > 0 {
			fmt.Fprintf(out, "allowed slugs: %s\n", strings.Join(t.AllowedSlugs, ", "))
		}
		fmt.Fprintf(out, "\n  %s\n\n", t.Token)
		fmt.Fprintln(out, "^ copy this now — it will not be shown again.")
	case "list":
		rows, err := tokens.List(ctx, d)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Fprintln(out, "(no tokens)")
			return nil
		}
		for _, r := range rows {
			state := "active"
			if !r.RevokedAt.IsZero() {
				state = "REVOKED"
			}
			used := "never used"
			if !r.LastUsedAt.IsZero() {
				used = "used " + jsx.FormatTime(r.LastUsedAt.Time)
			}
			grants := ""
			if len(r.AllowedSlugs) > 0 {
				grants = "  slugs=" + strings.Join(r.AllowedSlugs, ",")
			}
			fmt.Fprintf(out, "#%s  %s…  %s  [%s]  %s  %s%s\n", r.ID, r.Prefix, r.Label, r.Scope, state, used, grants)
		}
	case "revoke":
		arg := ""
		if len(args) > 0 {
			arg = args[0]
		}
		id, ok := jsInteger(arg, len(args) > 0)
		if !ok {
			return errors.New("usage: pages token revoke <id>")
		}
		revoked, err := tokens.Revoke(ctx, d, id)
		if err != nil {
			return err
		}
		if revoked {
			fmt.Fprintf(out, "revoked token #%d\n", id)
		} else {
			fmt.Fprintf(out, "no active token #%d\n", id)
		}
	}
	return nil
}

// jsInteger is Number.isInteger(Number(s)), as the Node release checked the
// id: surrounding whitespace is ignored, "" is 0, and 0x/0o/0b prefixes are
// accepted. present is false when no argument was given (Number(undefined)
// is NaN).
func jsInteger(s string, present bool) (int64, bool) {
	if !present {
		return 0, false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	if len(s) > 2 && s[0] == '0' && strings.ContainsRune("xXoObB", rune(s[1])) {
		n, err := strconv.ParseInt(s, 0, 64)
		return n, err == nil
	}
	if strings.ContainsAny(s, "xXpP_nN") { // hex floats, underscores, NaN/Infinity
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f != math.Trunc(f) || math.IsInf(f, 0) || math.Abs(f) > math.MaxInt64/2 {
		return 0, false
	}
	return int64(f), true
}
