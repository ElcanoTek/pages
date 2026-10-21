// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package central

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// KidForKey is Auth's key identifier: base64url(sha256(raw public key)[:16]).
func KidForKey(raw ed25519.PublicKey) string {
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

var b64urlSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// decodeB64URL is Buffer.from(s, "base64url") for an already-validated
// alphabet: unpadded, a dangling sextet ignored.
func decodeB64URL(s string) []byte {
	s = strings.TrimRight(s, "=")
	if len(s)%4 == 1 {
		s = s[:len(s)-1]
	}
	out, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return out
}

func decodeSegment(segment string) ([]byte, error) {
	if segment == "" || !b64urlSegment.MatchString(segment) {
		return nil, errInvalidToken()
	}
	return decodeB64URL(segment), nil
}

// parseJSON is the JS parseJson: a JSON object, else nil.
func parseJSON(b []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil
	}
	return m
}

// isObject is `v && typeof v === "object"` for a decoded JSON value.
func isObject(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

func tokenKid(raw string) string {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ""
	}
	seg, err := decodeSegment(parts[0])
	if err != nil {
		return ""
	}
	if kid, ok := parseJSON(seg)["kid"].(string); ok {
		return kid
	}
	return ""
}

func safeEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// verifySignature — a compact JWS from Auth: the expected typ, EdDSA, and an
// Ed25519 signature by the key its kid names. Returns the claims, unchecked.
func verifySignature(raw, typ string, keys []ed25519.PublicKey) (map[string]any, error) {
	if wire16Len(raw) > MaxSignedTokenChars {
		return nil, errInvalidToken()
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errInvalidToken()
	}
	hseg, err := decodeSegment(parts[0])
	if err != nil {
		return nil, err
	}
	cseg, err := decodeSegment(parts[1])
	if err != nil {
		return nil, err
	}
	signature, err := decodeSegment(parts[2])
	if err != nil {
		return nil, err
	}
	header, claims := parseJSON(hseg), parseJSON(cseg)
	if header == nil || claims == nil {
		return nil, errInvalidToken()
	}
	htyp, _ := header["typ"].(string)
	alg, _ := header["alg"].(string)
	kid, kidOK := header["kid"].(string)
	if htyp != typ || alg != "EdDSA" || !kidOK {
		return nil, errInvalidToken()
	}
	signed := []byte(parts[0] + "." + parts[1])
	for _, key := range keys {
		if KidForKey(key) == kid && len(key) == ed25519.PublicKeySize && ed25519.Verify(key, signed, signature) {
			return claims, nil
		}
	}
	return nil, errInvalidToken()
}

// wire16Len is raw.length for a token (ASCII in practice; UTF-16 units in
// general).
func wire16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// VerifyOptions are what a back-channel token is checked against.
type VerifyOptions struct {
	Issuer   string
	Audience string
	Keys     []ed25519.PublicKey
	// At is the verification time in Unix seconds (0 = now).
	At int64
}

func (o VerifyOptions) at() int64 {
	if o.At != 0 {
		return o.At
	}
	return now()
}

// verifySigned — signature, typ/alg and the claims both event kinds share.
func verifySigned(raw, typ string, o VerifyOptions) (map[string]any, error) {
	claims, err := verifySignature(raw, typ, o.Keys)
	if err != nil {
		return nil, err
	}
	at := o.at()
	exp, expOK := isInt(claims["exp"])
	iat, iatOK := isInt(claims["iat"])
	iss, issOK := claims["iss"].(string)
	aud, audOK := claims["aud"].(string)
	sub, subOK := claims["sub"].(string)
	jti, jtiOK := claims["jti"].(string)
	_, hasNonce := claims["nonce"]
	if !expOK || exp+ClockSkewSeconds <= at ||
		!issOK || StripTrailingSlashes(iss) != o.Issuer ||
		!audOK || aud != o.Audience ||
		!subOK || sub == "" || wire16Len(sub) > 255 ||
		!jtiOK || jti == "" || wire16Len(jti) > 255 ||
		!iatOK || iat <= 0 || iat > at+ClockSkewSeconds ||
		hasNonce {
		return nil, errInvalidToken()
	}
	return claims, nil
}

// LogoutEvent is a verified back-channel logout.
type LogoutEvent struct {
	EventID  string
	Subject  string
	Issuer   string
	IssuedAt int64
}

