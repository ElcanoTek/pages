// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package uploads

// The one endpoint that accepts page bytes over the wire instead of through a
// model's tool arguments (lib/uploadticket.js).
//
// WHY. Everywhere else, an agent hands Pages HTML by emitting it: inline html
// or base64 append_page_upload chunks. That means a 65 KB dashboard costs ~25k
// output tokens the model must produce without a single slipped character, and
// a 300 KB one is effectively undeployable — we watched turns time out
// mid-upload and an agent shrink a client's dashboard to fit the chunk budget.
//
// An agent's sandbox already has outbound HTTP. So create_upload_ticket mints a
// one-shot credential, the sandbox PUTs the file here in a single curl, and the
// model only ever touches a URL and an opaque handle.
//
// THIS IS THE ONLY BEARER PATH ON THE DASHBOARD HOST THAT IS NOT AN AGENT
// TOKEN, so it is deliberately the weakest credential in the system
// (migrations/014):
//   - write-only — stages bytes; cannot deploy, publish, read, or list.
//   - content-pinned — total_bytes and content_sha256 were fixed by the
//     authenticated agent when the ticket was minted, so the only byte string
//     this will ever accept is the one already committed to. A stolen ticket
//     cannot substitute different content; at worst it re-sends the same file.
//   - one upload, minutes-long TTL, spent on first use.
//
// Deploying the staged bytes still requires the real agent token.

import (
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// HandlerOptions configures the upload-ticket endpoint.
type HandlerOptions struct {
	DB *db.DB
	// Limit wraps every request under the mount (the API rate limiter, as
	// server.js mounted `limits.api` ahead of the router). nil = no limit.
	Limit func(http.Handler) http.Handler
	// Next receives requests that are not /<upload_id>; nil answers 404.
	Next http.Handler
	// Logger records unexpected failures; nil uses slog.Default().
	Logger *slog.Logger
}

// NewHandler returns the /upload endpoint. Mount it with the "/upload" prefix
// stripped (http.StripPrefix("/upload", h)) and AHEAD of any broad body
// parsing: it owns its raw body reader and its own (ticket, not agent-token)
// auth. PUT /<upload_id> stages the body; any other method on that path is 405.
func NewHandler(o HandlerOptions) http.Handler {
	h := &ticketHandler{o}
	if o.Limit != nil {
		return o.Limit(h)
	}
	return h
}

type ticketHandler struct{ o HandlerOptions }

type ticketError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func (h *ticketHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawID, ok := splitUploadRoute(r.URL.EscapedPath())
	if !ok {
		if h.o.Next != nil {
			h.o.Next.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	uploadID, err := httpx.DecodeURIComponent(rawID)
	if err != nil {
		// Express's router fails a param it cannot decode before any route
		// runs, and the error handler maps the unknown error to a 500.
		h.fail(w, r, fmt.Errorf("decode upload path: %w", err))
		return
	}
	// A ticket is for exactly one PUT of one document; nothing else is offered.
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		httpx.SendJSON(w, r, http.StatusMethodNotAllowed, ticketError{"use PUT to send the page bytes", "method_not_allowed"})
		return
	}
	// Authenticate on the HEADER before reading the body — an invalid ticket
	// must not make the server buffer a whole staged file first. Same ordering rule as /mcp.
	ticket := bearer(r)
	if ticket == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="pages-upload"`)
		h.fail(w, r, ticketMissing())
		return
	}
	if _, err := CheckTicket(r.Context(), h.o.DB, ticket, uploadID); err != nil {
		h.fail(w, r, err)
		return
	}
	// Raw body of any Content-Type, capped at the staged-upload ceiling: a
	// sandbox curl sends whatever Content-Type it likes (or none) — the bytes
	// are opaque and validated by length + SHA-256, not by header.
	body, err := readRawBody(r, MaxUploadBytes())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	s, err := PutTicketContent(r.Context(), h.o.DB, ticket, uploadID, body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.SendJSON(w, r, http.StatusOK, s)
}

var errTooLarge = errors.New("request entity too large")

func (h *ticketHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errTooLarge) {
		httpx.SendJSON(w, r, http.StatusRequestEntityTooLarge, ticketError{
			fmt.Sprintf("page content must be at most %d bytes", MaxUploadBytes()), "upload_size_invalid"})
		return
	}
	mapped, ok := apierror.As(err)
	if !ok {
		mapped = apierror.FromDB(err)
	}
	if mapped != nil {
		httpx.SendJSON(w, r, mapped.Status, ticketError{mapped.Message, mapped.Code})
		return
	}
	logger := h.o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("upload ticket error", "err", err)
	httpx.SendJSON(w, r, http.StatusInternalServerError, ticketError{Error: "internal error"})
}

// splitUploadRoute matches Express's "/:upload_id" (non-strict: one trailing
// slash allowed) against the raw path, returning the still-encoded param.
func splitUploadRoute(p string) (string, bool) {
	p = strings.TrimSuffix(p, "/")
	rest, found := strings.CutPrefix(p, "/")
	if !found || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// bearer is /^Bearer\s+(.+)$/i on the Authorization header, trimmed.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < len("Bearer") || !strings.EqualFold(h[:len("Bearer")], "Bearer") {
		return ""
	}
	rest := h[len("Bearer"):]
	first, size := utf8.DecodeRuneInString(rest)
	if size == 0 || !jsstr.IsSpace(first) || len(rest) == size {
		return ""
	}
	return jsstr.Trim(rest)
}

// readRawBody is express.raw({type: () => true, limit}): the body inflated
// when gzip/deflate encoded, refused past limit bytes (Content-Length is
// checked before reading). A request with no body reads as empty, which
// PutTicketContent refuses as upload_body_empty.
func readRawBody(r *http.Request, limit int) ([]byte, error) {
	enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if (enc == "" || enc == "identity") && r.ContentLength > int64(limit) {
		return nil, errTooLarge
	}
	var src io.Reader = r.Body
	switch enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, apierror.New(http.StatusBadRequest, "request body could not be decoded", "upload_encoding_invalid", nil)
		}
		defer zr.Close()
		src = zr
	case "deflate":
		zr := flate.NewReader(r.Body)
		defer zr.Close()
		src = zr
	default:
		// body-parser answered an unknown encoding with a 415 that the Node
		// error handler then reported as a 500; say what is wrong instead.
		return nil, apierror.New(http.StatusUnsupportedMediaType,
			fmt.Sprintf("unsupported content encoding %q", enc), "upload_encoding_unsupported", nil)
	}
	b, err := io.ReadAll(io.LimitReader(src, int64(limit)+1))
	if err != nil {
		if enc == "gzip" || enc == "deflate" {
			return nil, apierror.New(http.StatusBadRequest, "request body could not be decoded", "upload_encoding_invalid", nil)
		}
		return nil, fmt.Errorf("read upload body: %w", err)
	}
	if len(b) > limit {
		return nil, errTooLarge
	}
	return b, nil
}
