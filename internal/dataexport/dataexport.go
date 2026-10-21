// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package dataexport mints and serves short-lived, read-only URLs for one
// published version's managed-data schema and envelope, so a refresh run can
// put them in a workspace file instead of its context (lib/data-export.js).
//
// WHY. get_page_data is the first and most repeated call of every refresh, and
// the full answer (schema, profile, 150–400 KB of rows) is re-sent on every
// later model step. A client that fetches URLs host-side — Fleet's
// download_url, which takes a URL and nothing else, no headers — can land these
// bytes on disk where run_python reads them, and the model only ever handles a
// link. That client constraint is why the credential is IN the URL: an
// Authorization-header-only endpoint would be unreachable from the one seam
// that keeps bytes out of context, and a sandbox curl needs network egress most
// refresh runs do not have.
//
// So the URL is a bearer capability, like a signed object-store link, and it is
// bounded to make a leak uninteresting:
//
//   - read-only — GET/HEAD of three derived JSON documents; nothing else.
//   - one version — bound to the page id and the immutable version id that was
//     live when get_page_data minted it. page_versions content is append-only,
//     so the bytes behind a URL never change.
//   - minutes — ExportTTLSeconds, then refused.
//   - re-authorized on every fetch — the minting agent token must still be
//     unrevoked and still allowed to call get_page_data on this page (a
//     data_update grant that no longer holds the slug ends its URLs too). A URL
//     can never reach more than the token that minted it could read at that
//     moment.
//   - its own key — HMAC under a key DERIVED from RAW_TOKEN_SECRET for this
//     audience alone, so no /raw view/session/edit token verifies here and no
//     export token verifies at /raw.
//   - dashboard host only — mounted on the trusted host beside /api/v1 and
//     /upload (behind the API rate limiter), reads no cookie, and serves
//     attachment JSON with nosniff, never HTML. No content-host route exists.
package dataexport

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/nodebuf"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/tokens"
)

// Audience is the aud claim of every export token.
const Audience = "pages-data-export"

// ExportTTLSeconds is long enough for a run to fetch three files one model
// step apart; short enough that a link copied out of a transcript is dead
// before anyone reads it.
const ExportTTLSeconds = 10 * 60

// Parts are the documents an export URL set serves, in URL order.
var Parts = []string{"schema", "data", "envelope"}

// key is HMAC-SHA256(RAW_TOKEN_SECRET, "pages-data-export/v1"), or nil when no
// secret is configured. Read at call time (the JS read it at load) so tests
// can set the secret.
func key() []byte {
	secret := env.String("RAW_TOKEN_SECRET", "")
	if secret == "" {
		return nil
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(Audience + "/v1"))
	return m.Sum(nil)
}

func sign(k []byte, body string) []byte {
	m := hmac.New(sha256.New, k)
	m.Write([]byte(body))
	return m.Sum(nil)
}

var positiveID = regexp.MustCompile(`^[1-9][0-9]*$`)

// MintParams binds an export token to a page, one of its versions, and the
// agent token that asked.
type MintParams struct {
	PageID    jsx.ID
	VersionID jsx.ID
	TokenID   jsx.ID
}

// Minted is a new export token and when it stops verifying.
type Minted struct {
	Token string
	// ExpiresAt is the expiry as Date#toISOString writes it.
	ExpiresAt string
}

// Mint signs an export token valid for ttlSeconds (ExportTTLSeconds in
// production; a negative value mints an already-expired token for tests).
// Without RAW_TOKEN_SECRET it is a 409 data_export_unavailable.
func Mint(p MintParams, ttlSeconds int) (Minted, error) {
	k := key()
	if k == nil {
		return Minted{}, apierror.Conflict("data export URLs are unavailable: this server has no RAW_TOKEN_SECRET", "data_export_unavailable", nil)
	}
	for _, f := range []struct {
		name string
		id   jsx.ID
	}{{"pageId", p.PageID}, {"versionId", p.VersionID}, {"tokenId", p.TokenID}} {
		if f.id <= 0 {
			return Minted{}, fmt.Errorf("data export %s is missing", f.name)
		}
	}
	exp := time.Now().Unix() + int64(ttlSeconds)
	claims := `{"aud":` + jsonv.Quote(Audience) +
		`,"pid":"` + p.PageID.String() + `","vid":"` + p.VersionID.String() + `","tid":"` + p.TokenID.String() +
		`","exp":` + strconv.FormatInt(exp, 10) + `}`
	body := base64.RawURLEncoding.EncodeToString([]byte(claims))
	return Minted{
		Token:     body + "." + base64.RawURLEncoding.EncodeToString(sign(k, body)),
		ExpiresAt: jsx.FormatTime(time.Unix(exp, 0)),
	}, nil
}

