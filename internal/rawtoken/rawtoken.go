// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package rawtoken mints and verifies the signed, short-TTL access tokens for
// the cookieless content host (PLAN.md §6d, §7). The token is the ONLY thing
// that authorizes anything on that domain — there are no cookies there.
// HMAC-SHA256 with RAW_TOKEN_SECRET; constant-time verify.
//
//	token  = base64url(JSON(claims)) + "." + base64url(HMAC(secret, body))
//	claims = { pid, vid, purpose, mode, sid, exp[, actor] }
//
// The format is the one lib/rawtoken.js used, so a token either
// implementation minted verifies in the other during a rolling deploy.
package rawtoken

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/nodebuf"
)

// Purposes. `purpose` is the AUDIENCE of a token, and each one is consumed by
// exactly one route that allow-lists it. Keeping them disjoint is what stops a
// credential minted for a narrow job from being redeemed for a wider one.
const (
	// View renders one page version at /raw. Minted by the /admin preview
	// endpoint for ANY version, including the published one. Read-only, one
	// version, no session.
	View = "view"
	// Template renders one template revision at /raw-template. A template
	// revision id and a page version id are different id spaces, so a token
	// bound to one must be unusable against the other.
	Template = "template"
	// Session is exchanged for a page-session cookie on the content host.
	// Minted ONLY by the dashboard's /view broker, after Elcano-staff SSO. It
	// is the one purpose that is a *page* credential rather than a *render*
	// credential, which is why it is separate: an /admin preview URL is
	// exactly the kind of link that gets pasted into a chat, and it must never
	// buy an hour of access to the live page.
	Session = "session"
	// Edit opens ONE deck page in its own editor at /raw with a save channel
	// back to Pages, and authorises the saves. Minted only by the admin (staff
	// SSO) for a page whose current version is a Bento deck; bound to the
	// page, not a version, because every save makes a new one; carries the
	// staff actor so the audit row names a person. It buys the right to add
	// DRAFT versions to that page for its TTL and nothing else.
	Edit = "edit"
)

func validPurpose(p string) bool {
	switch p {
	case View, Template, Session, Edit:
		return true
	}
	return false
}

// DefaultTTL is mint's default lifetime in seconds.
const DefaultTTL = 300

func secret() string { return env.String("RAW_TOKEN_SECRET", "") }

// Validate fails CLOSED in production: an empty key lets anyone forge a /raw
// view token. Mint already refuses without a secret; the server calls this at
// boot so production refuses to start instead.
func Validate() error {
	if secret() == "" && env.Production() {
		return errors.New("RAW_TOKEN_SECRET is required in production (an empty key lets anyone forge /raw access tokens)")
	}
	return nil
}

// Enabled reports whether a RAW_TOKEN_SECRET is configured.
func Enabled() bool { return secret() != "" }

func sign(key, body string) []byte {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(body))
	return m.Sum(nil)
}

// MintParams are the claims a caller binds into a token.
type MintParams struct {
	PageID     int64
	VersionID  int64
	Purpose    string
	RenderMode string
	// SID is the session id; a random one is generated when empty.
	SID string
	// Actor is required for Edit (the audit row of every save names it) and
	// refused for every other purpose, so a render credential can never carry
	// an identity it has no use for.
	Actor string
}

// Mint signs a token valid for ttlSeconds (DefaultTTL is the JS default).
func Mint(p MintParams, ttlSeconds int) (string, error) {
	key := secret()
	if key == "" {
		return "", errors.New("RAW_TOKEN_SECRET is not set")
	}
	if !validPurpose(p.Purpose) {
		return "", fmt.Errorf("bad purpose: %s", p.Purpose)
	}
	if p.Purpose == Edit && p.Actor == "" {
		return "", errors.New("an edit token must name its actor")
	}
	if p.Purpose != Edit && p.Actor != "" {
		return "", fmt.Errorf("a %s token carries no actor", p.Purpose)
	}
	sid := p.SID
	if sid == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		sid = hex.EncodeToString(b[:])
	}
	// Field order is the JS object's: pid, vid, purpose, mode, sid, exp, actor.
	var buf bytes.Buffer
	buf.WriteString(`{"pid":` + strconv.FormatInt(p.PageID, 10))
	buf.WriteString(`,"vid":` + strconv.FormatInt(p.VersionID, 10))
	buf.WriteString(`,"purpose":` + jsonv.Quote(p.Purpose))
	if p.RenderMode != "" {
		buf.WriteString(`,"mode":` + jsonv.Quote(p.RenderMode))
	}
	buf.WriteString(`,"sid":` + jsonv.Quote(sid))
	buf.WriteString(`,"exp":` + strconv.FormatInt(time.Now().Unix()+int64(ttlSeconds), 10))
	if p.Actor != "" {
		buf.WriteString(`,"actor":` + jsonv.Quote(p.Actor))
	}
	buf.WriteByte('}')
	body := base64.RawURLEncoding.EncodeToString(buf.Bytes())
	return body + "." + base64.RawURLEncoding.EncodeToString(sign(key, body)), nil
}

// Claims is a verified token's payload.
type Claims struct {
	// PageID and VersionID are the bound ids. The Node implementation minted
	// page ids as JSON strings (BIGINT from node-postgres) and template
	// revision ids as numbers; both decode here.
	PageID    int64
	VersionID int64
	Purpose   string
	Mode      string
	SID       string
	Exp       int64
	// Actor is set only on Edit tokens.
	Actor string
}

// Verify returns the claims of a valid token, or nil. It checks the signature
// in constant time and rejects expired tokens and unknown purposes.
func Verify(token string) *Claims {
	key := secret()
	if key == "" {
		return nil
	}
	dot := strings.IndexByte(token, '.')
	if dot < 1 || dot == len(token)-1 {
		return nil
	}
	body := token[:dot]
	given := nodebuf.Base64Decode(token[dot+1:])
	if !hmac.Equal(given, sign(key, body)) {
		return nil
	}
	var raw struct {
		PID     json.RawMessage `json:"pid"`
		VID     json.RawMessage `json:"vid"`
		Purpose any             `json:"purpose"`
		Mode    any             `json:"mode"`
		SID     any             `json:"sid"`
		Exp     any             `json:"exp"`
		Actor   any             `json:"actor"`
	}
	if err := json.Unmarshal(nodebuf.Base64Decode(body), &raw); err != nil {
		return nil
	}
	purpose, _ := raw.Purpose.(string)
	if !validPurpose(purpose) {
		return nil
	}
	exp, ok := raw.Exp.(float64)
	if !ok || exp <= float64(time.Now().Unix()) {
		return nil
	}
	c := &Claims{Purpose: purpose, Exp: int64(exp)}
	c.PageID = idOf(raw.PID)
	c.VersionID = idOf(raw.VID)
	c.Mode, _ = raw.Mode.(string)
	c.SID, _ = raw.SID.(string)
	c.Actor, _ = raw.Actor.(string)
	return c
}

// idOf reads an id claim that is a JSON number or a numeric string, as
// JavaScript's Number() would for the values Pages mints. Anything else is 0,
// which never matches a real row (ids start at 1).
func idOf(m json.RawMessage) int64 {
	var v any
	if json.Unmarshal(m, &v) != nil {
		return 0
	}
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		f = n
	default:
		return 0
	}
	if f != math.Trunc(f) || f < 0 || f > 1<<53 {
		return 0
	}
	return int64(f)
}
