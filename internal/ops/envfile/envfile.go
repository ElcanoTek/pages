// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package envfile reads and writes the shell-style KEY="value" files Pages is
// configured through (/etc/default/pages, /etc/default/pages-install,
// /etc/os-release) WITHOUT evaluating them.
//
// The files hold every credential, and a root-run doctor/backup/bootstrap
// exists precisely for boxes whose file permissions have gone wrong, so the
// tooling never sources them: a tampered file must not be able to run code as
// root. Parse decodes the quoting systemd's EnvironmentFile loader and a POSIX
// shell agree on (double quotes with \" \\ \$ \` escapes, single quotes,
// backslash escapes outside quotes); anything else on a line — a command, a
// comment — is ignored.
package envfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/ElcanoTek/pages/internal/ops/runner"
)

// Entry is one assignment, in file order.
type Entry struct {
	Key   string
	Value string
}

// File is a parsed environment file.
type File struct {
	Entries []Entry
}

// Get returns the last assignment of key (last wins, as when sourced) and
// whether there was one.
func (f *File) Get(key string) (string, bool) {
	if f == nil {
		return "", false
	}
	for i := len(f.Entries) - 1; i >= 0; i-- {
		if f.Entries[i].Key == key {
			return f.Entries[i].Value, true
		}
	}
	return "", false
}

// Value is Get without the presence flag.
func (f *File) Value(key string) string {
	v, _ := f.Get(key)
	return v
}

// Map returns the effective assignments (last wins).
func (f *File) Map() map[string]string {
	m := map[string]string{}
	if f == nil {
		return m
	}
	for _, e := range f.Entries {
		m[e.Key] = e.Value
	}
	return m
}

// Read parses the file at path.
func Read(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(b)), nil
}

