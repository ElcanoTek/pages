// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package contentview

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/shell"
)

// AssetCSP is the CSP on a /assets response. The floor already set
// Referrer-Policy/nosniff/X-Robots-Tag; only the CSP needs widening, and only
// for a DIRECT navigation to an asset (a CSP on a subresource response does
// not govern it). That still matters because `/assets/flag/logos/*.svg` are
// documents, and SVG can carry script — `default-src 'none'` keeps a navigated
// one inert while the stylesheet and its @font-face resolve. `sandbox` is
// deliberately absent: it binds only a navigation, and on a subresource it is
// pure noise.
const AssetCSP = "default-src 'none'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; base-uri 'none'; form-action 'none'"

// setAssetHeaders is the content host's /assets header hook.
//
// Cache-Control is set EXPLICITLY: the floor's `no-store` would otherwise make
// every dashboard load re-download the font files and the token sheet.
// `max-age=0` is cacheable-but-revalidate — browsers keep the bytes and get a
// 304 — which is what these stable, overwritten-in-place paths want. Not
// `immutable`: re-vendoring Flag replaces them under the same URL, so
// immutable would pin every client to whatever it first loaded.
func setAssetHeaders(h http.Header) {
	h.Set("Content-Security-Policy", AssetCSP)
	h.Set("Cache-Control", "public, max-age=0")
}

// setDefaultCache is express.static's own default: revalidating, unless the
// zone already chose a Cache-Control.
func setDefaultCache(h http.Header) {
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "public, max-age=0")
	}
}

// mountRel is Express's app.use(prefix) match: case-insensitive, on a
// segment boundary. rel is what the mounted middleware sees ("" for the bare
// prefix).
func mountRel(p, prefix string) (string, bool) {
	if len(p) < len(prefix) || !strings.EqualFold(p[:len(prefix)], prefix) {
		return "", false
	}
	rest := p[len(prefix):]
	if rest != "" && rest[0] != '/' {
		return "", false
	}
	return rest, true
}

// contentTypes is what `send` (mime-types) answers for the extensions under
// public/; anything else is application/octet-stream.
var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/vnd.microsoft.icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".txt":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".map":   "application/json; charset=utf-8",
}

type staticFile struct {
	body []byte
	etag string
}

var fileCache sync.Map // fs.FS+path → *staticFile

type cacheKey struct {
	fsys fs.FS
	name string
}

func loadFile(fsys fs.FS, name string) (*staticFile, bool, bool) {
	st, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, false, false
	}
	if st.IsDir() {
		return nil, true, true
	}
	key := cacheKey{fsys, name}
	if f, ok := fileCache.Load(key); ok {
		return f.(*staticFile), true, false
	}
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, false, false
	}
	f := &staticFile{body: b, etag: httpx.ETag(b)}
	fileCache.Store(key, f)
	return f, true, false
}

func etagMatches(list, etag string) bool {
	for _, m := range strings.Split(list, ",") {
		m = strings.TrimSpace(m)
		if m == etag || m == "W/"+etag || "W/"+m == etag {
			return true
		}
	}
	return false
}

var bytesRangeRE = regexp.MustCompile(`^ *bytes=`)

type byteRange struct{ start, end int64 }

// parseRange is range-parser with combine: nil, false when malformed (the
// header is ignored); an empty slice when nothing is satisfiable.
//
// One deliberate difference from range-parser 1.2: a suffix range longer
// than the representation ("bytes=-1000" on a 500-byte file) selects the
// whole file, as RFC 9110 §14.1.2 requires ("If the selected representation
// is shorter than the specified suffix-length, the entire representation is
// used"); range-parser computed a negative start and answered 416. A
// zero-length suffix ("bytes=-0") stays unsatisfiable.
func parseRange(size int64, header string) ([]byteRange, bool) {
	eq := strings.IndexByte(header, '=')
	if eq < 0 {
		return nil, false
	}
	var out []byteRange
	for _, spec := range strings.Split(header[eq+1:], ",") {
		a, b, ok := strings.Cut(strings.TrimSpace(spec), "-")
		if !ok {
			continue
		}
		start, errA := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
		end, errB := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
		switch {
		case strings.TrimSpace(a) == "" && errB == nil:
			suffix := end
			start, end, errA = size-suffix, size-1, nil
			if suffix > 0 && start < 0 {
				start = 0 // clamp an oversized suffix to the whole file
			}
		case errA == nil && strings.TrimSpace(b) == "":
			end, errB = size-1, nil
		}
		if end > size-1 {
			end = size - 1
		}
		if errA != nil || errB != nil || start > end || start < 0 {
			continue
		}
		out = append(out, byteRange{start, end})
	}
	if len(out) == 0 {
		return []byteRange{}, true
	}
	// combine overlapping / adjacent ranges
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].start < out[j-1].start; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	merged := []byteRange{out[0]}
	for _, r := range out[1:] {
		last := &merged[len(merged)-1]
		if r.start > last.end+1 {
			merged = append(merged, r)
		} else if r.end > last.end {
			last.end = r.end
		}
	}
	return merged, true
}

const redirectDoc = "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Redirecting</title>\n</head>\n<body>\n<pre>Redirecting to %s</pre>\n</body>\n</html>\n"

var leadingSlashes = regexp.MustCompile(`^/+`)

