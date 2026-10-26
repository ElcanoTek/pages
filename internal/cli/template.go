// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/versions"
)

// `pages template` registers page templates from FILES, without an agent
// (the Node release's scripts/template.js):
//
//	pages template list
//	pages template show <name> [--revision N]
//	pages template register <file> [--name X] [--title T] [--description D] [--note N]
//	pages template sync [dir]        # default: the templates/ built into the binary
//
// Two ways a template reaches Pages, and this is the one that does not involve
// a model: point it at a file on disk. `register` takes any path, so a client
// bundle can carry its own templates and an operator registers them from the
// bundle checkout; `sync` walks a directory of them, which is what `pages
// update` runs against the templates/ shipped in this release.
//
// Idempotent by construction: registration dedupes on content_sha256 against
// the NEWEST revision, so re-running sync on unchanged files creates nothing.
// Edit a template and the next sync records revision N+1 — and still moves no
// deployed page, because every page stays pinned to the revision it was built
// from. That is what makes "git push + pages update" a safe way to ship a
// design fix.
//
// Requires DATABASE_URL (or PG*) matching the running server; on an installed
// box it is read from the service environment file (serviceenv.go).

const templateUsage = "usage: pages template list | show <name> [--revision N] | register <file> [--name X] | sync [dir]"

func init() {
	Register(&Command{
		Name:    "template",
		Summary: "list, register or sync page templates from files",
		Run: serviceVerb("template", func(args []string) int {
			return runTemplate(context.Background(), args, os.Stdout, os.Stderr, db.OpenFromEnv)
		}),
	})
}

// templateActor: the operator is the actor. These registrations (and
// refresh-schedule changes) are attributable to a person with shell access,
// not to an agent token.
func templateActor() versions.Actor {
	who := os.Getenv("SUDO_USER")
	if who == "" {
		who = os.Getenv("USER")
	}
	if who == "" {
		who = "operator"
	}
	return versions.Actor{Actor: "pages-cli:" + who, ActorType: audit.System}
}

// parseTemplateFlags splits --key value flags from positionals. A flag with
// no value would otherwise swallow the next flag as its argument.
func parseTemplateFlags(args []string) (map[string]string, []string, error) {
	flags := map[string]string{}
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
			continue
		}
		key := arg[2:]
		rest := args[i+1:]
		if len(rest) == 0 || strings.HasPrefix(rest[0], "--") {
			return nil, nil, fmt.Errorf("--%s needs a value", key)
		}
		flags[key] = rest[0]
		i++
	}
	return flags, positional, nil
}

func flagPtr(flags map[string]string, key string) *string {
	v, ok := flags[key]
	if !ok {
		return nil
	}
	return &v
}

func runTemplate(ctx context.Context, args []string, stdout, stderr io.Writer, open func() (*db.DB, error)) int {
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "list", "show", "register", "sync":
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, templateUsage)
		return 0
	default:
		fmt.Fprintln(stderr, templateUsage)
		return 2
	}
	flags, positional, err := parseTemplateFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	d, err := open()
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	defer d.Close()
	code, err := templateVerb(ctx, d, cmd, flags, positional, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	return code
}

var htmlExt = regexp.MustCompile(`(?i)\.html?$`)

