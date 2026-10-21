// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package versions is the version mutation state machine (PLAN.md §5). REST,
// MCP and the admin UI all route every page/version change through it; there
// is no privileged backdoor that skips it.
//
// "Live" is pointer-is-truth: pages.published_version_id is the ONLY truth for
// what a viewer gets. Content rows (page_versions) are append-only — every
// deploy/edit is a new row; only status/reviewed_* ever mutate (a DB trigger
// enforces the rest). Publish, approve and rollback each move the pointer in a
// single transaction, and the pointer may only ever reference an `approved`
// version.
//
// Every mutation:
//  1. runs in a transaction whose first statement is SELECT … FROM pages …
//     FOR UPDATE (LockPage), serializing all writers for that page;
//  2. honors optimistic concurrency: an expected version that is not the
//     page's current published_version_id is a 409 stale_version;
//  3. writes its audit_log row in the SAME transaction.
//
// Agent authority depends on the page's approval gate: on an open page an
// agent may deploy, publish, roll back and delete; on an approval-gated page it
// may only create `pending` versions. Approve/reject are always admin-only,
// and agents cannot publish, roll back or delete a page an admin disabled.
//
// Errors are *apierror.Error with the JavaScript implementation's exact
// messages, codes and statuses; anything else (including pgx errors — pass
// them through apierror.FromDB for lock/statement timeouts) is unexpected.
package versions
