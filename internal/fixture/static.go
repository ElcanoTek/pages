// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/assetver"
	"github.com/ElcanoTek/pages/internal/contentview"
	"github.com/ElcanoTek/pages/internal/httpx"
)

// assetTypes is what express.static answers for the files under
// public/assets.
var assetTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".txt":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
	".html":  "text/html; charset=utf-8",
}

// serveAssets is express.static(dir, { index: false }) over fsys, mounted at
// prefix, with setHeaders applied to every file it serves. It returns true to
// fall through: wrong method, missing file, a directory, a dotfile or a
// traversal attempt. Ranges and conditional requests are http.ServeContent's.
//
// contentview serves the content host's /assets the same way; its helper is
// unexported, so the fixture keeps this small copy.
func serveAssets(fsys fs.FS, setHeaders func(http.Header)) func(w http.ResponseWriter, r *req) bool {
	return func(w http.ResponseWriter, r *req) bool {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return true
		}
		// The versioned form (/assets/_v/<version>/...) is the same file,
		// immutable when the version is current, as on the content host.
		rest, _, current := assetver.Strip(r.rest)
		decoded, err := httpx.DecodeURIComponent(rest)
		if err != nil || strings.ContainsRune(decoded, 0) || strings.HasSuffix(decoded, "/") {
			return true
		}
		for _, part := range strings.Split(strings.Trim(decoded, "/"), "/") {
			if part == ".." || strings.HasPrefix(part, ".") {
				return true
			}
		}
		name := strings.TrimPrefix(path.Clean("/"+decoded), "/")
		st, err := fs.Stat(fsys, name)
		if err != nil || st.IsDir() {
			return true
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return true
		}
		h := w.Header()
		h.Set("Accept-Ranges", "bytes")
		h.Set("Cache-Control", assetver.Revalidate)
		h.Set("ETag", httpx.ETag(body))
		ct, ok := assetTypes[strings.ToLower(path.Ext(name))]
		if !ok {
			ct = "application/octet-stream"
		}
		h.Set("Content-Type", ct)
		if setHeaders != nil {
			setHeaders(h)
		}
		if current {
			h.Set("Cache-Control", assetver.Immutable)
		}
		http.ServeContent(w, r.Request, name, time.Time{}, bytes.NewReader(body))
		return false
	}
}

// assetHeaders are the headers the content host puts on /assets, so the gate
// pages are rendered by a real browser against a real asset CSP. With a bare
// mount, a CSP that blocked the Flag stylesheet or the @font-face would have
// passed every test in this suite.
func assetHeaders(h http.Header) {
	h.Set("Content-Security-Policy", contentview.AssetCSP)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Robots-Tag", "noindex, noarchive")
}

func staticRoutes() []route {
	pub := pages.Public()
	flag, _ := fs.Sub(pub, "assets/flag")
	assets, _ := fs.Sub(pub, "assets")
	// /shell-assets and /shell-assets/flag are the dashboard host's own
	// mounts, served exactly as the server serves them.
	shellAssets := func(w http.ResponseWriter, r *req) bool {
		fell := false
		contentview.ShellAssets(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fell = true })).ServeHTTP(w, r.Request)
		return fell
	}
	return []route{
		{pattern: "/shell-assets", mount: true, mw: shellAssets},
		// The content host serves the same vendored files from its own mount,
		// and the standalone chrome links them from there.
		{pattern: "/assets/flag", mount: true, mw: serveAssets(flag, nil)},
		{pattern: "/assets", mount: true, mw: serveAssets(assets, assetHeaders)},
	}
}
