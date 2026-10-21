// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package central

import (
	"context"
	"crypto/rand"
	"encoding/base64"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/db"
)

// Store is the central-mode state in Postgres (migration 025): the access
// list, the hashed sessions, back-channel replay protection and the last
// applied access event per subject. Times are Unix seconds, matching the
// claims they are compared with.
type Store struct {
	DB *db.DB
	// Now is the clock in Unix seconds, NowMs in milliseconds (defaults: wall
	// clock). Tests move them.
	Now   func() int64
	NowMs func() int64
}

// NewStore wraps d.
func NewStore(d *db.DB) *Store { return &Store{DB: d, Now: now, NowMs: nowMs} }

func (s *Store) now() int64 {
	if s.Now != nil {
		return s.Now()
	}
	return now()
}

func (s *Store) nowMs() int64 {
	if s.NowMs != nil {
		return s.NowMs()
	}
	return nowMs()
}

// lockSubject serializes session issuance and back-channel logout for one
// Auth subject (transaction-scoped).
func lockSubject(ctx context.Context, tx pgx.Tx, subject string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "pages-central-subject|"+subject)
	return err
}

// GrantAccess enables email (normalized) and returns it.
func (s *Store) GrantAccess(ctx context.Context, rawEmail string) (string, error) {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return "", err
	}
	_, err = s.DB.Exec(ctx,
		`INSERT INTO central_access (email, enabled) VALUES ($1, TRUE)
		 ON CONFLICT (email) DO UPDATE SET enabled = TRUE, updated_at = now()`, email)
	if err != nil {
		return "", err
	}
	return email, nil
}

// RevokeAccess disables the email and ends its sessions in one transaction.
// It reports whether the email had active access.
func (s *Store) RevokeAccess(ctx context.Context, rawEmail string) (bool, error) {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return false, err
	}
	at := s.now()
	return db.WithTxResult(ctx, s.DB, func(tx pgx.Tx) (bool, error) {
		tag, err := tx.Exec(ctx, `UPDATE central_access SET enabled = FALSE, updated_at = now() WHERE email = $1 AND enabled`, email)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE central_sessions SET revoked_at = $2 WHERE email = $1 AND revoked_at IS NULL`, email, at); err != nil {
			return false, err
		}
		return tag.RowsAffected() > 0, nil
	})
}

// ListAccess is every enabled email, sorted.
func (s *Store) ListAccess(ctx context.Context) ([]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT email FROM central_access WHERE enabled ORDER BY email`)
	if err != nil {
		return nil, err
	}
	emails, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if emails == nil {
		emails = []string{}
	}
	return emails, nil
}

