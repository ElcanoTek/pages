// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package term is the operator tooling's output: the » info, ▸ step, ✓ ok,
// ! warn and ✗ error lines bootstrap/doctor/update print, colored only on a
// terminal (and never with TERM=dumb), plus the TTY helpers prompts need.
package term

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// Printer writes operator messages.
type Printer struct {
	mu    sync.Mutex
	Out   io.Writer
	Err   io.Writer
	Color bool
}

// New returns a Printer on stdout/stderr, colored when stdout is a terminal.
func New() *Printer {
	return &Printer{Out: os.Stdout, Err: os.Stderr, Color: IsTerminal(os.Stdout) && os.Getenv("TERM") != "dumb"}
}

// Buffer returns an uncolored Printer writing both streams to w (tests).
func Buffer(w io.Writer) *Printer { return &Printer{Out: w, Err: w} }

// Palette escape codes.
const (
	reset  = "\033[0m"
	dim    = "\033[2m"
	red    = "\033[0;31m"
	green  = "\033[0;32m"
	yellow = "\033[0;33m"
	cyan   = "\033[0;36m"
	bold   = "\033[1m"
)

// C returns code when color is on.
func (p *Printer) C(code string) string {
	if p.Color {
		return code
	}
	return ""
}

// Dim, Bold, Reset are the codes callers splice into their own lines.
func (p *Printer) Dim() string   { return p.C(dim) }
func (p *Printer) Bold() string  { return p.C(bold) }
func (p *Printer) Reset() string { return p.C(reset) }
func (p *Printer) Green() string { return p.C(green) }
func (p *Printer) Cyan() string  { return p.C(cyan) }

func (p *Printer) line(w io.Writer, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(w, format, args...)
}

// Say prints a plain line.
func (p *Printer) Say(s string) { p.line(p.Out, "%s\n", s) }

// Info prints "» s" dimmed.
func (p *Printer) Info(s string) { p.line(p.Out, "%s» %s%s\n", p.C(dim), s, p.C(reset)) }

// Step prints a blank line and "▸ s" in bold.
func (p *Printer) Step(s string) { p.line(p.Out, "\n%s▸ %s%s\n", p.C(bold), s, p.C(reset)) }

// OK prints "✓ s".
func (p *Printer) OK(s string) { p.line(p.Out, "%s✓ %s%s\n", p.C(green), s, p.C(reset)) }

// Warn prints "! s" to stderr.
func (p *Printer) Warn(s string) { p.line(p.Err, "%s! %s%s\n", p.C(yellow), s, p.C(reset)) }

// Error prints "✗ s" to stderr.
func (p *Printer) Error(s string) { p.line(p.Err, "%s✗ %s%s\n", p.C(red), s, p.C(reset)) }

// Plain prints s to stdout and Plainerr to stderr, uncolored and unprefixed
// (the updater's terse lines).
func (p *Printer) Plain(s string)    { p.line(p.Out, "%s\n", s) }
func (p *Printer) PlainErr(s string) { p.line(p.Err, "%s\n", s) }

// Mark prints "<mark> s" with the mark colored (doctor's ✓ ↻ ! ✗ rows).
func (p *Printer) Mark(mark, color, s string) {
	p.line(p.Out, "%s%s%s %s\n", p.C(color), mark, p.C(reset), s)
}

// Colors for Mark.
const (
	MarkGreen  = green
	MarkCyan   = cyan
	MarkYellow = yellow
	MarkRed    = red
)

// IsTerminal reports whether f is a character device (a TTY).
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
