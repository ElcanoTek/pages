// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package runner is the one door the operator tooling uses to run external
// commands (systemctl, runuser, pg_dump, caddy, git, journalctl, sshd, dnf …).
// Every operator flow takes a Runner instead of calling os/exec directly, so a
// test can hand it a Fake that records the calls and scripts their outcomes —
// the Go counterpart of the shimmed-PATH fakes the shell tests used — and the
// flows can be asserted end to end without root, systemd or a real box.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Cmd is one external command.
type Cmd struct {
	// Name is the program (looked up on PATH unless it contains a slash).
	Name string
	Args []string
	// Env is the complete child environment; nil inherits the caller's.
	Env []string
	// Dir is the working directory ("" ⇒ the caller's).
	Dir string
	// Stdin feeds the child; nil means no input.
	Stdin io.Reader
	// Stdout and Stderr, when set, receive the child's output as it is
	// produced (in addition to nothing: Result then holds no copy of that
	// stream). When unset the stream is captured into Result.
	Stdout io.Writer
	Stderr io.Writer
}

// String renders the command for logs and test assertions: name and argv
// joined by spaces. It never includes the environment.
func (c Cmd) String() string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

// Result is what a finished command produced.
type Result struct {
	// ExitCode is the process exit status (-1 when it could not start or was
	// killed by a signal).
	ExitCode int
	Stdout   string
	Stderr   string
}

// ErrNotFound is returned (wrapped) when the program does not exist.
var ErrNotFound = errors.New("command not found")

// ExitError reports a command that ran and exited nonzero.
type ExitError struct {
	Cmd    string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s: exit status %d", e.Cmd, e.Code)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

// Runner runs commands and answers PATH lookups.
type Runner interface {
	// Run executes c. A nonzero exit returns the Result together with an
	// *ExitError; a program that cannot start returns an error wrapping the
	// cause (ErrNotFound for a missing program).
	Run(ctx context.Context, c Cmd) (Result, error)
	// LookPath reports where name would be found, or an error.
	LookPath(name string) (string, error)
}

// Exec is the real Runner, backed by os/exec.
type Exec struct{}

// Run implements Runner.
func (Exec) Run(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = c.Dir
	cmd.Stdin = c.Stdin
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	cmd.Stderr = &errb
	if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	}
	err := cmd.Run()
	res := Result{ExitCode: 0, Stdout: out.String(), Stderr: errb.String()}
	if err == nil {
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, &ExitError{Cmd: c.Name, Code: res.ExitCode, Stderr: res.Stderr}
	}
	res.ExitCode = -1
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("%s: %w", c.Name, ErrNotFound)
	}
	return res, err
}

// LookPath implements Runner.
func (Exec) LookPath(name string) (string, error) { return exec.LookPath(name) }

// Default is the Runner production code uses.
var Default Runner = Exec{}

// Output runs c and returns trimmed stdout, or the error.
func Output(ctx context.Context, r Runner, c Cmd) (string, error) {
	res, err := r.Run(ctx, c)
	return strings.TrimSpace(res.Stdout), err
}

// OK reports whether c ran and exited 0.
func OK(ctx context.Context, r Runner, c Cmd) bool {
	_, err := r.Run(ctx, c)
	return err == nil
}

// Have reports whether name is on PATH.
func Have(r Runner, name string) bool {
	_, err := r.LookPath(name)
	return err == nil
}

// ExitCode returns the exit code carried by err (0 for nil, -1 when the
// command did not run at all).
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

// Fake is a scripted Runner for tests. Handle decides each call's outcome;
// every call is recorded (with its environment) in Calls.
type Fake struct {
	mu sync.Mutex
	// Handle returns the outcome of c: stdout, exit code. A nil Handle makes
	// every command succeed with no output.
	Handle func(c Cmd) (stdout string, code int)
	// Missing lists programs LookPath (and Run) report as not installed.
	Missing map[string]bool
	calls   []Cmd
}

// Run implements Runner.
func (f *Fake) Run(_ context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		c.Stdin = bytes.NewReader(b)
	}
	f.calls = append(f.calls, c)
	missing := f.Missing[c.Name]
	handle := f.Handle
	f.mu.Unlock()
	if missing {
		return Result{ExitCode: -1}, fmt.Errorf("%s: %w", c.Name, ErrNotFound)
	}
	out, code := "", 0
	if handle != nil {
		out, code = handle(c)
	}
	if c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, out)
		out = ""
	}
	res := Result{ExitCode: code, Stdout: out}
	if code != 0 {
		return res, &ExitError{Cmd: c.Name, Code: code}
	}
	return res, nil
}

// LookPath implements Runner.
func (f *Fake) LookPath(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[name] {
		return "", fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return "/usr/bin/" + name, nil
}

// Calls returns a copy of the recorded calls.
func (f *Fake) Calls() []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Cmd(nil), f.calls...)
}

// Log returns every recorded call's String(), one per line.
func (f *Fake) Log() string {
	var b strings.Builder
	for _, c := range f.Calls() {
		b.WriteString(c.String())
		b.WriteByte('\n')
	}
	return b.String()
}