// serveStatic is express.static(dir, {index: false, fallthrough: true}) over
// fsys for the mount-relative raw path rel. handled is false when the request
// falls through to the next route (wrong method, missing file, dotfile, a
// directory with a trailing slash). A 412 or 416 comes back as a
// *statusError for the error handler, as `send` forwarded them.
func serveStatic(w http.ResponseWriter, r *http.Request, fsys fs.FS, rel string, setHeaders func(http.Header)) (bool, error) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false, nil
	}
	// A malformed escape or a NUL is a 400 in send, which falls through.
	decoded, decErr := httpx.DecodeURIComponent(rel)
	if decErr != nil || strings.ContainsRune(decoded, 0) {
		return false, nil //nolint:nilerr // fallthrough, as serve-static does for a 4xx
	}
	trailing := decoded == "" || strings.HasSuffix(decoded, "/")
	parts := strings.Split(strings.Trim(decoded, "/"), "/")
	for _, part := range parts {
		if part == ".." || (strings.HasPrefix(part, ".") && part != "") {
			return false, nil // traversal (403) and dotfiles (ignored) both fall through
		}
	}
	name := path.Clean("/" + strings.Trim(decoded, "/"))[1:]
	if name == "" {
		name = "."
	}
	f, ok, isDir := loadFile(fsys, name)
	if !ok {
		return false, nil
	}
	if isDir {
		if trailing && decoded != "" {
			return false, nil
		}
		loc := leadingSlashes.ReplaceAllString(r.URL.EscapedPath()+"/", "/")
		if r.URL.RawQuery != "" {
			loc += "?" + r.URL.RawQuery
		}
		loc = httpx.EncodeURL(loc)
		doc := strings.Replace(redirectDoc, "%s", esc(loc), 1)
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Length", strconv.Itoa(len(doc)))
		h.Set("Content-Security-Policy", "default-src 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Location", loc)
		w.WriteHeader(http.StatusMovedPermanently)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(doc)) //nolint:gosec // loc is URL-encoded and HTML-escaped
		}
		return true, nil
	}
	if strings.HasSuffix(decoded, "/") {
		return false, nil // a file addressed as a directory: ENOTDIR → 404
	}
	size := int64(len(f.body))

	// Preconditions (send's isPreconditionFailure). Pages serves no
	// Last-Modified, so a valid If-Unmodified-Since cannot be satisfied.
	if im := httpx.Header(r, "If-Match"); im != "" {
		if im != "*" && !etagMatches(im, f.etag) {
			return true, &statusError{status: http.StatusPreconditionFailed, msg: "precondition failed"}
		}
	} else if ius := r.Header.Get("If-Unmodified-Since"); ius != "" {
		if _, err := http.ParseTime(ius); err == nil {
			return true, &statusError{status: http.StatusPreconditionFailed, msg: "precondition failed"}
		}
	}

	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("ETag", f.etag)
	setHeaders(h)
	if h.Get("Content-Type") == "" {
		ct, ok := contentTypes[strings.ToLower(path.Ext(name))]
		if !ok {
			ct = "application/octet-stream"
		}
		h.Set("Content-Type", ct)
	}

	// Fresh → 304.
	if inm := httpx.Header(r, "If-None-Match"); inm != "" && !strings.Contains(strings.ToLower(httpx.Header(r, "Cache-Control")), "no-cache") {
		if inm == "*" || etagMatches(inm, f.etag) {
			for _, k := range []string{"Content-Type", "Content-Length", "Content-Encoding", "Content-Language", "Content-Range"} {
				h.Del(k)
			}
			w.WriteHeader(http.StatusNotModified)
			return true, nil
		}
	}

	status, body := http.StatusOK, f.body
	if rh := r.Header.Get("Range"); rh != "" && bytesRangeRE.MatchString(rh) {
		// If-Range: an ETag must be contained; a date can never match (no
		// Last-Modified), so the full body is sent.
		ifRange := r.Header.Get("If-Range")
		fresh := ifRange == "" || (strings.Contains(ifRange, `"`) && strings.Contains(ifRange, f.etag))
		if fresh {
			ranges, ok := parseRange(size, rh)
			switch {
			case ok && len(ranges) == 0:
				// The Content-Range send attached is kept (RFC 9110 §15.5.17).
				h.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
				return true, &statusError{status: http.StatusRequestedRangeNotSatisfiable, msg: "range not satisfiable"}
			case ok && len(ranges) == 1:
				rg := ranges[0]
				status = http.StatusPartialContent
				h.Set("Content-Range", "bytes "+strconv.FormatInt(rg.start, 10)+"-"+strconv.FormatInt(rg.end, 10)+"/"+strconv.FormatInt(size, 10))
				body = f.body[rg.start : rg.end+1]
			}
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body) //nolint:gosec // an embedded first-party file, served with nosniff and its own CSP
	}
	return true, nil
}

// ShellAssets serves the dashboard host's /shell-assets (the trusted /view and
// /admin UI's CSS/JS) and, behind it, /shell-assets/flag from the vendored
// Flag tree — the same files the content host serves at /assets/flag, in the
// other zone. Requests it does not serve go to next. Cache-Control is the
// revalidating default unless the zone already set one; a forwarded 412/416
// answers with the dashboard's branded request-error page.
func ShellAssets(next http.Handler) http.Handler {
	pub := pages.Public()
	shellFS, _ := fs.Sub(pub, "shell-assets")
	flagFS, _ := fs.Sub(pub, "assets/flag")
	mounts := []struct {
		prefix string
		fsys   fs.FS
	}{{"/shell-assets", shellFS}, {"/shell-assets/flag", flagFS}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		for _, m := range mounts {
			rel, ok := mountRel(p, m.prefix)
			if !ok {
				continue
			}
			handled, err := serveStatic(w, r, m.fsys, rel, setDefaultCache)
			if !handled {
				continue
			}
			var se *statusError
			if errors.As(err, &se) {
				httpx.SendHTML(w, r, se.status, shell.RequestError(se.status))
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}
