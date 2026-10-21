// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package central is sign-in through a password-mode Auth deployment
// (PAGES_AUTH_MODE=central). It mirrors the contract every Elcano
// application implements against Auth's application handoff
// (auth/docs/INTEGRATION.md, "Password mode: the application contract"),
// ported from Lens/Explorer:
//
//  1. /authorize with S256 PKCE, state and nonce;
//  2. /token with client_secret_basic, claims checked (iss, aud, exp, nonce);
//  3. a local access list (who may use THIS Pages), maintained by Auth's
//     signed access events and by `pages access` for bootstrap;
//  4. an opaque Pages session, stored only as its SHA-256 hash;
//  5. sign-out both ways: our logout hands the browser to Auth's RP logout,
//     and Auth's back-channel logout ends every session for the subject.
//
// Session conventions are the owner-decided ones shared by every Elcano
// service: 24 h absolute, 12 h idle, touched at most once a minute, the idle
// clock never extending past the absolute one. Auth's central session is the
// long one; expiry here costs a silent redirect while that session is live.
//
// Nothing in this package mints a credential another host accepts: the
// session cookie is host-only on the dashboard host, and the content host
// never reads it (the server routes the two hosts to different handlers).
//
// Every wire format — cookies, signed-token checks, database rows — is the
// one lib/central-auth.js used, so a running deployment keeps its sessions
// across the switch to this implementation.
package central

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/ElcanoTek/pages/internal/jsstr"
	"golang.org/x/text/unicode/norm"
)

// Wire constants of the Auth contract and the session convention.
const (
	BackchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"
	ApplicationAccessEvent = "urn:elcanotek:event:application-access"
	ClockSkewSeconds       = 60
	IdleSeconds            = 12 * 60 * 60
	AbsoluteSeconds        = 24 * 60 * 60
	// SessionTouchSeconds is a constant, never a setting: one write per
	// minute per session at most, so the idle limit behaves as "12 hours
	// minus at most one minute".
	SessionTouchSeconds                     = 60
	LoginTransactionSeconds                 = 10 * 60
	RevocationEventRetentionSeconds         = 7 * 24 * 60 * 60
	MaxTokenResponseBytes                   = 64 * 1024
	MaxJWKSBytes                            = 64 * 1024
	MaxSignedTokenChars                     = 16 * 1024
	JWKSCacheSeconds                        = 10 * 60
	JWKSMinRefreshSeconds                   = 60
	HTTPTimeout                             = 10 * time.Second
	JWKSTimeout                             = 5 * time.Second
	maxSafeInteger                  float64 = 1<<53 - 1
)

// The error kinds. Every error this package returns for a refused sign-in or
// token matches ErrCentralAuth under errors.Is; the specific kinds also match
// their own sentinel, mirroring the JS class hierarchy.
var (
	// ErrCentralAuth is the base kind (CentralAuthError).
	ErrCentralAuth = errors.New("central auth error")
	// ErrAccessDenied: the identity is valid but not on this Pages access
	// list.
	ErrAccessDenied = errors.New("access denied")
	// ErrAuthTransaction: the callback does not match a live login
	// transaction in this browser.
	ErrAuthTransaction = errors.New("auth transaction error")
	// ErrCodeExchangeRejected: Auth refused the code (consumed, expired,
	// superseded by another tab). The person retries; it is not an outage.
	ErrCodeExchangeRejected = errors.New("code exchange rejected")
)

// Error is a refusal with its kind and the message the JS error carried.
type Error struct {
	Kind    error
	Message string
}

func (e *Error) Error() string { return e.Message }

// Is matches the error's own kind and ErrCentralAuth.
func (e *Error) Is(target error) bool {
	return target == ErrCentralAuth || (e.Kind != nil && target == e.Kind)
}

func newErr(kind error, msg string) *Error { return &Error{Kind: kind, Message: msg} }

func errInvalidToken() error { return newErr(nil, "invalid token") }

func now() int64 { return time.Now().Unix() }

func nowMs() int64 { return time.Now().UnixMilli() }

// isInt is Number.isSafeInteger for a decoded JSON value.
func isInt(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f > maxSafeInteger || f < -maxSafeInteger || f != math.Trunc(f) {
		return 0, false
	}
	return int64(f), true
}

// StripTrailingSlashes — "https://auth.example/" names the same issuer as
// the configured origin. A loop, not a /\/+$/ regexp: that pattern
// backtracks quadratically on a long run of slashes in an attacker-supplied
// value (Go's regexp would not, but the loop says what it means).
func StripTrailingSlashes(value string) string {
	end := len(value)
	for end > 0 && value[end-1] == '/' {
		end--
	}
	return value[:end]
}

// isControlOrSpace is /[\p{C}\s]/u: control, format, private-use, surrogate
// and unassigned code points, and JavaScript whitespace.
func isControlOrSpace(r rune) bool {
	if jsstr.IsSpace(r) {
		return true
	}
	return !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z)
}

// jsLower is String.prototype.toLowerCase for the cases where Go's simple
// mapping differs in ways an address can contain: U+0130 lowercases to "i̇"
// (two code points), and a capital sigma that ends a word becomes a final
// sigma (ς).
func jsLower(s string) string {
	s = strings.ReplaceAll(s, "\u0130", "i\u0307")
	if !strings.ContainsRune(s, 'Σ') {
		return strings.ToLower(s)
	}
	runes := []rune(s)
	cased := func(r rune) bool { return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) }
	ignorable := func(r rune) bool {
		return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) || r == '\'' || r == '.' || r == ':' || r == 0x00b7 || r == 0x2019
	}
	var b strings.Builder
	for i, r := range runes {
		if r != 'Σ' {
			b.WriteString(strings.ToLower(string(r)))
			continue
		}
		before, after := false, false
		for j := i - 1; j >= 0; j-- {
			if !ignorable(runes[j]) {
				before = cased(runes[j])
				break
			}
		}
		for j := i + 1; j < len(runes); j++ {
			if !ignorable(runes[j]) {
				after = cased(runes[j])
				break
			}
		}
		if before && !after {
			b.WriteRune('ς')
		} else {
			b.WriteRune('σ')
		}
	}
	return b.String()
}

// NormalizeEmail — Auth already sends a normalized address; this applies the
// same shape to CLI input so `pages access grant` and Auth's events agree.
func NormalizeEmail(raw string) (string, error) {
	email := jsLower(jsstr.Trim(norm.NFC.String(strings.ToValidUTF8(raw, "�"))))
	at := strings.IndexByte(email, '@')
	if email == "" ||
		jsstr.UTF16Len(email) > 254 ||
		at < 1 ||
		at != strings.LastIndexByte(email, '@') ||
		at == len(email)-1 ||
		strings.HasSuffix(email, ".") ||
		email[at+1] == '.' ||
		strings.IndexFunc(email, isControlOrSpace) >= 0 {
		return "", newErr(nil, "a valid email address is required")
	}
	return email, nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
