// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package pagecookie is the per-page client session for the CONTENT host
// (PLAN §6b, amended for direct-serve). Two pieces:
//
//  1. Password hashing — scrypt. The stored `pages.password_hash` is
//     `scrypt$N$r$p$saltB64$hashB64`, self-describing so params can evolve.
//     (PLAN said "bcrypt"; scrypt is an equivalent memory-hard KDF — same
//     column.) The format is byte-for-byte what lib/pagecookie.js stored, so
//     every existing hash keeps verifying.
//
//  2. Page-session cookie — after a client enters the right password the
//     content host sets a signed, HttpOnly cookie scoped to the CONTENT origin
//     (its own cookie jar — NOT the SSO cookie). Signed with
//     PAGE_COOKIE_SECRET, bound to the page id, ~30d. The content origin is
//     sandboxed (opaque) so page JS can't read it; HttpOnly
//     belt-and-suspenders.
//
// The session cookie is keyed by page id (slugs may contain '/', illegal in a
// cookie name): cookie name = `pgs<id>`. Portal sessions are `pgp<id>`.
// Tokens and cookie strings are identical to the Node implementation's, so
// live client sessions survive the switch.
package pagecookie

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"

	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/nodebuf"
)

// scrypt parameters for new hashes.
const (
	scryptN      = 16384
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 32
	// Node's crypto.scrypt default maxmem; stored params needing more are
	// refused (and a corrupt stored hash cannot make us allocate gigabytes).
	scryptMaxMem = 32 << 20
)

// DefaultTTLDays is a session's default lifetime. The portal index tells the
// reader how long their session lasts, and must read the number rather than
// restate it.
const DefaultTTLDays = 30

// DefaultTTLSeconds is DefaultTTLDays in seconds.
const DefaultTTLSeconds = DefaultTTLDays * 86400

// MaxPasswordLength is the cheap input bound (in UTF-16 code units, as the JS
// counted) applied before the deliberately expensive KDF runs — scrypt cost
// doesn't scale with input length, but there's no reason to hash megabytes.
const MaxPasswordLength = 512

func secret() string { return env.String("PAGE_COOKIE_SECRET", "") }

// Validate fails CLOSED in production: an empty HMAC key lets anyone compute a
// valid pgs<id> cookie and walk past the per-page password gate on
// confidential client pages. Outside production an empty key is tolerated
// with a warning. The server calls this at boot.
func Validate() error {
	if secret() != "" {
		return nil
	}
	if env.Production() {
		return errors.New("PAGE_COOKIE_SECRET is required in production (an empty key lets anyone forge page-session cookies and bypass the password gate)")
	}
	slog.Warn("PAGE_COOKIE_SECRET is unset — page-session cookies use an empty key (dev only).")
	return nil
}

// TruncatePassword applies MaxPasswordLength exactly as the JS
// String(plain).slice(0, 512) did, so a long password hashes, seals and
// verifies identically in both implementations.
func TruncatePassword(plain string) string { return nodebuf.SliceUTF16(plain, MaxPasswordLength) }

// ── password hashing (scrypt) ───────────────────────────────────────────────

// HashPassword returns a new `scrypt$16384$8$1$salt$hash` for plain.
func HashPassword(plain string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash, err := scrypt.Key([]byte(TruncatePassword(plain)), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", err
	}
	return "scrypt$" + strconv.Itoa(scryptN) + "$" + strconv.Itoa(scryptR) + "$" + strconv.Itoa(scryptP) + "$" +
		base64.StdEncoding.EncodeToString(salt) + "$" + base64.StdEncoding.EncodeToString(hash), nil
}

// VerifyPassword reports whether plain matches stored. A missing, malformed or
// corrupt stored hash is a no-match, never an error.
func VerifyPassword(plain, stored string) bool {
	if !strings.HasPrefix(stored, "scrypt$") {
		return false
	}
	parts := strings.Split(stored, "$")
	if len(parts) < 6 {
		return false
	}
	n, okN := jsInt(parts[1])
	r, okR := jsInt(parts[2])
	p, okP := jsInt(parts[3])
	if !okN || !okR || !okP || n < 2 || n&(n-1) != 0 || r < 1 || p < 1 {
		return false
	}
	// OpenSSL's memory bound, as Node applied it: 128·r·p (B) + 128·r·(N+2) (V).
	if float64(128*r*p)+float64(128*r)*float64(n+2) > scryptMaxMem {
		return false
	}
	salt := nodebuf.Base64Decode(parts[4])
	expected := nodebuf.Base64Decode(parts[5])
	if len(expected) == 0 {
		// Node would have compared two empty buffers and said "match" for any
		// password. No real row has this shape; refuse it.
		return false
	}
	got, err := scrypt.Key([]byte(TruncatePassword(plain)), salt, n, r, p, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, expected) == 1
}

