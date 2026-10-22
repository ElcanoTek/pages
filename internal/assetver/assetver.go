// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package assetver versions the URLs of the embedded static assets, so they
// can be cached for a year without a release ever serving stale bytes.
//
// The version is one content hash over the whole embedded public/ tree,
// computed once, and it is a PATH segment rather than a query string:
// /shell-assets/_v/<version>/flag/fonts/fonts.css. A path segment is what
// lets the relative url(...) references inside vendored Flag CSS (the font
// files) and the sprite path primitives.js derives from its own URL inherit
// the version without editing a vendored file.
//
// A request whose version is the current one is answered immutable. Any other
// version, and every unversioned path, is answered with the current bytes and
// a revalidating Cache-Control: an open tab from the previous release still
// renders, and the stable unversioned URLs that client pages hard-code
// (/assets/flag/..., the Flag Motion kit) keep revalidating.
package assetver

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"io/fs"
	"strings"
	"sync"

	pages "github.com/ElcanoTek/pages"
)

// Segment introduces the version in a URL path.
const Segment = "_v"

// Immutable is the Cache-Control of a current-version asset.
const Immutable = "public, max-age=31536000, immutable"

// Revalidate is the Cache-Control of an unversioned or stale-version asset:
// no-cache, so it may be stored but is revalidated (ETag → 304) before every
// use and is never served stale.
const Revalidate = "public, no-cache"

var version = sync.OnceValue(func() string { return Of(pages.Public()) })

// Version is the current asset version: a hash of the embedded public/ tree.
func Version() string { return version() }

// Of hashes every file under fsys into a 12-hex-digit version. Each file is
// one unambiguous record — len(path), path, len(body), body, with the lengths
// as big-endian uint64 — in WalkDir's lexical order, so no two different
// trees can produce the same byte stream (a body cannot be re-read as the
// next file's path).
func Of(fsys fs.FS) string {
	h := sha256.New()
	var n [8]byte
	writeLen := func(l int) {
		binary.BigEndian.PutUint64(n[:], uint64(l)) //nolint:gosec // a length is never negative
		_, _ = h.Write(n[:])
	}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		writeLen(len(p))
		_, _ = io.WriteString(h, p)
		writeLen(len(body))
		_, _ = h.Write(body)
		return nil
	})
	if err != nil {
		panic("assetver: hashing the embedded public tree: " + err.Error())
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Shell is the versioned base of the dashboard's /shell-assets mount.
func Shell() string { return "/shell-assets/" + Segment + "/" + Version() }

// ShellFlag is the versioned base of the vendored Flag files on the dashboard.
func ShellFlag() string { return Shell() + "/flag" }

// Content is the versioned base of the content host's /assets mount.
func Content() string { return "/assets/" + Segment + "/" + Version() }

// ContentFlag is the versioned base of the vendored Flag files on the content
// host.
func ContentFlag() string { return Content() + "/flag" }

// Strip takes a mount-relative path ("/_v/<version>/flag/x.css" under
// /assets) and returns it without the version segment. versioned reports
// whether there was one; current whether it names this build's version.
func Strip(rel string) (rest string, versioned, current bool) {
	tail, ok := strings.CutPrefix(rel, "/"+Segment+"/")
	if !ok {
		return rel, false, false
	}
	v, after, found := strings.Cut(tail, "/")
	if !found {
		after = ""
	}
	return "/" + after, true, v == Version()
}