// Claims is a verified export token's payload. The ids are decimal strings,
// as minted.
type Claims struct {
	PageID    string
	VersionID string
	TokenID   string
	Exp       float64
}

// Verify returns the claims of a well-signed, unexpired export token, or nil.
// Constant-time signature check, then audience and expiry. It says nothing
// about whether the minting token is still good; that needs the database and
// happens per request.
func Verify(token string) *Claims {
	k := key()
	if k == nil {
		return nil
	}
	dot := strings.IndexByte(token, '.')
	if dot < 1 || dot == len(token)-1 || strings.IndexByte(token[dot+1:], '.') != -1 {
		return nil
	}
	body := token[:dot]
	// Node's base64url decoding is lenient; the HMAC comparison is what
	// rejects a mangled signature.
	if !hmac.Equal(nodebuf.Base64Decode(token[dot+1:]), sign(k, body)) {
		return nil
	}
	v, err := jsonv.Parse([]byte(jsonv.DecodeUTF8(nodebuf.Base64Decode(body))))
	if err != nil {
		return nil
	}
	obj, ok := v.(*jsonv.Object)
	if !ok || obj.Value("aud") != Audience {
		return nil
	}
	exp, ok := obj.Value("exp").(float64)
	if !ok || exp <= float64(time.Now().Unix()) {
		return nil
	}
	ids := make([]string, 3)
	for i, name := range []string{"pid", "vid", "tid"} {
		raw, present := obj.Get(name)
		if !present {
			return nil
		}
		ids[i] = jsonv.ToString(raw) // String(claims[key])
		if !positiveID.MatchString(ids[i]) {
			return nil
		}
	}
	return &Claims{PageID: ids[0], VersionID: ids[1], TokenID: ids[2], Exp: exp}
}

// URLs are the three download links for an export token, in the key order
// get_page_data reports them.
type URLs struct {
	SchemaURL   string `json:"schema_url"`
	DataURL     string `json:"data_url"`
	EnvelopeURL string `json:"envelope_url"`
}

// ExportURLs builds the dashboard-host URLs for token.
func ExportURLs(token string) URLs {
	base := env.DashboardOrigin() + "/export/" + token
	return URLs{
		SchemaURL:   base + "/schema.json",
		DataURL:     base + "/data.json",
		EnvelopeURL: base + "/envelope.json",
	}
}

// partBody is the bytes each part serves. schema and data are the exact
// canonical JSON the semantic hashes are computed over, so sha256(file) ===
// schema_sha256 / data_sha256 and a caller can prove it downloaded the version
// it was told about.
func partBody(m *pagedata.Managed, part string) (string, error) {
	switch part {
	case "schema":
		return pagedata.CanonicalJSON(m.Schema)
	case "data":
		return pagedata.CanonicalJSON(m.Envelope.Value("data"))
	}
	return pagedata.CanonicalJSON(m.Envelope)
}

// invalid is one non-revealing answer for a forged, expired, cross-audience
// or revoked credential, like the upload-ticket endpoint.
func invalid() error {
	return apierror.Unauthorized("unknown or expired data export URL", "data_export_invalid", nil)
}

var unsafeSlugChars = regexp.MustCompile(`[^a-z0-9_-]+`)

