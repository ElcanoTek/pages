// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package cli is the pages binary's verb registry. `pages serve` runs the
// server; every other verb is the operator CLI (doctor, update, backup, token,
// …). Each verb registers itself from its own file with Register, so adding
// one never edits a shared switch.
package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Command is one verb.
type Command struct {
	Name    string
	Aliases []string
	// Summary is the one line `pages help` prints.
	Summary string
	// Hidden verbs work but are left out of `pages help` (development and
	// test harness entry points).
	Hidden bool
	// Run executes the verb with the arguments after it and returns the
	// process exit code.
	Run func(args []string) int
}

var registry = map[string]*Command{}

// Register adds c. It panics on a duplicate name, which is a programming error.
func Register(c *Command) {
	for _, n := range append([]string{c.Name}, c.Aliases...) {
		if _, dup := registry[n]; dup {
			panic("cli: duplicate command " + n)
		}
		registry[n] = c
	}
}

// Lookup returns the verb registered under name.
func Lookup(name string) (*Command, bool) {
	c, ok := registry[name]
	return c, ok
}

// Main dispatches args (os.Args[1:]) and returns the exit code.
func Main(args []string) int {
	if len(args) == 0 {
		Usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		Usage(os.Stdout)
		return 0
	}
	c, ok := registry[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "pages: unknown command %q\n\n", args[0])
		Usage(os.Stderr)
		return 2
	}
	return c.Run(args[1:])
}

// Usage prints the verb list.
func Usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: pages <command> [arguments]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	seen := map[*Command]bool{}
	var cmds []*Command
	for _, c := range registry {
		if !seen[c] && !c.Hidden {
			seen[c] = true
			cmds = append(cmds, c)
		}
	}
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	width := 0
	for _, c := range cmds {
		width = max(width, len(c.Name))
	}
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.Name, c.Summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `pages <command> --help` for a command's own options.")
}

// Fail prints "pages <verb>: msg" to stderr and returns exit code 1.
func Fail(verb string, format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "pages %s: %s\n", verb, strings.TrimSpace(fmt.Sprintf(format, args...)))
	return 1
}
