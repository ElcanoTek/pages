// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"regexp"
	"strings"
	"sync"
)

var (
	sensitiveKey     = regexp.MustCompile(`(?i)PASSWORD|SECRET|TOKEN|PEPPER|DATABASE_URL|PGDATABASE`)
	connectionString = regexp.MustCompile(`(?i)postgres(?:ql)?://`)
)

// RemovedLine stands in for a diagnostic line that may hold a credential.
const RemovedLine = "[line removed: it may contain a credential]"

func sensitiveValues(env map[string]string) []string {
	var out []string
	for k, v := range env {
		if v != "" && sensitiveKey.MatchString(k) {
			out = append(out, v)
		}
	}
	return out
}

// RedactLine makes one complete line of child diagnostics safe to print.
// PostgreSQL can echo a connection string, cut to 63 bytes when it was taken
// as a name, and the cut can land anywhere, even inside quotes. So any line
// that shows a connection string, or the start of a sensitive value, is
// dropped whole rather than trimmed; exact sensitive values elsewhere are
// replaced.
func RedactLine(line string, env map[string]string) string {
	return redactLine(line, sensitiveValues(env))
}

func redactLine(line string, sensitive []string) string {
	if connectionString.MatchString(line) {
		return RemovedLine
	}
	result := line
	for _, v := range sensitive {
		result = strings.ReplaceAll(result, v, "[redacted]")
	}
	// After whole values are gone, any surviving start of one is a cut copy.
	for _, v := range sensitive {
		if len(v) >= 12 && strings.Contains(result, v[:12]) {
			return RemovedLine
		}
	}
	return result
}

// Redact applies RedactLine to every line of text.
func Redact(text string, env map[string]string) string {
	sensitive := sensitiveValues(env)
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = redactLine(l, sensitive)
	}
	return strings.Join(lines, "\n")
}

// StderrCollector keeps a bounded tail of a child's stderr, redacted line by
// line BEFORE anything is kept, so trimming to the tail can never cut a
// secret away from the text that identifies it. A line longer than the limit
// is dropped whole, including the rest of it that arrives later. It is an
// io.Writer.
type StderrCollector struct {
	mu        sync.Mutex
	sensitive []string
	limit     int
	kept      string
	partial   string
	skipping  bool
}

// NewStderrCollector returns a collector for env's secrets keeping at most
// limit bytes (8000 when limit <= 0).
func NewStderrCollector(env map[string]string, limit int) *StderrCollector {
	if limit <= 0 {
		limit = 8000
	}
	return &StderrCollector{sensitive: sensitiveValues(env), limit: limit}
}

func (c *StderrCollector) keep(line string) {
	c.kept += line + "\n"
	if len(c.kept) > c.limit {
		c.kept = c.kept[len(c.kept)-c.limit:]
	}
}

// Write implements io.Writer.
func (c *StderrCollector) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.partial += string(p)
	for {
		i := strings.IndexByte(c.partial, '\n')
		if i < 0 {
			break
		}
		line := c.partial[:i]
		c.partial = c.partial[i+1:]
		switch {
		case c.skipping:
			c.skipping = false
		case len(line) > c.limit:
			c.keep(RemovedLine)
		default:
			c.keep(redactLine(line, c.sensitive))
		}
	}
	if len(c.partial) > c.limit {
		if !c.skipping {
			c.keep(RemovedLine)
		}
		c.skipping = true
		c.partial = ""
	}
	return len(p), nil
}

// Text returns the kept tail (flushing a final unterminated line).
func (c *StderrCollector) Text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.partial != "" && !c.skipping {
		c.keep(redactLine(c.partial, c.sensitive))
	}
	c.partial = ""
	return strings.TrimSpace(c.kept)
}