func templateVerb(ctx context.Context, d *db.DB, cmd string, flags map[string]string, positional []string, stdout, stderr io.Writer) (int, error) {
	switch cmd {
	case "list":
		res, err := templates.List(ctx, d)
		if err != nil {
			return 1, err
		}
		if len(res.Templates) == 0 {
			fmt.Fprintln(stdout, "no templates registered")
			return 0, nil
		}
		for _, row := range res.Templates {
			rev := "null"
			if row.CurrentRevision != nil {
				rev = strconv.Itoa(*row.CurrentRevision)
			}
			title := ""
			if row.Title != "" {
				title = "  — " + row.Title
			}
			fmt.Fprintf(stdout, "%s  revision %s  %d page(s)%s\n", row.Name, rev, row.PageCount, title)
		}
		return 0, nil

	case "show":
		if len(positional) == 0 {
			return 1, errors.New("usage: pages template show <name> [--revision N]")
		}
		var revision *int
		if v, ok := flags["revision"]; ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return 1, fmt.Errorf("--revision must be an integer: %s", v)
			}
			revision = &n
		}
		res, err := templates.Get(ctx, d, positional[0], revision, false)
		if err != nil {
			return 1, err
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return 1, err
		}
		_, _ = stdout.Write(buf.Bytes())
		return 0, nil

	case "register":
		if len(positional) == 0 {
			return 1, errors.New("usage: pages template register <file> [--name X] [--title T] [--description D]")
		}
		htmlPath, err := filepath.Abs(positional[0])
		if err != nil {
			return 1, err
		}
		b, err := os.ReadFile(htmlPath) // #nosec G304 -- an operator-named file
		if errors.Is(err, fs.ErrNotExist) {
			return 1, errors.New("no such file: " + htmlPath)
		}
		if err != nil {
			return 1, err
		}
		// Default the name from the containing directory when the file follows
		// the templates/<name>/template.html convention, else from the stem.
		name := filepath.Base(htmlPath)
		if name == templates.TemplateFile {
			name = filepath.Base(filepath.Dir(htmlPath))
		} else {
			name = htmlExt.ReplaceAllString(name, "")
		}
		if v := flags["name"]; v != "" {
			name = v
		}
		ok, err := registerTemplateFile(ctx, d, templates.FileEntry{Name: name, HTML: string(b),
			Title: flagPtr(flags, "title"), Description: flagPtr(flags, "description"), Note: flagPtr(flags, "note")}, stdout, stderr)
		if err != nil {
			return 1, err
		}
		if !ok {
			return 1, nil
		}
		return 0, nil

	case "sync":
		var fsys fs.FS
		label := "the templates built into this release"
		if len(positional) > 0 {
			dir, err := filepath.Abs(positional[0])
			if err != nil {
				return 1, err
			}
			fsys, label = os.DirFS(dir), dir
		} else {
			fsys = pages.Templates()
		}
		root := label
		if len(positional) == 0 {
			root = "templates"
		}
		found, err := templates.Discover(fsys, root)
		if err != nil {
			return 1, err
		}
		if len(found) == 0 {
			fmt.Fprintf(stdout, "no templates found under %s\n", label)
			return 0, nil
		}
		failed := 0
		for _, entry := range found {
			ok, err := registerTemplateFile(ctx, d, entry, stdout, stderr)
			if err != nil {
				// One bad template must not stop the rest: a deploy that
				// half-registers is worse than one that reports exactly which
				// file is broken.
				failed++
				fmt.Fprintf(stderr, "%s: FAILED — %s\n", entry.Name, err.Error())
				continue
			}
			if !ok {
				failed++
			}
		}
		fmt.Fprintf(stdout, "synced %d/%d template(s) from %s\n", len(found)-failed, len(found), label)
		if failed > 0 {
			return 1, nil
		}
		return 0, nil
	}
	return 2, nil
}

// registerTemplateFile registers one file and reports it; ok is false when
// preflight found errors.
func registerTemplateFile(ctx context.Context, d *db.DB, e templates.FileEntry, stdout, stderr io.Writer) (bool, error) {
	if e.ReadErr != nil {
		return false, e.ReadErr
	}
	res, err := templates.Register(ctx, d, templates.RegisterInput{Name: e.Name, HTML: e.HTML, Title: e.Title,
		Description: e.Description, Note: e.Note, Source: templates.SourceCLI}, templateActor())
	if err != nil {
		return false, err
	}
	state := "new revision"
	switch {
	case res.Deduped:
		state = "unchanged"
	case res.Created:
		state = "created"
	}
	fmt.Fprintf(stdout, "%s: %s (revision %d, %s)\n", res.Template.Name, state, res.Revision.Revision, res.Revision.ContentSHA256[:12])
	// Preflight errors in a template are inherited by every page built from
	// it, so they are reported loudly here rather than left in a field nobody
	// reads.
	if !res.Preflight.OK {
		fmt.Fprintf(stderr, "  ⚠ preflight found %d error(s) — every page built from this inherits them:\n", len(res.Preflight.Errors))
		for i, f := range res.Preflight.Errors {
			if i == 10 {
				break
			}
			fmt.Fprintf(stderr, "      %s: %s\n", f.Code, f.Message)
		}
		return false, nil
	}
	return true, nil
}