// jsInt is Number(s) restricted to positive safe integers.
func jsInt(s string) (int, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f != math.Trunc(f) || f < 0 || f > 1<<30 {
		return 0, false
	}
	return int(f), true
}

// ── page-session cookie ─────────────────────────────────────────────────────

// CookieName is the page-session cookie name for pageID.
func CookieName(pageID int64) string { return "pgs" + strconv.FormatInt(pageID, 10) }

func mac(input string) string {
	m := hmac.New(sha256.New, []byte(secret()))
	m.Write([]byte(input))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func digest(input string) string {
	m := hmac.New(sha256.New, []byte(secret()))
	m.Write([]byte(input))
	return hex.EncodeToString(m.Sum(nil))[:16]
}

// CredentialDigest is a short fingerprint of the page's CURRENT credential
// state, embedded in every session at mint time and re-checked on verify.
// Rotating the password changes password_hash (fresh salt even for the same
// password) ⇒ digest mismatch ⇒ every prior session is invalid. No password
// (the Elcano-only/broker state, "") digests the empty string, so setting a
// first password — or an admin clearing one — also invalidates prior
// sessions, deliberately. Keyed with the cookie secret so the stored hash
// isn't recoverable-by-dictionary from a leaked cookie payload.
func CredentialDigest(passwordHash string) string { return digest("cred:" + passwordHash) }

func mintBody(field string, id int64, ttlSeconds int, cd string) string {
	exp := time.Now().Unix() + int64(ttlSeconds)
	js := `{"` + field + `":` + strconv.FormatInt(id, 10) + `,"exp":` + strconv.FormatInt(exp, 10) + `,"cd":"` + cd + `"}`
	return base64.RawURLEncoding.EncodeToString([]byte(js))
}

// MintSession mints a session token for a page, bound to the page id AND the
// credential state that authorized it ("" for Elcano-only pages). Callers
// pass DefaultTTLSeconds, or a shorter TTL for the Elcano-broker session.
func MintSession(pageID int64, ttlSeconds int, passwordHash string) string {
	body := mintBody("pid", pageID, ttlSeconds, CredentialDigest(passwordHash))
	return body + "." + mac(body)
}

// VerifySession reports whether token is a live session for pageID under the
// page's current passwordHash.
func VerifySession(token string, pageID int64, passwordHash string) bool {
	return verify(token, "", "pid", pageID, CredentialDigest(passwordHash))
}

// verify is the shared check. The two token types stay byte-identical to
// their JS forms; only the MAC domain prefix and the subject field differ.
func verify(token, macPrefix, field string, id int64, wantCD string) bool {
	dot := strings.IndexByte(token, '.')
	if dot < 1 {
		return false
	}
	body := token[:dot]
	if subtle.ConstantTimeCompare([]byte(token[dot+1:]), []byte(mac(macPrefix+body))) != 1 {
		return false
	}
	var p map[string]any
	if json.Unmarshal(nodebuf.Base64Decode(body), &p) != nil {
		return false
	}
	exp, ok := p["exp"].(float64)
	if !ok || exp <= float64(time.Now().Unix()) {
		return false
	}
	if subject, ok := jsNumber(p[field]); !ok || subject != float64(id) {
		return false
	}
	// Credential binding: sessions minted before this field existed (no `cd`)
	// are simply invalid — the viewer re-authenticates once. Fail closed.
	cd, ok := p["cd"].(string)
	return ok && cd == wantCD
}

// jsNumber is JavaScript's Number(v) for the JSON values a payload can hold;
// ok is false when the result would be NaN.
func jsNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || strings.ContainsAny(s, "xXpP_iInN") {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// CookieOptions configures the Set-Cookie helpers.
type CookieOptions struct {
	// TTLSeconds is the session lifetime; 0 means DefaultTTLSeconds.
	TTLSeconds int
	// Insecure drops the Secure attribute (plain-http dev only). The zero
	// value is Secure, as the JS default was.
	Insecure bool
	// PasswordHash must be the page's (or portal's) CURRENT password_hash
	// ("" for Elcano-only pages) so the session dies with the credential that
	// minted it.
	PasswordHash string
}

func (o CookieOptions) ttl() int {
	if o.TTLSeconds == 0 {
		return DefaultTTLSeconds
	}
	return o.TTLSeconds
}

func cookieHeader(name, token string, maxAge int, insecure bool) string {
	s := name + "=" + token + "; Path=/; HttpOnly; SameSite=Lax; Max-Age=" + strconv.Itoa(maxAge)
	if !insecure {
		s += "; Secure"
	}
	return s
}

// SessionCookieHeader is the Set-Cookie value for a page session.
func SessionCookieHeader(pageID int64, o CookieOptions) string {
	return cookieHeader(CookieName(pageID), MintSession(pageID, o.ttl(), o.PasswordHash), o.ttl(), o.Insecure)
}

// ── portal-session cookie ───────────────────────────────────────────────────
// A portal session is a DIFFERENT credential from a page session: it proves
// knowledge of one PORTAL's shared password, and it authorizes whatever that
// portal contains at the moment of each request. Cookie name is
// `pgp<portal_id>`.
//
// Three deliberate choices, each of which was the alternative to something
// worse:
//
//   - The two token types are domain-separated in the MAC input (`portal.` ‖
//     body) and in the credential digest (`cred:portal:`) — NOT discriminated
//     by a field inside the payload. A discriminator would have to appear in
//     the PAGE token too to mean anything, and VerifySession fails closed on
//     anything unexpected, so shipping that would have logged out every live
//     client on deploy.
//   - The payload names the portal `poid`, not `pid`, so a cross-type replay
//     still fails on the field name alone if the domain separation were ever
//     removed.
//   - Path=/ is load-bearing: the cookie has to be sent on requests for member
//     pages at arbitrary (nested) slugs. It is also why portals IMPROVE the
//     cookie-header exhaustion story — one cookie per portal instead of one
//     per page.

// PortalCookieName is the portal-session cookie name for portalID.
func PortalCookieName(portalID int64) string { return "pgp" + strconv.FormatInt(portalID, 10) }

// PortalCredentialDigest is CredentialDigest for a portal, domain-separated.
// Rotating a portal password invalidates every session for it.
// `page_portals.password_hash` is NOT NULL and non-empty by constraint, so the
// empty-string case is unreachable from a real row — it exists so that a
// caller who passes "" can never mint a token that verifies against the
// digest of "no credential at all" for a page.
func PortalCredentialDigest(passwordHash string) string { return digest("cred:portal:" + passwordHash) }

// MintPortalSession mints a portal session token.
func MintPortalSession(portalID int64, ttlSeconds int, passwordHash string) string {
	body := mintBody("poid", portalID, ttlSeconds, PortalCredentialDigest(passwordHash))
	return body + "." + mac("portal."+body)
}

// VerifyPortalSession reports whether token is a live session for portalID
// under the portal's current passwordHash.
func VerifyPortalSession(token string, portalID int64, passwordHash string) bool {
	return verify(token, "portal.", "poid", portalID, PortalCredentialDigest(passwordHash))
}

// PortalSessionCookieHeader is the Set-Cookie value for a portal session.
func PortalSessionCookieHeader(portalID int64, o CookieOptions) string {
	return cookieHeader(PortalCookieName(portalID), MintPortalSession(portalID, o.ttl(), o.PasswordHash), o.ttl(), o.Insecure)
}

// ClearedCookieHeader is the exact inverse of the two minting helpers.
//
// A sign-out is only real if the browser drops the cookie, and a browser only
// drops one when the clearing header matches the original on name, Path and
// Domain. So this is written from the same pieces rather than as a second
// hand-rolled string: get one attribute wrong and the cookie survives while
// the page cheerfully says the reader is signed out — the worst possible
// failure on the shared machine this exists for. Only Insecure is read from o.
func ClearedCookieHeader(name string, o CookieOptions) string {
	return cookieHeader(name, "", 0, o.Insecure)
}

// ClearedSessionCookieHeader clears a page session.
func ClearedSessionCookieHeader(pageID int64, o CookieOptions) string {
	return ClearedCookieHeader(CookieName(pageID), o)
}

// ClearedPortalSessionCookieHeader clears a portal session.
func ClearedPortalSessionCookieHeader(portalID int64, o CookieOptions) string {
	return ClearedCookieHeader(PortalCookieName(portalID), o)
}
