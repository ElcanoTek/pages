// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package rawtoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func setSecret(t *testing.T) {
	t.Helper()
	t.Setenv("RAW_TOKEN_SECRET", "test-secret-do-not-use-in-prod")
}

func mustMint(t *testing.T, p MintParams, ttl int) string {
	t.Helper()
	tok, err := Mint(p, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestRoundTripPreservesBinding(t *testing.T) {
	setSecret(t)
	c := Verify(mustMint(t, MintParams{PageID: 7, VersionID: 42, Purpose: View, RenderMode: "themed"}, DefaultTTL))
	if c == nil || c.PageID != 7 || c.VersionID != 42 || c.Purpose != "view" || c.Mode != "themed" || c.SID == "" || c.Exp == 0 {
		t.Fatalf("claims %+v", c)
	}
}

func TestRejectsTamperedSignature(t *testing.T) {
	setSecret(t)
	tok := mustMint(t, MintParams{PageID: 1, VersionID: 1, Purpose: View, RenderMode: "themed"}, DefaultTTL)
	if Verify(tok[:len(tok)-3]+"AAA") != nil {
		t.Fatal("tampered signature verified")
	}
}

// Upgrade a read-only "view" render token into a "session" page credential by
// swapping the body and keeping the old signature → must fail. The forged
// purpose is a REAL one on purpose: an unknown purpose would be caught by the
// allow-list even with a good signature, which would prove nothing about the
// signature check.
func TestRejectsTamperedPayload(t *testing.T) {
	setSecret(t)
	tok := mustMint(t, MintParams{PageID: 1, VersionID: 1, Purpose: View, RenderMode: "themed"}, DefaultTTL)
	sig := strings.Split(tok, ".")[1]
	forged := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"pid":1,"vid":1,"purpose":"session","mode":"themed","sid":"x","exp":%d}`, time.Now().Unix()+300)))
	if Verify(forged+"."+sig) != nil {
		t.Fatal("forged body verified")
	}
}

func TestMintRefusesUnknownPurpose(t *testing.T) {
	setSecret(t)
	for _, bad := range []string{"publish", "admin", "", "VIEW", "Edit"} {
		if _, err := Mint(MintParams{PageID: 1, VersionID: 1, Purpose: bad, RenderMode: "themed"}, DefaultTTL); err == nil ||
			!strings.Contains(err.Error(), "bad purpose") {
			t.Errorf("mint must refuse purpose %q (err %v)", bad, err)
		}
	}
	for _, good := range []string{View, Template, Session} {
		if Verify(mustMint(t, MintParams{PageID: 1, VersionID: 1, Purpose: good, RenderMode: "themed"}, DefaultTTL)) == nil {
			t.Errorf("%s token did not verify", good)
		}
	}
	if Verify(mustMint(t, MintParams{PageID: 1, VersionID: 1, Purpose: Edit, RenderMode: "raw", Actor: "qa@elcanotek.com"}, DefaultTTL)) == nil {
		t.Error("edit token did not verify")
	}
}

func TestRejectsExpired(t *testing.T) {
	setSecret(t)
	if Verify(mustMint(t, MintParams{PageID: 1, VersionID: 1, Purpose: View, RenderMode: "themed"}, -1)) != nil {
		t.Fatal("expired token verified")
	}
}

func TestRejectsGarbage(t *testing.T) {
	setSecret(t)
	for _, bad := range []string{"", "no-dot", ".", "a.b", "a.", ".b"} {
		if Verify(bad) != nil {
			t.Errorf("%q verified", bad)
		}
	}
}

func TestEditTokenNamesItsActor(t *testing.T) {
	setSecret(t)
	c := Verify(mustMint(t, MintParams{PageID: 7, VersionID: 70, Purpose: Edit, RenderMode: "raw", Actor: "qa@elcanotek.com"}, 60))
	if c == nil || c.Purpose != Edit || c.Actor != "qa@elcanotek.com" || c.PageID != 7 {
		t.Fatalf("claims %+v", c)
	}
	if _, err := Mint(MintParams{PageID: 7, VersionID: 70, Purpose: Edit, RenderMode: "raw"}, 60); err == nil || !strings.Contains(err.Error(), "actor") {
		t.Fatalf("edit without actor: %v", err)
	}
	if _, err := Mint(MintParams{PageID: 7, VersionID: 70, Purpose: View, RenderMode: "raw", Actor: "qa@elcanotek.com"}, 60); err == nil ||
		!strings.Contains(err.Error(), "carries no actor") {
		t.Fatalf("view with actor: %v", err)
	}
}

func TestNoSecret(t *testing.T) {
	t.Setenv("RAW_TOKEN_SECRET", "")
	if _, err := Mint(MintParams{PageID: 1, Purpose: View}, 60); err == nil {
		t.Fatal("mint without a secret must fail")
	}
	if Verify("a.b") != nil {
		t.Fatal("verify without a secret must fail")
	}
	t.Setenv("PAGES_ENV", "")
	t.Setenv("NODE_ENV", "")
	if Validate() != nil {
		t.Fatal("dev tolerates an empty secret")
	}
	t.Setenv("NODE_ENV", "production")
	if err := Validate(); err == nil || !strings.Contains(err.Error(), "RAW_TOKEN_SECRET is required in production") {
		t.Fatalf("production must fail closed: %v", err)
	}
	t.Setenv("RAW_TOKEN_SECRET", "x")
	if Validate() != nil {
		t.Fatal("configured secret validates")
	}
}

// A token the Node implementation minted (page ids as JSON strings) verifies.
func TestVerifiesNodeMintedToken(t *testing.T) {
	t.Setenv("RAW_TOKEN_SECRET", "fixture-raw")
	const node = "eyJwaWQiOiIxMiIsInZpZCI6IjM0IiwicHVycG9zZSI6InZpZXciLCJtb2RlIjoidGhlbWVkIiwic2lkIjoiNjhiMDRjODZiYmU2MjYwOSIsImV4cCI6MjEwNzk2MjQ2NX0.qX9SW1YN2u8JKbHwWAbKvsoQyF5xJF66bkLFdKtUI34"
	c := Verify(node)
	if c == nil || c.PageID != 12 || c.VersionID != 34 || c.Purpose != View || c.Mode != "themed" || c.SID != "68b04c86bbe62609" {
		t.Fatalf("claims %+v", c)
	}
}

// The minted body is the JS object's JSON, field for field.
func TestMintedBodyShape(t *testing.T) {
	setSecret(t)
	tok := mustMint(t, MintParams{PageID: 0, VersionID: 9, Purpose: Template, RenderMode: "themed", SID: "abc"}, 60)
	body, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[0])
	if !strings.HasPrefix(string(body), `{"pid":0,"vid":9,"purpose":"template","mode":"themed","sid":"abc","exp":`) ||
		!strings.HasSuffix(string(body), "}") || strings.Contains(string(body), "actor") {
		t.Fatalf("body %s", body)
	}
	m := hmac.New(sha256.New, []byte("test-secret-do-not-use-in-prod"))
	m.Write([]byte(strings.Split(tok, ".")[0]))
	if base64.RawURLEncoding.EncodeToString(m.Sum(nil)) != strings.Split(tok, ".")[1] {
		t.Fatal("signature is HMAC-SHA256(secret, body) base64url")
	}
}