// AccessEvent is a verified application-access event: Auth's desired state
// for one subject.
type AccessEvent struct {
	EventID  string
	Subject  string
	Email    string
	Issuer   string
	IssuedAt int64
	Version  int64
	Allowed  bool
}

// VerifyLogoutToken checks Auth's logout+jwt.
func VerifyLogoutToken(raw string, o VerifyOptions) (LogoutEvent, error) {
	claims, err := verifySigned(raw, "logout+jwt", o)
	if err != nil {
		return LogoutEvent{}, err
	}
	events, ok := claims["events"].(map[string]any)
	if !ok || !isObject(events[BackchannelLogoutEvent]) {
		return LogoutEvent{}, errInvalidToken()
	}
	iat, _ := isInt(claims["iat"])
	return LogoutEvent{EventID: claims["jti"].(string), Subject: claims["sub"].(string), Issuer: o.Issuer, IssuedAt: iat}, nil
}

// VerifyAccessToken checks Auth's access+jwt.
func VerifyAccessToken(raw string, o VerifyOptions) (AccessEvent, error) {
	claims, err := verifySigned(raw, "access+jwt", o)
	if err != nil {
		return AccessEvent{}, err
	}
	var access map[string]any
	if events, ok := claims["events"].(map[string]any); ok {
		access, _ = events[ApplicationAccessEvent].(map[string]any)
	}
	if access == nil {
		return AccessEvent{}, errInvalidToken()
	}
	version, vOK := isInt(access["version"])
	action, _ := access["action"].(string)
	if !vOK || version <= 0 || (action != "grant" && action != "revoke") {
		return AccessEvent{}, errInvalidToken()
	}
	rawEmail, ok := claims["email"].(string)
	if !ok {
		return AccessEvent{}, errInvalidToken()
	}
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return AccessEvent{}, errInvalidToken()
	}
	// `settings` (application-defined key/values) carries nothing Pages uses
	// — it has one human role, so a grant is the whole decision — but its
	// shape is still the contract's string-to-string map.
	if s, present := access["settings"]; present {
		settings, ok := s.(map[string]any)
		if !ok {
			return AccessEvent{}, errInvalidToken()
		}
		for _, v := range settings {
			if _, ok := v.(string); !ok {
				return AccessEvent{}, errInvalidToken()
			}
		}
	}
	iat, _ := isInt(claims["iat"])
	return AccessEvent{
		EventID:  claims["jti"].(string),
		Subject:  claims["sub"].(string),
		Email:    email,
		Issuer:   o.Issuer,
		IssuedAt: iat,
		Version:  version,
		Allowed:  action == "grant",
	}, nil
}

// IDTokenOptions are what Auth's id_token is checked against.
type IDTokenOptions struct {
	Issuer   string
	Audience string
	Nonce    string
	Keys     []ed25519.PublicKey
	At       int64
}

// VerifyIDToken checks the identity assertion from Auth's /token (typ
// "JWT"), as Auth's docs/INTEGRATION.md requires: signed by a current Auth
// key, exact iss (Auth emits its issuer without a trailing slash, and
// LoadConfig keeps the configured one canonical) and aud, live exp, and the
// nonce this sign-in sent. Identity is read from these signed claims only,
// never from the unsigned JSON beside them.
func VerifyIDToken(raw string, o IDTokenOptions) (map[string]any, error) {
	claims, err := verifySignature(raw, "JWT", o.Keys)
	if err != nil {
		return nil, err
	}
	at := o.At
	if at == 0 {
		at = now()
	}
	exp, expOK := isInt(claims["exp"])
	iat, iatOK := isInt(claims["iat"])
	iss, issOK := claims["iss"].(string)
	aud, audOK := claims["aud"].(string)
	sub, subOK := claims["sub"].(string)
	_, emailOK := claims["email"].(string)
	nonce, nonceOK := claims["nonce"].(string)
	if !expOK || exp+ClockSkewSeconds <= at ||
		!iatOK || iat <= 0 || iat > at+ClockSkewSeconds ||
		!issOK || iss != o.Issuer ||
		!audOK || aud != o.Audience ||
		!subOK || sub == "" || wire16Len(sub) > 255 ||
		!emailOK ||
		!nonceOK || !safeEqual(nonce, o.Nonce) {
		return nil, errInvalidToken()
	}
	return claims, nil
}

// readCapped reads at most limit bytes; a larger body is an error, never
// buffered whole.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, newErr(nil, "response too large")
	}
	return b, nil
}

