// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/ElcanoTek/pages/internal/auth/central"
	"github.com/ElcanoTek/pages/internal/testdb"
)

func TestAccessVerb(t *testing.T) {
	d := testdb.New(t)
	store := central.NewStore(d)
	ctx := context.Background()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := accessMain(ctx, store, args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	steps := []struct {
		args          []string
		code          int
		stdout, stder string
	}{
		{[]string{"list"}, 0, "(no one has access)\n", ""},
		{[]string{"grant", " Ana@Northwind.Example "}, 0, "granted ana@northwind.example\n", ""},
		{[]string{"grant", "ben@northwind.example"}, 0, "granted ben@northwind.example\n", ""},
		{[]string{"list"}, 0, "ana@northwind.example\nben@northwind.example\n", ""},
		{[]string{"revoke", "ana@northwind.example"}, 0, "revoked ana@northwind.example and ended its sessions\n", ""},
		{[]string{"revoke", "ana@northwind.example"}, 0, "ana@northwind.example had no active access\n", ""},
		{[]string{"grant", "not-an-email"}, 1, "", "a valid email address is required\n"},
		{[]string{"grant"}, 1, "", "usage: pages access grant <email>\n"},
		{[]string{"bogus"}, 1, accessUsage + "\n", ""},
		{nil, 1, accessUsage + "\n", ""},
	}
	for _, s := range steps {
		code, out, errOut := run(s.args...)
		if code != s.code || out != s.stdout || errOut != s.stder {
			t.Errorf("%v: %d %q %q", s.args, code, out, errOut)
		}
	}
}
