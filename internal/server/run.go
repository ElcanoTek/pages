// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/auth"
	"github.com/ElcanoTek/pages/internal/clientip"
	"github.com/ElcanoTek/pages/internal/csp"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/pagecookie"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/rawtoken"
	"github.com/ElcanoTek/pages/internal/readiness"
	"github.com/ElcanoTek/pages/internal/render"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/uploads"
)

// Validate is the server's fail-closed startup validation, everything the
// Node process refused at require time: the three production secrets, the
// body/upload budgets (MAX_HTML_BYTES, PAGES_UPLOAD_MAX_BYTES), the proxy
// trust policy (PAGES_TRUST_PROXY), the sign-in configuration (central mode's
// settings, AUTH_LOGIN_URL) and the shell CSP built from its origin. It opens
// nothing, so `pages preflight` runs it too.
func Validate() error {
	if err := validateSettings(); err != nil {
		return err
	}
	a, err := auth.NewFromEnv(nil)
	if err != nil {
		return err
	}
	_, err = csp.ShellHeaders(a.Origin())
	return err
}

func validateSettings() error {
	for _, check := range []func() error{rawtoken.Validate, pagecookie.Validate, tokens.Validate, render.ValidateAssetsBase} {
		if err := check(); err != nil {
			return err
		}
	}
	if _, err := datalimits.Load(); err != nil {
		return err
	}
	_, err := clientip.FromEnv()
	return err
}

// MCPFactory builds the /mcp handler for a running server (internal/mcp);
// clientIP is the trust-proxy policy, maxBodyBytes the JSON body cap.
type MCPFactory func(d *db.DB, clientIP func(*http.Request) string, maxBodyBytes int64, logger *slog.Logger) http.Handler

// RunOptions configure Run.
type RunOptions struct {
	// MCP builds the /mcp endpoint; nil serves the JSON-RPC 503 stand-in.
	MCP MCPFactory
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Started, when set, is called once the server is accepting.
	Started func(addr net.Addr)
	// DrainTimeout bounds the graceful drain on shutdown (default 60s,
	// inside systemd's 90s stop timeout).
	DrainTimeout time.Duration
}

// Run validates the configuration, opens the database pool and readiness
// probe from the environment, and serves on ln until ctx ends. Then it stops
// accepting, drains in-flight requests, and closes the pool and the probe
// (server.js's shutdown: server.close → db.pool.end + readiness.close).
func Run(ctx context.Context, ln net.Listener, o RunOptions) error {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	// The sign-in half of Validate runs in New (auth.NewFromEnv), so its
	// warnings are logged once.
	if err := validateSettings(); err != nil {
		ln.Close()
		return err
	}
	if w := uploads.ChunkCeiling().Warning; w != "" {
		log.Warn(w)
	}
	if w := pagedata.TemplateLimit().Warning; w != "" {
		log.Warn(w)
	}
	d, err := db.OpenFromEnv()
	if err != nil {
		ln.Close()
		return err
	}
	defer d.Close()
	ready, err := readiness.NewFromEnv(pages.Migrations())
	if err != nil {
		ln.Close()
		return err
	}
	defer ready.Close()
	policy, err := clientip.FromEnv()
	if err != nil {
		ln.Close()
		return err
	}
	cfg := Config{DB: d, ClientIP: policy, Ready: ready, Logger: log}
	if o.MCP != nil {
		cfg.MCP = o.MCP(d, policy.ClientIP, int64(datalimits.MaxRequestBytes()), log)
	}
	s, err := New(cfg)
	if err != nil {
		ln.Close()
		return err
	}
	defer s.Close()

	// Node's defaults: a 60 s header timeout and 5 s keep-alive. There is
	// deliberately no ReadTimeout: it would cap slow but legitimate large
	// uploads, and once a body is read it would also fire on the server's
	// background read and cancel long streaming responses. Instead every
	// body reader arms a read deadline scaled to its size limit
	// (httpx.ArmBodyReadDeadline, via the controller ServeHTTP records), so a
	// declared but stalled body fails like a truncated one.
	hs := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 60 * time.Second,
		IdleTimeout:       5 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	if o.Started != nil {
		o.Started(ln.Addr())
	}
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	drain := o.DrainTimeout
	if drain <= 0 {
		drain = 60 * time.Second
	}
	sctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		_ = hs.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// Banner is the startup line server.js printed.
func Banner(port string) string {
	return fmt.Sprintf("Elcano Pages on :%s  (dashboard=%s  content=%s)", port, strings.ToLower(env.DashboardHost()), strings.ToLower(env.ContentHost()))
}
