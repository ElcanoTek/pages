// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagecookie

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

func setSecret(t *testing.T) {
	t.Helper()
	t.Setenv("PAGE_COOKIE_SECRET", "unit-test-secret")
}

func TestHashVerifyRoundTripFormatUnchanged(t *testing.T) {
	setSecret(t)
	h, err := HashPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^scrypt\$16384\$8\$1\$[A-Za-z0-9+/]{22}==\$[A-Za-z0-9+/]{43}=$`).MatchString(h) {
		t.Fatalf("self-describing format kept: %s", h)
	}
	if !VerifyPassword("s3cret", h) || VerifyPassword("wrong", h) {
		t.Fatal("round trip")
	}
	if VerifyPassword("x", "not-a-hash") || VerifyPassword("x", "") {
		t.Fatal("non-hashes never verify")
	}
}

func TestNodeHashesVerify(t *testing.T) {
	// minted with crypto.scryptSync before the async switch
	legacy := "scrypt$16384$8$1$bGVnYWN5LXNhbHQtMTIzNA==$en9jQ5e2sVFXN38xXYT2N6LaGJFOvfAMXua0gQhwQ/0="
	if !VerifyPassword("legacy-pass", legacy) || VerifyPassword("other", legacy) {
		t.Fatal("legacy sync hash")
	}
	// minted by the async lib/pagecookie.js hashPassword
	node := "scrypt$16384$8$1$c20Hh3TzSY0j0RE1hqWsbg==$BQsJHtu0Yv9ZJD+rDoA0NKQpopdv1rFr21af8eIWxSg="
	if !VerifyPassword("Contoso pass", node) || VerifyPassword("Contoso pas", node) {
		t.Fatal("node async hash")
	}
}

func TestCorruptStoredHashesAreNoMatch(t *testing.T) {
	for _, bad := range []string{
		"scrypt$", "scrypt$16384$8$1$c2FsdA==", "scrypt$abc$8$1$c2FsdA==$aGFzaA==",
		"scrypt$1000$8$1$c2FsdA==$aGFzaA==",    // N not a power of two
		"scrypt$1048576$8$1$c2FsdA==$aGFzaA==", // beyond Node's 32 MiB maxmem
		"scrypt$16384$8$1$c2FsdA==$",           // empty expected hash: never "matches"
	} {
		if VerifyPassword("x", bad) {
			t.Errorf("%q verified", bad)
		}
	}
}

func TestLongPasswordsTruncateLikeJS(t *testing.T) {
	long := strings.Repeat("a", 511) + "😀tail"
	h, err := HashPassword(long)
	if err != nil {
		t.Fatal(err)
	}
	// 511 'a' + the high surrogate of 😀: the lone surrogate becomes U+FFFD.
	if !VerifyPassword(strings.Repeat("a", 511)+"�", h) {
		t.Fatal("UTF-16 truncation must match JS slice(0, 512)")
	}
	if TruncatePassword(strings.Repeat("é", 600)) != strings.Repeat("é", 512) {
		t.Fatal("non-ASCII counts code units, not bytes")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	setSecret(t)
	hash := "scrypt$16384$8$1$c2FsdA==$aGFzaA=="
	if !VerifySession(MintSession(7, 60, hash), 7, hash) {
		t.Fatal("hash-bound session")
	}
	// Elcano-only pages (no password) bind to the empty state
	if !VerifySession(MintSession(7, 60, ""), 7, "") {
		t.Fatal("broker session")
	}
}

func TestPasswordChangeInvalidatesSessions(t *testing.T) {
	setSecret(t)
	oldHash := "scrypt$16384$8$1$b2xk$b2xk"
	newHash := "scrypt$16384$8$1$bmV3$bmV3"
	s := MintSession(7, 60, oldHash)
	if !VerifySession(s, 7, oldHash) {
		t.Fatal("sanity: valid under the minting credential")
	}
	if VerifySession(s, 7, newHash) {
		t.Fatal("rotated password → session invalid")
	}
	if VerifySession(s, 7, "") {
		t.Fatal("cleared password → session invalid")
	}
	if VerifySession(MintSession(7, 60, ""), 7, newHash) {
		t.Fatal("first password set → broker session invalid")
	}
}

func signAsPage(body string) string {
	m := hmac.New(sha256.New, []byte("unit-test-secret"))
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func signAsPortal(body string) string { return signAsPage("portal." + body) }

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestLegacySessionsWithoutDigestRejected(t *testing.T) {
	setSecret(t)
	body := b64(fmt.Sprintf(`{"pid":7,"exp":%d}`, time.Now().Unix()+60))
	if VerifySession(body+"."+signAsPage(body), 7, "") {
		t.Fatal("pre-binding payload must fail closed")
	}
}

func TestSessionRejectsWrongPage(t *testing.T) {
	setSecret(t)
	if VerifySession(MintSession(7, 60, ""), 8, "") {
		t.Fatal("wrong page id")
	}
	if VerifySession(MintSession(7, -1, ""), 7, "") {
		t.Fatal("expired")
	}
}

func TestPortalSession(t *testing.T) {
	setSecret(t)
	hash := "scrypt$16384$8$1$cG9ydGFs$aGFzaA=="
	rotated := "scrypt$16384$8$1$cm90YXRl$aGFzaA=="
	tok := MintPortalSession(3, 60, hash)
	switch {
	case !VerifyPortalSession(tok, 3, hash):
		t.Fatal("round trip")
	case VerifyPortalSession(tok, 4, hash):
		t.Fatal("bound to one portal")
	case VerifyPortalSession(tok, 3, rotated):
		t.Fatal("rotation is revocation")
	case VerifyPortalSession(MintPortalSession(3, -1, hash), 3, hash):
		t.Fatal("expiry honoured")
	case VerifyPortalSession("garbage", 3, hash):
		t.Fatal("garbage")
	case VerifyPortalSession(tok+"x", 3, hash):
		t.Fatal("tampered signature")
	case PortalCookieName(3) != "pgp3":
		t.Fatal("cookie name")
	}
	// Path=/ is required: the cookie must ride requests for member pages at any slug
	if h := PortalSessionCookieHeader(3, CookieOptions{PasswordHash: hash}); !regexp.MustCompile(
		`^pgp3=[^;]+; Path=/; HttpOnly; SameSite=Lax; Max-Age=2592000; Secure$`).MatchString(h) {
		t.Fatalf("portal cookie header %q", h)
	}
}

func TestPageAndPortalCredentialsDoNotCross(t *testing.T) {
	setSecret(t)
	hash := "scrypt$16384$8$1$c2FsdA==$aGFzaA=="
	pageTok := MintSession(9, 60, hash)
	portalTok := MintPortalSession(9, 60, hash)
	if pageTok == portalTok {
		t.Fatal("the two tokens are not interchangeable byte strings")
	}
	if VerifyPortalSession(pageTok, 9, hash) || VerifySession(portalTok, 9, hash) {
		t.Fatal("cross-type replay")
	}
	if CredentialDigest(hash) == PortalCredentialDigest(hash) || CredentialDigest("") == PortalCredentialDigest("") {
		t.Fatal("digests are domain-separated")
	}
}

// The cross-replay test above passes on the field name alone; this isolates
// the MAC domain separation.
func TestMACDomainSeparation(t *testing.T) {
	setSecret(t)
	hash := "scrypt$16384$8$1$c2FsdA==$aGFzaA=="
	now := time.Now().Unix()
	portalBody := b64(fmt.Sprintf(`{"poid":5,"exp":%d,"cd":"%s"}`, now+60, PortalCredentialDigest(hash)))
	if VerifyPortalSession(portalBody+"."+signAsPage(portalBody), 5, hash) {
		t.Fatal("a portal payload signed with the page key must not verify")
	}
	if !VerifyPortalSession(portalBody+"."+signAsPortal(portalBody), 5, hash) {
		t.Fatal("sanity: the identical payload with the portal key does verify")
	}
	pageBody := b64(fmt.Sprintf(`{"pid":5,"exp":%d,"cd":"%s"}`, now+60, CredentialDigest(hash)))
	if VerifySession(pageBody+"."+signAsPortal(pageBody), 5, hash) {
		t.Fatal("a page payload signed with the portal key must not verify")
	}
	if !VerifySession(pageBody+"."+signAsPage(pageBody), 5, hash) {
		t.Fatal("sanity: the identical payload with the page key does verify")
	}
}

func TestPortalWrongShapeFailsClosed(t *testing.T) {
	setSecret(t)
	hash := "scrypt$16384$8$1$c2FsdA==$aGFzaA=="
	now := time.Now().Unix()
	noDigest := b64(fmt.Sprintf(`{"poid":3,"exp":%d}`, now+60))
	if VerifyPortalSession(noDigest+"."+signAsPortal(noDigest), 3, hash) {
		t.Fatal("no digest")
	}
	pageShaped := b64(fmt.Sprintf(`{"pid":3,"exp":%d,"cd":"%s"}`, now+60, PortalCredentialDigest(hash)))
	if VerifyPortalSession(pageShaped+"."+signAsPortal(pageShaped), 3, hash) {
		t.Fatal("pid instead of poid")
	}
}

// Tokens and digests are byte-identical to lib/pagecookie.js's.
func TestNodeCompatibility(t *testing.T) {
	t.Setenv("PAGE_COOKIE_SECRET", "fixture-secret")
	hash := "scrypt$16384$8$1$c2FsdA==$aGFzaA=="
	if CredentialDigest(hash) != "63e2d130168a2c29" || CredentialDigest("") != "efcd3778bc5452fc" ||
		PortalCredentialDigest(hash) != "1478c5aaa40152a4" {
		t.Fatal("credential digests differ from Node")
	}
	nodeSession := "eyJwaWQiOjcsImV4cCI6MjEwNzk2MjQ2NSwiY2QiOiI2M2UyZDEzMDE2OGEyYzI5In0.tYbiyjZKlJsUWKs6i5eDU4sJZ4QncK4d88iChUvvzso"
	nodePortal := "eyJwb2lkIjozLCJleHAiOjIxMDc5NjI0NjUsImNkIjoiMTQ3OGM1YWFhNDAxNTJhNCJ9.MStu97YXwp8hZWzLT9gejv97i_FghvPFOMTXbNkH4g4"
	if !VerifySession(nodeSession, 7, hash) || !VerifyPortalSession(nodePortal, 3, hash) {
		t.Fatal("Node-minted sessions must verify")
	}
	// And the Go body is the JS JSON.stringify shape.
	body, _ := base64.RawURLEncoding.DecodeString(strings.Split(MintSession(7, 60, hash), ".")[0])
	if !regexp.MustCompile(`^\{"pid":7,"exp":\d+,"cd":"63e2d130168a2c29"\}$`).Match(body) {
		t.Fatalf("body %s", body)
	}
}

func TestCookieHeaders(t *testing.T) {
	setSecret(t)
	if h := SessionCookieHeader(12, CookieOptions{TTLSeconds: 3600, Insecure: true}); !regexp.MustCompile(
		`^pgs12=[^;]+; Path=/; HttpOnly; SameSite=Lax; Max-Age=3600$`).MatchString(h) {
		t.Fatalf("session header %q", h)
	}
	if got := ClearedSessionCookieHeader(12, CookieOptions{}); got != "pgs12=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0; Secure" {
		t.Fatalf("cleared %q", got)
	}
	if got := ClearedPortalSessionCookieHeader(4, CookieOptions{Insecure: true}); got != "pgp4=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0" {
		t.Fatalf("cleared portal %q", got)
	}
	if CookieName(12) != "pgs12" {
		t.Fatal("cookie name")
	}
}

func TestValidate(t *testing.T) {
	t.Setenv("PAGE_COOKIE_SECRET", "")
	t.Setenv("NODE_ENV", "")
	t.Setenv("PAGES_ENV", "")
	if Validate() != nil {
		t.Fatal("dev tolerates an empty key")
	}
	t.Setenv("PAGES_ENV", "production")
	if err := Validate(); err == nil || !strings.Contains(err.Error(), "PAGE_COOKIE_SECRET is required in production") {
		t.Fatalf("production must fail closed: %v", err)
	}
}