// CreateSession mints a session for a proven identity and returns the
// cookie value. The local access decision and the session insert commit
// together: FOR SHARE makes a concurrent revoke wait for this insert, so its
// session sweep cannot miss the new row.
//
// exchangeStartedMs (Unix milliseconds) closes the logout race: Auth refuses
// a code once the central session is revoked, so the only gap is a logout
// that Pages receives after /token succeeded but before this insert. The
// per-subject lock (shared with ConsumeLogoutEvent) orders the two; if a
// logout for the subject arrived since the exchange began, the sign-in is
// refused (ErrAuthTransaction) and the person simply signs in again.
func (s *Store) CreateSession(ctx context.Context, subject, rawEmail string, exchangeStartedMs int64) (string, error) {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return "", err
	}
	if subject == "" || wire16Len(subject) > 255 {
		return "", newErr(nil, "the authenticated subject was invalid")
	}
	at := s.now()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	absolute := at + AbsoluteSeconds
	idle := min(at+IdleSeconds, absolute)
	// Validation only marks rows revoked; a login is the cheap moment to drop
	// rows that can never validate again. Its own statement, outside the
	// transaction below: holding these row locks while waiting for the
	// subject lock would deadlock against a logout sweeping the same rows.
	if _, err := s.DB.Exec(ctx, `DELETE FROM central_sessions WHERE revoked_at IS NOT NULL OR absolute_expires_at <= $1`, at); err != nil {
		return "", err
	}
	err = s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockSubject(ctx, tx, subject); err != nil {
			return err
		}
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM central_revocation_events WHERE subject = $1 AND received_ms >= $2 LIMIT 1`, subject, exchangeStartedMs).Scan(&one)
		if err == nil {
			return newErr(ErrAuthTransaction, "signed out during sign-in")
		}
		if !db.IsNoRows(err) {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT 1 FROM central_access WHERE email = $1 AND enabled FOR SHARE`, email).Scan(&one)
		if db.IsNoRows(err) {
			return newErr(ErrAccessDenied, "this account does not have access to Pages")
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO central_sessions
			   (token_hash, subject, email, created_at, last_seen_at, idle_expires_at, absolute_expires_at)
			 VALUES ($1, $2, $3, $4, $4, $5, $6)`,
			sha256Hex(token), subject, email, at, idle, absolute)
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// Identity is the signed-in person behind a live session.
type Identity struct {
	Subject string
	Email   string
}

// Identity resolves a session cookie value now; nil for anything that is not
// a live session whose email is still enabled.
func (s *Store) Identity(ctx context.Context, token string) (*Identity, error) {
	return s.IdentityAt(ctx, token, s.now())
}

// IdentityAt is Identity at Unix time at. A plain read on most requests; the
// write happens at most once a minute.
func (s *Store) IdentityAt(ctx context.Context, token string, at int64) (*Identity, error) {
	if token == "" || wire16Len(token) > 128 {
		return nil, nil
	}
	hash := sha256Hex(token)
	var (
		id                      Identity
		lastSeen, idleAt, absAt int64
		enabled                 bool
	)
	err := s.DB.QueryRow(ctx,
		`SELECT s.subject, s.email, s.last_seen_at, s.idle_expires_at, s.absolute_expires_at, a.enabled
		   FROM central_sessions s
		   JOIN central_access a ON a.email = s.email
		  WHERE s.token_hash = $1 AND s.revoked_at IS NULL`, hash).
		Scan(&id.Subject, &id.Email, &lastSeen, &idleAt, &absAt, &enabled)
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !enabled || at >= idleAt || at >= absAt {
		if _, err := s.DB.Exec(ctx, `UPDATE central_sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`, hash, at); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if at-lastSeen >= SessionTouchSeconds {
		// Conditional on the row still being stale, so a burst of concurrent
		// requests writes once and a slow request never moves the clock back.
		if _, err := s.DB.Exec(ctx,
			`UPDATE central_sessions SET last_seen_at = $2, idle_expires_at = $3
			  WHERE token_hash = $1 AND revoked_at IS NULL AND last_seen_at <= $4`,
			hash, at, min(at+IdleSeconds, absAt), at-SessionTouchSeconds); err != nil {
			return nil, err
		}
	}
	return &id, nil
}

// RevokeSession ends one session; it reports whether a live one was ended.
func (s *Store) RevokeSession(ctx context.Context, token string) (bool, error) {
	if token == "" || wire16Len(token) > 128 {
		return false, nil
	}
	tag, err := s.DB.Exec(ctx, `UPDATE central_sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`, sha256Hex(token), s.now())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ConsumeLogoutEvent is idempotent by jti; it ends every session of the
// subject and reports whether the event was new.
func (s *Store) ConsumeLogoutEvent(ctx context.Context, ev LogoutEvent) (bool, error) {
	at, receivedMs := s.now(), s.nowMs()
	return db.WithTxResult(ctx, s.DB, func(tx pgx.Tx) (bool, error) {
		if err := lockSubject(ctx, tx, ev.Subject); err != nil {
			return false, err
		}
		// Replay protection only has to outlive a token's acceptance window;
		// keep a week for forensics.
		if _, err := tx.Exec(ctx, `DELETE FROM central_revocation_events WHERE received_at < $1`, at-RevocationEventRetentionSeconds); err != nil {
			return false, err
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO central_revocation_events (event_id, issuer, subject, issued_at, received_at, received_ms)
			 VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (event_id) DO NOTHING`,
			ev.EventID, ev.Issuer, ev.Subject, ev.IssuedAt, at, receivedMs)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() > 0 {
			if _, err := tx.Exec(ctx, `UPDATE central_sessions SET revoked_at = $2 WHERE subject = $1 AND revoked_at IS NULL`, ev.Subject, at); err != nil {
				return false, err
			}
		}
		return tag.RowsAffected() > 0, nil
	})
}

// ApplyAccessEvent applies Auth's desired state for one subject. Ordered by
// version per issuer+subject; the same or an older version is ignored
// (false). A changed email disables the old address. Revoke ends the
// sessions.
func (s *Store) ApplyAccessEvent(ctx context.Context, ev AccessEvent) (bool, error) {
	at := s.now()
	return db.WithTxResult(ctx, s.DB, func(tx pgx.Tx) (bool, error) {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "pages-central-access|"+ev.Issuer+"|"+ev.Subject); err != nil {
			return false, err
		}
		var curEmail string
		var curVersion int64
		err := tx.QueryRow(ctx, `SELECT email, version FROM central_access_provisioning WHERE issuer = $1 AND subject = $2`, ev.Issuer, ev.Subject).
			Scan(&curEmail, &curVersion)
		found := err == nil
		if err != nil && !db.IsNoRows(err) {
			return false, err
		}
		if found && curVersion >= ev.Version {
			return false, nil
		}
		if found && curEmail != ev.Email {
			if _, err := tx.Exec(ctx, `UPDATE central_access SET enabled = FALSE, updated_at = now() WHERE email = $1`, curEmail); err != nil {
				return false, err
			}
			if _, err := tx.Exec(ctx, `UPDATE central_sessions SET revoked_at = $2 WHERE email = $1 AND revoked_at IS NULL`, curEmail, at); err != nil {
				return false, err
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO central_access (email, enabled) VALUES ($1, $2)
			 ON CONFLICT (email) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`, ev.Email, ev.Allowed); err != nil {
			return false, err
		}
		if !ev.Allowed {
			if _, err := tx.Exec(ctx, `UPDATE central_sessions SET revoked_at = $2 WHERE email = $1 AND revoked_at IS NULL`, ev.Email, at); err != nil {
				return false, err
			}
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO central_access_provisioning (issuer, subject, email, version, allowed, event_id, issued_at, received_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (issuer, subject) DO UPDATE SET
			   email = EXCLUDED.email, version = EXCLUDED.version, allowed = EXCLUDED.allowed,
			   event_id = EXCLUDED.event_id, issued_at = EXCLUDED.issued_at, received_at = EXCLUDED.received_at`,
			ev.Issuer, ev.Subject, ev.Email, ev.Version, ev.Allowed, ev.EventID, ev.IssuedAt, at)
		if err != nil {
			return false, err
		}
		return true, nil
	})
}
