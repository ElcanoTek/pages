// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package passwordgate is the progressive PER-PAGE backoff for the content
// host's password form (PLAN §7). The per-IP rate limit stops a single-source
// guessing run, but a distributed attacker gets a fresh budget per address.
// This brake is shared across every source IP: each failed unlock on a page
// delays the 401 a little more, so the total guess rate per page stays
// bounded no matter how many addresses attack (~10.8k guesses/day/page at the
// 8s ceiling, shared globally).
//
// Deliberately NOT a hard lockout: a hard per-page lock would let an attacker
// deny the page to its real viewers. A progressive delay keeps legitimate
// access cheap (one or two typos ≈ an extra half second) while making
// large-scale guessing impractical, and a successful unlock resets the page.
package passwordgate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ElcanoTek/pages/internal/db"
)

// Backoff curve constants.
const (
	// WindowMinutes: failures quiet for this long reset the counter.
	WindowMinutes = 15
	// BaseDelay is the delay after the first failed attempt.
	BaseDelay = 500 * time.Millisecond
	// MaxDelay is the ceiling — never harsher than this per attempt.
	MaxDelay = 8000 * time.Millisecond
)

// DelayForFailures is the pure backoff curve: 500ms, 1s, 2s, 4s, 8s, 8s, …
func DelayForFailures(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures > 5 {
		return MaxDelay
	}
	return min(MaxDelay, BaseDelay<<(failures-1))
}

const recordSQL = `INSERT INTO %[1]s (%[2]s, fail_count, window_start, last_fail_at)
     VALUES ($1, 1, now(), now())
     ON CONFLICT (%[2]s) DO UPDATE SET
       fail_count = CASE
         WHEN %[1]s.last_fail_at < now() - ($2 * interval '1 minute')
           THEN 1
         ELSE %[1]s.fail_count + 1
       END,
       window_start = CASE
         WHEN %[1]s.last_fail_at < now() - ($2 * interval '1 minute')
           THEN now()
         ELSE %[1]s.window_start
       END,
       last_fail_at = now()
     RETURNING fail_count`

var (
	recordPageSQL   = fmt.Sprintf(recordSQL, "page_password_failures", "page_id")
	recordPortalSQL = fmt.Sprintf(recordSQL, "portal_password_failures", "portal_id")
)

func record(ctx context.Context, q db.Querier, query string, id int64) (time.Duration, error) {
	var n int
	if err := q.QueryRow(ctx, query, id, WindowMinutes).Scan(&n); err != nil {
		return 0, err
	}
	return DelayForFailures(n), nil
}

// RecordFailure counts a failed unlock for the page (shared across all source
// IPs) and returns how long the caller should delay the 401. The counter
// resets when the page has been quiet for WindowMinutes.
func RecordFailure(ctx context.Context, q db.Querier, pageID int64) (time.Duration, error) {
	return record(ctx, q, recordPageSQL, pageID)
}

// ClearFailures resets the page after a successful unlock. Best-effort: a
// stale counter only means the next typo waits slightly longer, so it logs
// rather than failing.
func ClearFailures(ctx context.Context, q db.Querier, pageID int64) {
	if _, err := q.Exec(ctx, `DELETE FROM page_password_failures WHERE page_id = $1`, pageID); err != nil {
		slog.Error("passwordgate: failed to clear failures", "err", err)
	}
}

// ── portals ─────────────────────────────────────────────────────────────────
// Same curve, same window, same "delay the 401, never lock anyone out" rule,
// on its own counter (migrations/019). A portal password opens every dashboard
// in the portal, so it is worth more than any one page's password and gets its
// own budget rather than borrowing a member's.
//
// The counter is keyed by PORTAL, not by the URL the attempt arrived at, so
// every door which tests a portal password charges the same budget. When a
// member page's form also tries the portal passwords of the portals
// containing it, a per-door counter would hand an attacker N parallel budgets
// against a single secret; the caller applies max(pageDelay, portalDelay)
// when one submission tested both kinds of credential.

// RecordPortalFailure is RecordFailure for a portal's shared counter.
func RecordPortalFailure(ctx context.Context, q db.Querier, portalID int64) (time.Duration, error) {
	return record(ctx, q, recordPortalSQL, portalID)
}

// ClearPortalFailures is ClearFailures for a portal.
func ClearPortalFailures(ctx context.Context, q db.Querier, portalID int64) {
	if _, err := q.Exec(ctx, `DELETE FROM portal_password_failures WHERE portal_id = $1`, portalID); err != nil {
		slog.Error("passwordgate: failed to clear portal failures", "err", err)
	}
}
