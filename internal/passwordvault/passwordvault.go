// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package passwordvault keeps the staff-readable copy of a client password.
//
// A page or portal password is stored twice:
//
//   - password_hash (scrypt, internal/pagecookie) is what the content host
//     checks a client's submission against. It is one-way and stays the only
//     thing the gate reads.
//   - password_ciphertext (this package) is AES-256-GCM, so Elcano staff can
//     show the current password in the admin shell and resend it without
//     rotating it and locking out everyone else at the client.
//
// Why this is safe to have:
//   - The key is derived from PAGE_COOKIE_SECRET (HKDF, its own label), which
//     lives in the server env and never in the database, so a database dump
//     alone opens nothing. A full `pages backup` archive carries the service
//     env beside the dump, so it can open every copy: handle it as a secret.
//   - With no PAGE_COOKIE_SECRET there is no copy: an empty key would be
//     public, so Seal returns "" and only the hash is kept.
//   - Each ciphertext is bound to the password_hash it was sealed with (GCM
//     additional data). Any path that changes the hash without resealing, such
//     as an older release during a rolling deploy, leaves a copy that no
//     longer opens, and the shell says "not available" rather than showing a
//     stale password.
//   - The only reader is the admin shell (Elcano SSO cookie + CSRF), and every
//     reveal writes an audit row. No bearer, MCP or content-host route returns
//     a password, so an agent can never learn which password opens which
//     client's dashboards.
//
// Passwords set before this existed have only a hash. RememberOnSignIn seals
// the copy the next time a client signs in with that password, so nothing has
// to be rotated to become visible.
//
// The sealed format and key derivation are lib/passwordvault.js's exactly, so
// copies sealed by the Node implementation open here and vice versa.
package passwordvault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"strings"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/nodebuf"
	"github.com/ElcanoTek/pages/internal/pagecookie"
)

const version = "v1"

const hkdfInfo = "pages:client-password-reveal:v1"

func key() []byte {
	s := env.String("PAGE_COOKIE_SECRET", "")
	if s == "" {
		return nil
	}
	k, err := hkdf.Key(sha256.New, []byte(s), nil, hkdfInfo, 32)
	if err != nil {
		return nil
	}
	return k
}

func aad(passwordHash string) []byte { return []byte("pages-password:" + version + ":" + passwordHash) }

func gcm(k []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts one password for the row whose password_hash is passwordHash.
// Format: v1.<iv>.<ciphertext>.<tag>, each base64url. It returns "" (store
// NULL) when there is no key or either input is empty.
func Seal(password, passwordHash string) string {
	k := key()
	if k == nil || password == "" || passwordHash == "" {
		return ""
	}
	aead, err := gcm(k)
	if err != nil {
		return ""
	}
	iv := make([]byte, aead.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return ""
	}
	out := aead.Seal(nil, iv, []byte(pagecookie.TruncatePassword(password)), aad(passwordHash))
	body, tag := out[:len(out)-aead.Overhead()], out[len(out)-aead.Overhead():]
	enc := base64.RawURLEncoding
	return strings.Join([]string{version, enc.EncodeToString(iv), enc.EncodeToString(body), enc.EncodeToString(tag)}, ".")
}

// Open returns the password, or ok=false when there is no copy, it was sealed
// for another hash, it was tampered with, or the key has changed since.
func Open(sealed, passwordHash string) (string, bool) {
	k := key()
	if k == nil || passwordHash == "" {
		return "", false
	}
	parts := strings.Split(sealed, ".")
	if len(parts) != 4 || parts[0] != version {
		return "", false
	}
	aead, err := gcm(k)
	if err != nil {
		return "", false
	}
	iv := nodebuf.Base64Decode(parts[1])
	tag := nodebuf.Base64Decode(parts[3])
	// Node would also have accepted a truncated tag; a full 16-byte tag (what
	// every seal wrote) is required here.
	if len(iv) != aead.NonceSize() || len(tag) != aead.Overhead() {
		return "", false
	}
	plain, err := aead.Open(nil, iv, append(nodebuf.Base64Decode(parts[2]), tag...), aad(passwordHash))
	if err != nil {
		return "", false
	}
	return strings.ToValidUTF8(string(plain), "�"), true
}

// Kind selects the table RememberOnSignIn writes.
type Kind string

// Kinds.
const (
	Page   Kind = "page"
	Portal Kind = "portal"
)

var tables = map[Kind]string{Page: "pages", Portal: "page_portals"}

// RememberOnSignIn — after a client's password VERIFIED against passwordHash
// on the content host, keep a staff-readable copy if the row has none that
// opens. The UPDATE is conditional on the same hash, so a rotation that
// commits in between is never overwritten with the old password. Best effort:
// a failure here must never turn a correct password into an error page, so it
// logs and returns nothing.
func RememberOnSignIn(ctx context.Context, q db.Querier, kind Kind, id int64, passwordHash, password string) {
	table, ok := tables[kind]
	if key() == nil || !ok || passwordHash == "" || password == "" {
		return
	}
	plain := pagecookie.TruncatePassword(password)
	var current *string
	err := q.QueryRow(ctx, `SELECT password_ciphertext FROM `+table+` WHERE id = $1 AND password_hash = $2`, id, passwordHash).Scan(&current)
	if db.IsNoRows(err) {
		return
	}
	if err == nil {
		if current != nil {
			if got, ok := Open(*current, passwordHash); ok && got == plain {
				return
			}
		}
		var sealed any
		if s := Seal(plain, passwordHash); s != "" {
			sealed = s
		}
		_, err = q.Exec(ctx, `UPDATE `+table+` SET password_ciphertext = $3 WHERE id = $1 AND password_hash = $2`, id, passwordHash, sealed)
	}
	if err != nil {
		slog.Error("passwordvault: could not keep the "+string(kind)+" password copy", "err", err)
	}
}
