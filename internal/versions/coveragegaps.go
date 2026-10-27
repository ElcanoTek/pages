// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
)

// ── coverage gaps, per version ───────────────────────────────────────────────
//
// pagedata.DetectCoverageGaps finds the number fields that stopped for some
// entities while those entities' rows, and the same fields for other entities,
// carried on. A version's data never changes, so the answer is computed once
// and kept in page_versions.coverage_gaps (migration 029):
//
//   - on write: PrepareDeploy computes it from the document it already parsed,
//     and DeployLocked inserts it with the row. Every managed write (deploy,
//     data and config updates, template builds, uploads, deck saves) goes
//     through that pair;
//   - on read, for versions written before the column existed, or under an
//     older rule version: the health read computes it from the stored
//     document and saves it (best effort) for the next read. Only for pages
//     with a refresh schedule, the only ones judged, and at most
//     maxLazyCoverageGaps versions per read, so the first read after an
//     upgrade stays bounded; the rest are filled by the reads after it.
//
// Only the published version's gaps feed refresh health.

// maxLazyCoverageGaps bounds how many stored documents one read re-reads to
// compute gaps it has not stored yet. A near-limit document costs about 50 ms.
const maxLazyCoverageGaps = 24

// coverageGapsJSON is the stored form of a result.
func coverageGapsJSON(g *pagedata.CoverageGaps) string {
	b, err := json.Marshal(g)
	if err != nil {
		// Strings, ints and a bool: unreachable. Store nothing rather than fail
		// the write; a read recomputes it.
		return ""
	}
	return string(b)
}

// storedCoverageGaps decodes a stored result. ok is false when there is none,
// or when it was computed under another rule version and must be recomputed.
func storedCoverageGaps(raw []byte) (*pagedata.CoverageGaps, bool) {
	if raw == nil {
		return nil, false
	}
	var g pagedata.CoverageGaps
	if err := json.Unmarshal(raw, &g); err != nil || g.Version != pagedata.CoverageGapsVersion {
		return nil, false
	}
	if g.Gaps == nil {
		g.Gaps = []pagedata.CoverageGap{}
	}
	return &g, true
}

// storedGaps is one version's stored coverage_gaps column, as read.
type storedGaps struct {
	versionID jsx.ID
	raw       []byte
}

// resolveCoverageGaps returns the coverage gaps of each version: the stored
// result where there is a current one, else computed from the version's
// document, the first maxLazyCoverageGaps of those in the order given. The
// rest are left out (no finding on this read). What it computes, it saves
// through q, best effort.
func resolveCoverageGaps(ctx context.Context, q db.Querier, versions []storedGaps) (map[jsx.ID][]pagedata.CoverageGap, error) {
	out := make(map[jsx.ID][]pagedata.CoverageGap, len(versions))
	var missing []int64
	for _, v := range versions {
		if v.versionID == 0 {
			continue
		}
		if g, ok := storedCoverageGaps(v.raw); ok {
			out[v.versionID] = g.Gaps
		} else if len(missing) < maxLazyCoverageGaps {
			missing = append(missing, int64(v.versionID))
		}
	}
	computed, err := computeCoverageGaps(ctx, q, missing)
	if err != nil {
		return nil, err
	}
	for id, g := range computed {
		out[id] = g.Gaps
	}
	return out, nil
}

// computeCoverageGaps reads the listed versions' documents, computes their
// gaps and saves them.
func computeCoverageGaps(ctx context.Context, q db.Querier, ids []int64) (map[jsx.ID]*pagedata.CoverageGaps, error) {
	out := map[jsx.ID]*pagedata.CoverageGaps{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT id, html FROM page_versions WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id jsx.ID
		var html string
		if err := rows.Scan(&id, &html); err != nil {
			return nil, err
		}
		out[id] = pagedata.CoverageGapsOfHTML(html)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	saveCoverageGaps(ctx, q, out)
	return out, nil
}

// saveCoverageGaps stores computed results for the next read. Best effort: a
// read must never fail, or wait, because a cache could not be written. So the
// rows are claimed with SKIP LOCKED (a version a publish has locked is left
// for a later read), and inside a caller's transaction the write runs under a
// savepoint, so that a refusal (a read-only snapshot, say) leaves the
// transaction usable.
func saveCoverageGaps(ctx context.Context, q db.Querier, computed map[jsx.ID]*pagedata.CoverageGaps) {
	ids := make([]int64, 0, len(computed))
	docs := make([]string, 0, len(computed))
	for id, g := range computed {
		if doc := coverageGapsJSON(g); doc != "" {
			ids = append(ids, int64(id))
			docs = append(docs, doc)
		}
	}
	if len(ids) == 0 {
		return
	}
	const sql = `UPDATE page_versions pv SET coverage_gaps = f.gaps::jsonb
	   FROM unnest($1::bigint[], $2::text[]) AS f(id, gaps)
	  WHERE pv.id = f.id
	    AND pv.id IN (SELECT id FROM page_versions
	                   WHERE id = ANY($1::bigint[])
	                     AND (coverage_gaps IS NULL OR coverage_gaps->'v' IS DISTINCT FROM to_jsonb($3::int))
	                     FOR UPDATE SKIP LOCKED)`
	var err error
	if tx, ok := q.(pgx.Tx); ok {
		var sp pgx.Tx
		if sp, err = tx.Begin(ctx); err == nil {
			if _, err = sp.Exec(ctx, sql, ids, docs, pagedata.CoverageGapsVersion); err != nil {
				_ = sp.Rollback(ctx)
			} else {
				err = sp.Commit(ctx)
			}
		}
	} else {
		_, err = q.Exec(ctx, sql, ids, docs, pagedata.CoverageGapsVersion)
	}
	if err == nil {
		return
	}
	// A read-only snapshot and an update's drain are expected refusals: the
	// result is still used for this read, and a later one saves it.
	var pgErr *pgconn.PgError
	if (errors.As(err, &pgErr) && pgErr.Code == "25006") || isUpdateInProgress(err) {
		slog.Debug("versions: coverage gaps not saved", "versions", len(ids), "err", err)
		return
	}
	slog.Warn("versions: could not save coverage gaps", "versions", len(ids), "err", err)
}

func isUpdateInProgress(err error) bool {
	e, ok := apierror.As(err)
	return ok && e.Code == "update_in_progress"
}

// FillCoverageGaps computes and saves the coverage gaps of scheduled pages'
// published versions that have none stored yet (written before migration 029,
// or under an older rule version), at most maxLazyCoverageGaps per call. A health read does this
// for its own rows; a caller about to read inside a read-only snapshot (the
// admin index) calls it first, because the snapshot cannot save them.
func FillCoverageGaps(ctx context.Context, q db.Querier) error {
	rows, err := q.Query(ctx,
		`SELECT pv.id FROM pages p JOIN page_versions pv ON pv.id = p.published_version_id
		  WHERE p.deleted_at IS NULL AND p.refresh_schedule IS NOT NULL
		    AND (pv.coverage_gaps IS NULL OR pv.coverage_gaps->'v' IS DISTINCT FROM to_jsonb($1::int))
		  ORDER BY pv.id DESC LIMIT $2`, pagedata.CoverageGapsVersion, maxLazyCoverageGaps)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	_, err = computeCoverageGaps(ctx, q, ids)
	return err
}