// noRedirects is redirect: "manual": a 3xx is returned, never followed.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// KeyResolver is the static env keys plus Auth's published /jwks.json, so an
// Auth signing-key rotation needs no env edit here. Cached 10 minutes; a
// token naming an unknown kid forces at most one fetch a minute; a failed
// fetch keeps the cache. A token whose kid is already known never waits on
// Auth. Safe for concurrent use.
type KeyResolver struct {
	url        string
	staticKeys []ed25519.PublicKey
	client     *http.Client
	clock      func() int64

	mu          sync.Mutex
	remoteKeys  []ed25519.PublicKey
	fetchedAt   int64
	fetched     bool
	lastAttempt int64
	attempted   bool
}

// NewKeyResolver builds the resolver for issuer. client nil uses a default
// client (JWKS timeout, no redirects); clock nil is the wall clock in Unix
// seconds.
func NewKeyResolver(issuer string, staticKeys []ed25519.PublicKey, client *http.Client, clock func() int64) *KeyResolver {
	if client == nil {
		client = &http.Client{Timeout: JWKSTimeout, CheckRedirect: noRedirects}
	}
	if clock == nil {
		clock = now
	}
	return &KeyResolver{url: issuer + "/jwks.json", staticKeys: staticKeys, client: client, clock: clock}
}

// Known is the static and remote keys, deduplicated, static first.
func (k *KeyResolver) Known() []ed25519.PublicKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.knownLocked()
}

func (k *KeyResolver) knownLocked() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, key := range append(append([]ed25519.PublicKey{}, k.staticKeys...), k.remoteKeys...) {
		dup := false
		for _, seen := range out {
			if bytes.Equal(seen, key) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, key)
		}
	}
	return out
}

// Refresh fetches the JWKS unless one was attempted within the last minute.
// It reports whether the cache was replaced.
func (k *KeyResolver) Refresh(ctx context.Context) bool {
	k.mu.Lock()
	at := k.clock()
	if k.attempted && at-k.lastAttempt < JWKSMinRefreshSeconds {
		k.mu.Unlock()
		return false
	}
	k.attempted, k.lastAttempt = true, at
	k.mu.Unlock()

	keys, ok := k.fetch(ctx)
	if !ok {
		return false
	}
	k.mu.Lock()
	k.remoteKeys, k.fetchedAt, k.fetched = keys, at, true
	k.mu.Unlock()
	return true
}

func (k *KeyResolver) fetch(ctx context.Context) ([]ed25519.PublicKey, bool) {
	ctx, cancel := context.WithTimeout(ctx, JWKSTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	res, err := k.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, false
	}
	body, err := readCapped(res.Body, MaxJWKSBytes)
	if err != nil {
		return nil, false
	}
	doc := parseJSON(body)
	entries, ok := doc["keys"].([]any)
	if !ok {
		return nil, false
	}
	keys := []ed25519.PublicKey{}
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok || entry["kty"] != "OKP" || entry["crv"] != "Ed25519" {
			continue
		}
		x, ok := entry["x"].(string)
		if !ok || !b64urlSegment.MatchString(x) {
			continue
		}
		if raw := decodeB64URL(x); len(raw) == ed25519.PublicKeySize {
			keys = append(keys, ed25519.PublicKey(raw))
		}
	}
	return keys, true
}

// KeysForToken is the key set to verify raw with. A static key never waits
// on Auth. A key known only from the JWKS is re-checked once the cache is
// stale, so a key Auth retires stops being trusted within the cache window (a
// failed fetch keeps the cache).
func (k *KeyResolver) KeysForToken(ctx context.Context, raw string) []ed25519.PublicKey {
	kid := tokenKid(raw)
	k.mu.Lock()
	stale := !k.fetched || k.clock()-k.fetchedAt > JWKSCacheSeconds
	static, remote := false, false
	if kid != "" {
		for _, key := range k.staticKeys {
			if KidForKey(key) == kid {
				static = true
			}
		}
		for _, key := range k.remoteKeys {
			if KidForKey(key) == kid {
				remote = true
			}
		}
	}
	if static {
		defer k.mu.Unlock()
		return k.knownLocked()
	}
	k.mu.Unlock()
	if stale || (kid != "" && !remote) {
		k.Refresh(ctx)
	}
	return k.Known()
}

var errNoResolver = errors.New("Client needs a KeyResolver to verify id_token")
