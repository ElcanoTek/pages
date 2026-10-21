// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package adminapi

// The admin-side halves of test/password-reveal.integration.js: showing a
// client password to staff (internal/passwordvault). Only the admin cookie +
// CSRF surface reads it, every read is audited, a stale or missing copy is
// reported rather than shown, and a password that predates copies is kept
// the next time a client signs in with it. (The vault's own unit checks live
// in internal/passwordvault.)

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/pagecookie"
	"github.com/ElcanoTek/pages/internal/passwordvault"
	"github.com/ElcanoTek/pages/internal/portals"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/versions"
)

func TestPasswordReveal(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	agent := versions.Actor{Actor: "victoria@agent", ActorType: "agent", IP: "127.0.0.1"}
	const page = "/api/v1/admin/pages/rv-fabrikam/reveal-password"
	const pw = "fabrikam-client-pass"

	// ── a page password set by an agent is showable by staff ──
	must[*versions.CreatedPage](t)(versions.CreatePage(ctx, e.d, versions.CreatePageInput{Slug: "rv-fabrikam", Title: "Fabrikam"}, agent))
	must[*versions.DeployResult](t)(versions.Deploy(ctx, e.d, versions.DeployInput{Slug: "rv-fabrikam",
		HTML: `<!doctype html><html><head><title>Fabrikam</title></head><body><h1>Fabrikam Q3</h1></body></html>`, Publish: true}, agent))
	must[*versions.PasswordResult](t)(versions.SetPassword(ctx, e.d, "rv-fabrikam", pw, agent))

	before := e.auditCount("reveal_password")
	shown := e.post(page, nil)
	expect(t, shown, 200, "reveal")
	eq(t, shown.body, `{"slug":"rv-fabrikam","has_password":true,"available":true,"password":"`+pw+`"}`, "reveal body")
	eq(t, shown.header.Get("Cache-Control"), "no-store", "a credential must not sit in a cache")
	eq(t, shown.header.Get("ETag"), "", "no SHA-1 of a body holding a password")
	eq(t, e.auditCount("reveal_password"), before+1, "every reveal is audited")
	var actor, actorType, metadata string
	if err := e.d.QueryRow(ctx, `SELECT actor, actor_type, metadata::text FROM audit_log WHERE action = 'reveal_password' ORDER BY id DESC LIMIT 1`).Scan(&actor, &actorType, &metadata); err != nil {
		t.Fatal(err)
	}
	eq(t, actor, admin, "the audit row names who looked")
	eq(t, actorType, "user", "actor type")
	if strings.Contains(metadata, pw) {
		t.Fatal("the audit row never carries the password")
	}

	// ── who may NOT read it ──
	expect(t, e.req("POST", page, opts{body: map[string]any{}}), 401, "no session")
	expect(t, e.req("POST", page, opts{cookie: e.outsideCk, body: map[string]any{}}), 403, "outsider")
	expect(t, e.req("POST", page, opts{cookie: e.adminCk, origin: origin, body: map[string]any{}}), 403, "CSRF is required")
	if e.req("GET", page, opts{cookie: e.adminCk}).status == 200 {
		t.Fatal("there is no GET form of the reveal")
	}
	minted := must[*tokens.Minted](t)(tokens.Mint(ctx, e.d, tokens.MintParams{Label: "reveal-agent", Scope: "deploy"}))
	for _, path := range []string{"/api/v1/pages/rv-fabrikam/reveal-password", page} {
		r := e.req("POST", path, opts{bearer: minted.Token, body: map[string]any{}})
		if r.status == 200 || strings.Contains(r.body, pw) {
			t.Fatalf("an agent token never reads a password (%s): %d %s", path, r.status, r.body)
		}
	}
	_, err := versions.RevealPassword(ctx, e.d, "rv-fabrikam", agent)
	expectAPIErr(t, err, 403, "admin_only", "the state machine refuses an agent")

	// ── no other read carries the password or its copy ──
	leak := regexp.MustCompile(pw + `|password_ciphertext|v1\.[A-Za-z0-9_-]{16}\.`)
	for label, r := range map[string]resp{
		"admin detail": e.get("/api/v1/admin/pages/rv-fabrikam"),
		"agent read":   e.req("GET", "/api/v1/pages/rv-fabrikam", opts{bearer: minted.Token}),
	} {
		expect(t, r, 200, label)
		if leak.MatchString(r.body) {
			t.Fatalf("%s carries a password", label)
		}
	}

	// ── a password from before copies were kept appears after a sign-in ──
	signIn := func(path, password string) resp {
		return e.req("POST", path, opts{host: contentHost, form: url.Values{"password": {password}}})
	}
	if _, err := e.d.Exec(ctx, `UPDATE pages SET password_ciphertext = NULL WHERE slug = 'rv-fabrikam'`); err != nil {
		t.Fatal(err)
	}
	missing := e.post(page, nil)
	eq(t, missing.at("available"), false, "no copy yet is reported")
	eq(t, missing.at("password"), nil, "and not guessed")
	expect(t, signIn("/rv-fabrikam", "not-the-password"), 401, "wrong guess")
	eq(t, e.post(page, nil).at("available"), false, "a wrong guess never becomes the stored copy")
	expect(t, signIn("/rv-fabrikam", pw), 303, "the client still signs in")
	recovered := e.post(page, nil)
	eq(t, recovered.at("available"), true, "recovered")
	eq(t, recovered.str("password"), pw, "kept from the sign-in that verified")

	// ── a copy for a different hash is reported unavailable ──
	otherHash := must[string](t)(pagecookie.HashPassword("an-old-password"))
	if _, err := e.d.Exec(ctx, `UPDATE pages SET password_ciphertext = $1 WHERE slug = 'rv-fabrikam'`, passwordvault.Seal("an-old-password", otherHash)); err != nil {
		t.Fatal(err)
	}
	eq(t, e.post(page, nil).at("available"), false, "a stale copy is not shown")
	signIn("/rv-fabrikam", pw)
	eq(t, e.post(page, nil).str("password"), pw, "the next sign-in replaces a copy that no longer opens")

	// ── clearing the password clears the copy ──
	e.post("/api/v1/admin/pages/rv-fabrikam/password", map[string]any{"password": ""})
	eq(t, e.post(page, nil).body, `{"slug":"rv-fabrikam","has_password":false,"available":false,"password":null}`, "cleared")
	var ct *string
	if err := e.d.QueryRow(ctx, `SELECT password_ciphertext FROM pages WHERE slug = 'rv-fabrikam'`).Scan(&ct); err != nil || ct != nil {
		t.Fatalf("no copy outlives the password: %v %v", ct, err)
	}

	// ── portals: create, show, rotate, sign-in capture ──
	created := e.post("/api/v1/admin/portals", map[string]any{"slug": "rv-northwind", "name": "Northwind Reveal"})
	expect(t, created, 201, "create portal")
	id := created.str("portal", "id")
	pr := "/api/v1/admin/portals/" + id + "/reveal-password"
	portalBefore := e.auditCount("reveal_portal_password")
	ps := e.post(pr, nil)
	expect(t, ps, 200, "portal reveal")
	eq(t, ps.body, `{"id":"`+id+`","available":true,"password":"`+created.str("password")+`"}`, "portal reveal body")
	eq(t, ps.header.Get("Cache-Control"), "no-store", "portal no-store")
	eq(t, e.auditCount("reveal_portal_password"), portalBefore+1, "portal reveals are audited")
	rotated := e.post("/api/v1/admin/portals/"+id+"/password", nil).str("password")
	eq(t, e.post(pr, nil).str("password"), rotated, "after a rotation the new password is shown")
	if _, err := e.d.Exec(ctx, `UPDATE page_portals SET password_ciphertext = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	eq(t, e.post(pr, nil).at("available"), false, "portal copy missing")
	expect(t, signIn("/portal/rv-northwind", rotated), 303, "the partner still signs in")
	eq(t, e.post(pr, nil).str("password"), rotated, "kept from the partner's next sign-in")
	_, err = portals.RevealPassword(ctx, e.d, id, agent)
	expectAPIErr(t, err, 403, "portal_admin_only", "an agent never reads a portal password")
	expect(t, e.req("POST", pr, opts{bearer: minted.Token, body: map[string]any{}}), 401, "bearer on the admin surface")
	if pd := e.get("/api/v1/admin/portals/" + id); strings.Contains(pd.body, rotated) || strings.Contains(pd.body, "password_ciphertext") {
		t.Fatal("portal reads carry no password")
	}

	// ── unknown targets ──
	expect(t, e.post("/api/v1/admin/pages/rv-nope/reveal-password", nil), 404, "unknown page")
	nf := e.post("/api/v1/admin/portals/999999/reveal-password", nil)
	expect(t, nf, 404, "unknown portal")
	eq(t, nf.header.Get("Cache-Control"), "no-store", "no-store is set before the lookup, as res.set did")
}
