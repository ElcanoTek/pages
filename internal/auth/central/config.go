// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package central

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/jsstr"
)

// Config is every central-mode setting, validated.
type Config struct {
	// Issuer is Auth's canonical origin (no trailing slash).
	Issuer string
	// PublicURL is this deployment's dashboard origin.
	PublicURL    string
	ClientID     string
	ClientSecret string
	// StaticKeys are AUTH_SIGNING_PUBKEY plus AUTH_SIGNING_PREVIOUS_PUBKEYS.
	StaticKeys   []ed25519.PublicKey
	CookieSecure bool
	// TransactionKey signs the login-transaction cookie: a distinct key
	// derived from PAGE_COOKIE_SECRET, so no new secret has to be provisioned
	// and no other purpose shares the key.
	TransactionKey []byte
}

// Getenv looks a variable up, reporting whether it is set (os.LookupEnv).
type Getenv func(name string) (string, bool)

// MapEnv adapts a map to Getenv (tests).
func MapEnv(m map[string]string) Getenv {
	return func(name string) (string, bool) { v, ok := m[name]; return v, ok }
}

func truthy(v string) bool {
	switch strings.ToLower(jsstr.Trim(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// parseOrigin is lib/central-auth.js parseOrigin over WHATWG URL semantics:
// an absolute http(s) URL with no credentials, query, fragment or path, as
// its origin.
func parseOrigin(name, raw string, allowHTTP bool) (string, error) {
	text := jsstr.Trim(raw)
	unparsable := fmt.Errorf("%s must be an absolute https:// origin", name)
	u, err := url.Parse(text)
	if err != nil || u.Scheme == "" {
		return "", unparsable
	}
	scheme := strings.ToLower(u.Scheme)
	special := scheme == "https" || scheme == "http"
	if special && (u.Host == "" || u.Opaque != "") {
		// new URL("https:") throws: a special scheme needs a host.
		return "", unparsable
	}
	what := "https"
	if allowHTTP {
		what = "http(s)"
	}
	bad := fmt.Errorf("%s must be an %s:// origin without a path, query or credentials", name, what)
	if scheme != "https" && (!allowHTTP || scheme != "http") {
		return "", bad
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "/" && u.Path != "") {
		return "", bad
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

var b64Std = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

// DecodePublicKey decodes a base64 (standard alphabet) 32-byte Ed25519 key,
// nil when it is not one.
func DecodePublicKey(encoded string) ed25519.PublicKey {
	text := jsstr.Trim(encoded)
	if !b64Std.MatchString(text) {
		return nil
	}
	body := strings.TrimRight(text, "=")
	if len(body)%4 == 1 {
		body = body[:len(body)-1] // Node ignores a dangling sextet
	}
	raw, err := base64.RawStdEncoding.DecodeString(body)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}

// staticSigningKeys — AUTH_SIGNING_PUBKEY plus AUTH_SIGNING_PREVIOUS_PUBKEYS.
// Central mode refuses to start without one valid key, so back-channel
// verification works even when Auth is unreachable at boot.
func staticSigningKeys(get func(string) string) ([]ed25519.PublicKey, error) {
	var encoded []string
	for _, k := range append([]string{get("AUTH_SIGNING_PUBKEY")}, strings.Split(get("AUTH_SIGNING_PREVIOUS_PUBKEYS"), ",")...) {
		if k = jsstr.Trim(k); k != "" {
			encoded = append(encoded, k)
		}
	}
	if len(encoded) == 0 {
		return nil, errors.New("AUTH_SIGNING_PUBKEY is required in central mode: run `auth pubkey` on the Auth host")
	}
	keys := make([]ed25519.PublicKey, 0, len(encoded))
	for _, k := range encoded {
		raw := DecodePublicKey(k)
		if raw == nil {
			return nil, errors.New("AUTH_SIGNING_PUBKEY / AUTH_SIGNING_PREVIOUS_PUBKEYS must each be a base64 32-byte Ed25519 key")
		}
		keys = append(keys, raw)
	}
	return keys, nil
}

// LoadConfigFromEnv is LoadConfig(os.LookupEnv).
func LoadConfigFromEnv() (Config, error) { return LoadConfig(os.LookupEnv) }

// LoadConfig reads and validates every central-mode setting. It fails on any
// problem so a misconfigured box fails at start, not on the first sign-in.
func LoadConfig(lookup Getenv) (Config, error) {
	// get is the JS `env.X || ""`.
	get := func(name string) string { v, _ := lookup(name); return v }
	allowHTTP := truthy(get("AUTH_ALLOW_INSECURE_HTTP"))
	cookieSecure := true
	if v, ok := lookup("PAGES_AUTH_COOKIE_SECURE"); ok {
		cookieSecure = truthy(v)
	}
	if !cookieSecure && !allowHTTP {
		return Config{}, errors.New("PAGES_AUTH_COOKIE_SECURE=0 needs AUTH_ALLOW_INSECURE_HTTP=1 (local development only)")
	}
	dashboardHost := strings.ToLower(jsstr.Trim(get("DASHBOARD_HOST")))
	if dashboardHost == "" {
		return Config{}, errors.New("central mode needs DASHBOARD_HOST: the sign-in callback must land on it")
	}
	publicURL := get("PAGES_PUBLIC_URL")
	if publicURL == "" {
		publicURL = "https://" + dashboardHost
	}
	clientID := get("AUTH_CLIENT_ID")
	if clientID == "" {
		clientID = "pages"
	}
	clientID = jsstr.Trim(clientID)
	clientSecret := get("AUTH_CLIENT_SECRET")
	if clientID == "" || strings.Contains(clientID, ":") || jsstr.UTF16Len(clientID) > 128 {
		return Config{}, errors.New("AUTH_CLIENT_ID must be 1-128 characters without ':'")
	}
	if n := len(clientSecret); n < 32 || n > 256 || strings.Contains(clientSecret, ":") {
		return Config{}, errors.New("AUTH_CLIENT_SECRET must be 32-256 bytes without ':' (it is shown once by `auth app create`)")
	}
	cookieSecret := get("PAGE_COOKIE_SECRET")
	if len(cookieSecret) < 16 {
		return Config{}, errors.New("central mode signs its login transaction with PAGE_COOKIE_SECRET; it must be set")
	}
	production := get("NODE_ENV") == "production" || get("PAGES_ENV") == "production"
	if production && (allowHTTP || !cookieSecure) {
		return Config{}, errors.New("AUTH_ALLOW_INSECURE_HTTP / PAGES_AUTH_COOKIE_SECURE=0 are for local development and refused in production")
	}
	parsedPublic, err := parseOrigin("PAGES_PUBLIC_URL", publicURL, allowHTTP)
	if err != nil {
		return Config{}, err
	}
	// The login cookie is host-only, so the registered callback must land on
	// the host that set it.
	if pu, _ := url.Parse(parsedPublic); pu.Host != dashboardHost {
		return Config{}, fmt.Errorf("PAGES_PUBLIC_URL must be the dashboard host (%s); the sign-in cookie is host-only", dashboardHost)
	}
	issuer, err := parseOrigin("AUTH_ISSUER_URL", get("AUTH_ISSUER_URL"), allowHTTP)
	if err != nil {
		return Config{}, err
	}
	keys, err := staticSigningKeys(get)
	if err != nil {
		return Config{}, err
	}
	txKey, err := hkdf.Key(sha256.New, []byte(cookieSecret), nil, "pages/central-auth/login-transaction/v1", 32)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Issuer:         issuer,
		PublicURL:      parsedPublic,
		ClientID:       clientID,
		ClientSecret:   clientSecret,
		StaticKeys:     keys,
		CookieSecure:   cookieSecure,
		TransactionKey: txKey,
	}, nil
}