func resolve(ctx context.Context, d *db.DB, token, part string) (body, filename string, err error) {
	claims := Verify(token)
	if claims == nil {
		return "", "", invalid()
	}
	agentToken, err := tokens.VerifyID(ctx, d, claims.TokenID)
	if err != nil {
		return "", "", err
	}
	if agentToken == nil {
		return "", "", invalid()
	}
	var slug, html string
	err = d.QueryRow(ctx,
		`SELECT p.slug, v.html
		   FROM page_versions v
		   JOIN pages p ON p.id = v.page_id
		  WHERE v.id = $1::bigint AND p.id = $2::bigint AND p.deleted_at IS NULL`,
		claims.VersionID, claims.PageID).Scan(&slug, &html)
	if db.IsNoRows(err) {
		return "", "", apierror.NotFound("page data not found", "page_not_found", nil)
	}
	if err != nil {
		return "", "", err
	}
	if tokens.AuthorizeMcpTool(tokens.AgentFor(agentToken), "get_page_data", map[string]any{"slug": slug}) != nil {
		return "", "", invalid()
	}
	managed, err := pagedata.ParseManagedHTML(html)
	if err != nil {
		return "", "", err
	}
	body, err = partBody(managed, part)
	if err != nil {
		return "", "", err
	}
	safeSlug := unsafeSlugChars.ReplaceAllString(slug, "-")
	return body, safeSlug + ".v" + claims.VersionID + "." + part + ".json", nil
}

// Handler serves GET/HEAD /<token>/<file> relative to its mount point (the
// server strips "/export"). Any other method on such a path is a 405; a path
// of another shape is passed to Next, as an Express router falls through.
type Handler struct {
	DB *db.DB
	// Next receives requests that are not /<token>/<file>; nil answers 404.
	Next http.Handler
	// Logger records unexpected failures; nil uses slog.Default().
	Logger *slog.Logger
}

var fileRE = regexp.MustCompile(`^(schema|data|envelope)\.json$`)

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawToken, rawFile, ok := splitRoute(r.URL.EscapedPath())
	if !ok {
		if h.Next != nil {
			h.Next.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	token, err1 := httpx.DecodeURIComponent(rawToken)
	file, err2 := httpx.DecodeURIComponent(rawFile)
	if err := errors.Join(err1, err2); err != nil {
		// Express's router fails a param it cannot decode before any route
		// runs, and the error handler maps the unknown error to a 500.
		h.fail(w, r, fmt.Errorf("decode export path: %w", err))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpx.SendJSON(w, r, http.StatusMethodNotAllowed, errorBody{"export URLs are read-only", "method_not_allowed"})
		return
	}
	m := fileRE.FindStringSubmatch(file)
	if m == nil {
		h.fail(w, r, apierror.NotFound("unknown export file", "not_found", nil))
		return
	}
	body, filename, err := resolve(r.Context(), h.DB, token, m[1])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	hd := w.Header()
	hd.Set("Cache-Control", "no-store")
	hd.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	hd.Set("X-Content-Type-Options", "nosniff")
	httpx.Send(w, r, http.StatusOK, "application/json; charset=utf-8", []byte(body))
}

// fail is the router's error handler: an *apierror.Error (or a mapped
// database timeout) answers with its status and code; anything else is
// logged and answered as a bare 500.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	mapped, ok := apierror.As(err)
	if !ok {
		mapped = apierror.FromDB(err)
	}
	if mapped != nil {
		httpx.SendJSON(w, r, mapped.Status, errorBody{mapped.Message, mapped.Code})
		return
	}
	logger := h.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("data export error", "err", err)
	httpx.SendJSON(w, r, http.StatusInternalServerError, errorBody{Error: "internal error"})
}

// splitRoute matches Express's "/:token/:file" (non-strict: one trailing
// slash allowed) against the raw path, returning the still-encoded params.
func splitRoute(p string) (token, file string, ok bool) {
	p = strings.TrimSuffix(p, "/")
	rest, found := strings.CutPrefix(p, "/")
	if !found {
		return "", "", false
	}
	token, file, found = strings.Cut(rest, "/")
	if !found || token == "" || file == "" || strings.Contains(file, "/") {
		return "", "", false
	}
	return token, file, true
}
