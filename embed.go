// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package pages embeds the repository's static trees — migrations, the admin
// shell's browser assets, the vendored Flag design system, the shipped page
// templates and the agent-facing docs — into the one pages binary, so a
// release is a single file and the running server can never read a tree that
// disagrees with the code that serves it.
package pages

import (
	"embed"
	"io/fs"
)

//go:embed migrations
var migrations embed.FS

//go:embed all:public
var public embed.FS

//go:embed templates
var templates embed.FS

//go:embed docs/*.md
var docs embed.FS

//go:embed deploy/pages.service deploy/pages.caddy deploy/motd
var deploy embed.FS

func sub(f embed.FS, dir string) fs.FS {
	s, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return s
}

// Migrations is migrations/: the *.sql files, applied in filename order, and
// compatibility.json, which says which of them an in-place update may apply.
func Migrations() fs.FS { return sub(migrations, "migrations") }

// Public is public/: shell-assets/ (the admin UI) and assets/ (Flag).
func Public() fs.FS { return sub(public, "public") }

// Templates is templates/: the page templates `pages template sync` registers.
func Templates() fs.FS { return sub(templates, "templates") }

// Docs is docs/*.md, the source of the get_guide tool's topics.
func Docs() fs.FS { return sub(docs, "docs") }

// Deploy is deploy/: the systemd unit, Caddy site blocks and login banner the
// operator CLI renders and installs (pages bootstrap / pages update), so a
// release always installs the control files it was built with.
func Deploy() fs.FS { return sub(deploy, "deploy") }
