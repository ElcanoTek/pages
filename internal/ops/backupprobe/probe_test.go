// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backupprobe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func decode(t *testing.T, token string) (body []byte, signed, sig string) {
	t.Helper()
	signed, sig, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatal(token)
	}
	b, err := base64.RawURLEncoding.DecodeString(signed)
	if err != nil {
		t.Fatal(err)
	}
	return b, signed, sig
}

func mac(secret, text string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(text))
	return m.Sum(nil)
}

// The minters reproduce lib/rawtoken.js and lib/pagecookie.js exactly: key
// order, id types and the HMAC inputs are a compatibility contract.
func TestMintersMatchTheWireFormats(t *testing.T) {
	t.Setenv("RAW_TOKEN_SECRET", "northwind-raw")
	t.Setenv("PAGE_COOKIE_SECRET", "northwind-cookie")
	tok, err := MintRawToken("12", "34", "view", "themed")
	if err != nil {
		t.Fatal(err)
	}
	body, signed, sig := decode(t, tok)
	if !regexp.MustCompile(`^\{"pid":"12","vid":"34","purpose":"view","mode":"themed","sid":"[0-9a-f]{16}","exp":\d+\}$`).Match(body) {
		t.Fatal(string(body))
	}
	if sig != base64.RawURLEncoding.EncodeToString(mac("northwind-raw", signed)) {
		t.Fatal("raw token signature")
	}
	if tok, _ := MintRawToken(0, "7", "template", "themed"); !strings.HasPrefix(string(func() []byte { b, _, _ := decode(t, tok); return b }()), `{"pid":0,"vid":"7",`) {
		t.Fatal("template tokens carry pid 0 as a number")
	}
	hash := "scrypt$16384$8$1$c2FsdA==$aGFzaA=="
	body, signed, sig = decode(t, MintPageSession(12, 60, &hash))
	cd := hex.EncodeToString(mac("northwind-cookie", "cred:"+hash))[:16]
	if !regexp.MustCompile(`^\{"pid":12,"exp":\d+,"cd":"`+cd+`"\}$`).Match(body) || sig != base64.RawURLEncoding.EncodeToString(mac("northwind-cookie", signed)) {
		t.Fatal(string(body))
	}
	body, signed, sig = decode(t, MintPortalSession(5, 60, &hash))
	cd = hex.EncodeToString(mac("northwind-cookie", "cred:portal:"+hash))[:16]
	if !regexp.MustCompile(`^\{"poid":5,"exp":\d+,"cd":"`+cd+`"\}$`).Match(body) || sig != base64.RawURLEncoding.EncodeToString(mac("northwind-cookie", "portal."+signed)) {
		t.Fatal(string(body))
	}
	var counts Counts
	counts = append(counts, struct{ Table, Count string }{"pages", "2"}, struct{ Table, Count string }{"assets", "1"})
	if b, _ := json.Marshal(counts); string(b) != `{"pages":"2","assets":"1"}` {
		t.Fatal(string(b))
	}
}