var assignment = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=`)

// Parse decodes text. Records are split the way write-env splits them, so a
// quoted value may span lines.
func Parse(text string) *File {
	f := &File{}
	for _, rec := range records(text) {
		m := assignment.FindStringSubmatchIndex(rec)
		if m == nil {
			continue
		}
		key := rec[m[2]:m[3]]
		f.Entries = append(f.Entries, Entry{Key: key, Value: word(rec[m[1]:])})
	}
	return f
}

// word decodes one shell word starting at s (the text after `KEY=`): quoted
// and unquoted segments concatenate until unquoted whitespace, a comment or
// the end of the record.
func word(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					switch s[i+1] {
					case '"', '\\', '$', '`':
						b.WriteByte(s[i+1])
						i += 2
						continue
					case '\n':
						i += 2
						continue
					}
				}
				b.WriteByte(s[i])
				i++
			}
			i++ // closing quote
		case c == '\'':
			i++
			for i < len(s) && s[i] != '\'' {
				b.WriteByte(s[i])
				i++
			}
			i++
		case c == '\\':
			if i+1 < len(s) {
				if s[i+1] != '\n' {
					b.WriteByte(s[i+1])
				}
				i += 2
			} else {
				i++
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ';':
			return b.String()
		case c == '#' && b.Len() == 0:
			return b.String()
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// records splits text into logical records, keeping each record's bytes
// (including its newline) so a merge can retain unknown records verbatim. A
// newline inside quotes does not end a record; a '#' outside quotes starts a
// comment that runs to the end of the line.
func records(text string) []string {
	var out []string
	start := 0
	var quoted byte
	escaped, comment := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if escaped {
			escaped = false
			continue
		}
		if !comment && c == '\\' && quoted != '\'' {
			escaped = true
			continue
		}
		if !comment && (c == '\'' || c == '"') {
			switch quoted {
			case 0:
				quoted = c
			case c:
				quoted = 0
			}
		}
		if quoted == 0 && c == '#' {
			comment = true
		}
		if c == '\n' && quoted == 0 {
			out = append(out, text[start:i+1])
			start = i + 1
			comment = false
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// Quote renders value as a double-quoted shell word that both bash and
// systemd decode back to exactly value. Multi-line values are refused: the
// managed keys are single-line by contract.
func Quote(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("bootstrap-managed environment values must be single-line text")
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\', '"', '$', '`':
			b.WriteByte('\\')
		}
		b.WriteByte(value[i])
	}
	b.WriteByte('"')
	return b.String(), nil
}

var validKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Update is one managed assignment for Merge, applied in order.
type Update struct {
	Key   string
	Value string
}

// Merge returns original with each update's key rewritten in place (its first
// record) or appended, keeping every other record — comments, operator
// settings, quoted multi-line values — byte for byte. The result is checked
// with `bash -n` through r; an invalid result is an error and nothing is
// returned for writing.
func Merge(ctx context.Context, r runner.Runner, original string, updates []Update) (string, error) {
	pending := map[string]int{}
	for i, u := range updates {
		if !validKey.MatchString(u.Key) {
			return "", errors.New("invalid environment key")
		}
		pending[u.Key] = i
	}
	quoted := map[string]string{}
	for _, u := range updates {
		q, err := Quote(u.Value)
		if err != nil {
			return "", err
		}
		quoted[u.Key] = q
	}
	var b strings.Builder
	for _, rec := range records(original) {
		m := assignment.FindStringSubmatch(rec)
		if m != nil {
			if q, ok := quoted[m[1]]; ok {
				delete(pending, m[1])
				b.WriteString(m[1] + "=" + q + "\n")
				continue
			}
		}
		b.WriteString(rec)
	}
	result := b.String()
	if result != "" && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	for _, u := range updates {
		if _, ok := pending[u.Key]; ok {
			result += u.Key + "=" + quoted[u.Key] + "\n"
			delete(pending, u.Key)
		}
	}
	if r != nil {
		if _, err := r.Run(ctx, runner.Cmd{Name: "bash", Args: []string{"-n"}, Stdin: strings.NewReader(result)}); err != nil {
			return "", errors.New("generated environment is not valid shell syntax; original file retained")
		}
	}
	return result, nil
}

// Write merges updates into filename atomically. The candidate is fully
// validated before anything changes; the replaced file is kept as
// filename.previous with its ownership and mode; a new file is created 0640.
func Write(ctx context.Context, r runner.Runner, filename string, updates []Update) error {
	var original []byte
	st, err := os.Stat(filename)
	exists := err == nil
	if exists {
		if original, err = os.ReadFile(filename); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	next, err := Merge(ctx, r, string(original), updates) // finish validation before any mutation
	if err != nil {
		return err
	}
	var pending []string
	defer func() {
		for _, p := range pending {
			_ = os.Remove(p)
		}
	}()
	stage := func(target string, value []byte) (string, error) {
		tmp := fmt.Sprintf("%s.%d.tmp", target, os.Getpid())
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", err
		}
		pending = append(pending, tmp)
		defer f.Close()
		if _, err := f.Write(value); err != nil {
			return "", err
		}
		mode := os.FileMode(0o640)
		if exists {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok {
				if err := f.Chown(int(sys.Uid), int(sys.Gid)); err != nil {
					return "", err
				}
			}
			mode = st.Mode().Perm()
		}
		if err := f.Chmod(mode); err != nil {
			return "", err
		}
		if err := f.Sync(); err != nil {
			return "", err
		}
		return tmp, f.Close()
	}
	candidate, err := stage(filename, []byte(next))
	if err != nil {
		return err
	}
	if exists {
		prev, err := stage(filename+".previous", original)
		if err != nil {
			return err
		}
		if err := os.Rename(prev, filename+".previous"); err != nil {
			return err
		}
	}
	if err := os.Rename(candidate, filename); err != nil {
		return err
	}
	return syncDir(filepath.Dir(filename))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// redactKey matches key names whose whole value is a secret.
var redactKey = regexp.MustCompile(`TOKEN|KEY|SECRET|PASSWORD|PEPPER`)

// urlUserinfo is POSIX leftmost-longest, as awk's match(): it runs through the
// LAST raw '@' in the userinfo, and accepts an empty username
// (redis://:password@host).
var urlUserinfo = regexp.MustCompilePOSIX(`//[^/[:space:]]*:[^/[:space:]]*@`)

// Redact renders an environment file for `pages env`: comments and lines
// without '=' are dropped, whole values of secret-named keys (TOKEN, KEY,
// SECRET, PASSWORD, PEPPER, and DEV_ADMIN_COOKIE) become [REDACTED], and the
// userinfo password inside any URL-shaped value is replaced, so DATABASE_URL
// prints as postgres://pages:[REDACTED]@host/db. Line-based by design, exactly
// as the awk program it replaces.
func Redact(text string) string {
	var out bytes.Buffer
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		field := line[:eq]
		key := strings.ToUpper(strings.TrimSpace(field))
		if redactKey.MatchString(key) || key == "DEV_ADMIN_COOKIE" {
			out.WriteString(field + "=[REDACTED]\n")
			continue
		}
		if loc := urlUserinfo.FindStringIndex(line); loc != nil {
			head, auth, tail := line[:loc[0]], line[loc[0]:loc[1]], line[loc[1]:]
			auth = strings.TrimPrefix(auth, "//")
			if i := strings.IndexByte(auth, ':'); i >= 0 {
				auth = auth[:i]
			}
			out.WriteString(head + "//" + auth + ":[REDACTED]@" + tail + "\n")
			continue
		}
		out.WriteString(line + "\n")
	}
	return out.String()
}
