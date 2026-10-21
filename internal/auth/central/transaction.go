// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package central

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// Transaction is one login in flight: the PKCE verifier, state and nonce,
// and where to land afterwards.
type Transaction struct {
	State     string
	Nonce     string
	Verifier  string
	Challenge string
	Next      string
	IAT       int64
}

// OpenedTransaction is what a verified callback recovers.
type OpenedTransaction struct {
	Nonce    string
	Verifier string
	Next     string
}

func randomB64URL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewTransaction starts a login at Unix time at (0 = now).
func NewTransaction(next string, at int64) Transaction {
	if at == 0 {
		at = now()
	}
	verifier := randomB64URL(48)
	sum := sha256.Sum256([]byte(verifier))
	return Transaction{
		State:     randomB64URL(32),
		Nonce:     randomB64URL(32),
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		Next:      SafeLocalPath(next),
		IAT:       at,
	}
}

func mac(key []byte, body string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// SealTransaction is the login cookie value: base64url of the JSON
// {state, nonce, verifier, next, iat} (that key order), "." and its
// HMAC-SHA256 under key.
func SealTransaction(tx Transaction, key []byte) string {
	js := `{"state":` + jsonv.Quote(tx.State) +
		`,"nonce":` + jsonv.Quote(tx.Nonce) +
		`,"verifier":` + jsonv.Quote(tx.Verifier) +
		`,"next":` + jsonv.Quote(tx.Next) +
		`,"iat":` + strconv.FormatInt(tx.IAT, 10) + `}`
	body := base64.RawURLEncoding.EncodeToString([]byte(js))
	return body + "." + mac(key, body)
}

// OpenTransaction verifies the cookie and the callback's state at Unix time
// at (0 = now). Anything that does not match a live transaction is an
// ErrAuthTransaction.
func OpenTransaction(sealed, state string, key []byte, at int64) (OpenedTransaction, error) {
	if at == 0 {
		at = now()
	}
	fail := newErr(ErrAuthTransaction, "sign-in expired")
	if state == "" {
		return OpenedTransaction{}, fail
	}
	dot := strings.IndexByte(sealed, '.')
	if dot < 1 {
		return OpenedTransaction{}, fail
	}
	body := sealed[:dot]
	if !safeEqual(mac(key, body), sealed[dot+1:]) {
		return OpenedTransaction{}, fail
	}
	seg, err := decodeSegment(body)
	if err != nil {
		return OpenedTransaction{}, fail
	}
	tx := parseJSON(seg)
	if tx == nil {
		return OpenedTransaction{}, fail
	}
	iat, iatOK := isInt(tx["iat"])
	txState, stateOK := tx["state"].(string)
	nonce, nonceOK := tx["nonce"].(string)
	verifier, verifierOK := tx["verifier"].(string)
	if !iatOK || iat+LoginTransactionSeconds <= at || iat > at+ClockSkewSeconds ||
		!stateOK || !safeEqual(txState, state) || !nonceOK || !verifierOK {
		return OpenedTransaction{}, fail
	}
	next, _ := tx["next"].(string)
	return OpenedTransaction{Nonce: nonce, Verifier: verifier, Next: SafeLocalPath(next)}, nil
}

// SafeLocalPath is a same-host path, or "/admin". It rejects scheme and
// authority, protocol-relative and backslash forms, control characters, and
// anything long enough to push the transaction cookie past the browser's
// 4 KB limit. The path is normalized the way a WHATWG URL parser resolves it
// against the dashboard origin (dot segments, percent-encoding), and the
// result is checked again: normalization can turn "/a/..//evil.example" into
// "//evil.example".
func SafeLocalPath(raw string) string {
	const fallback = "/admin"
	if raw == "" || jsstr.UTF16Len(raw) > 2048 {
		return fallback
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, `\`) {
		return fallback
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] == 0x7f {
			return fallback
		}
	}
	out := whatwgPathAndQuery(strings.ToValidUTF8(raw, "�"))
	if !strings.HasPrefix(out, "/") || strings.HasPrefix(out, "//") || strings.Contains(out, `\`) {
		return fallback
	}
	return out
}

func isSingleDot(s string) bool { return s == "." || strings.EqualFold(s, "%2e") }

func isDoubleDot(s string) bool {
	switch strings.ToLower(s) {
	case "..", ".%2e", "%2e.", "%2e%2e":
		return true
	}
	return false
}

func pctEncode(b *strings.Builder, s string, enc func(c byte) bool) {
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		if enc(c) {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		} else {
			b.WriteByte(c)
		}
	}
}

// whatwgPathAndQuery is new URL(raw, "http://x").pathname + .search for a
// raw that starts with a single "/" and holds no backslash or control
// character.
func whatwgPathAndQuery(raw string) string {
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	path, query, hasQuery := strings.Cut(raw, "?")
	segs := strings.Split(path[1:], "/")
	var out []string
	for i, seg := range segs {
		last := i == len(segs)-1
		switch {
		case isDoubleDot(seg):
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			if last {
				out = append(out, "")
			}
		case isSingleDot(seg):
			if last {
				out = append(out, "")
			}
		default:
			var b strings.Builder
			pctEncode(&b, seg, func(c byte) bool {
				return c <= 0x20 || c >= 0x7f || strings.IndexByte("\"#<>?`{}", c) >= 0
			})
			out = append(out, b.String())
		}
	}
	var b strings.Builder
	b.WriteString("/" + strings.Join(out, "/"))
	if hasQuery && query != "" {
		b.WriteByte('?')
		pctEncode(&b, query, func(c byte) bool {
			return c <= 0x20 || c >= 0x7f || strings.IndexByte("\"#<>'", c) >= 0
		})
	}
	return b.String()
}
