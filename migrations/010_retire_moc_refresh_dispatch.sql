-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- Pages no longer owns recurring schedules or dispatches work through MOC.
-- Preserve the historical definitions/runs for auditability, but make every
-- legacy definition inert and remove grants Pages added for its old runner.

UPDATE page_refreshes
   SET status = 'paused',
       last_error = 'Pages-owned dispatch retired; use prepare_dashboard_update and a user-owned scheduler.',
       updated_at = now()
 WHERE status = 'active';

UPDATE page_refresh_runs
   SET status = 'cancelled',
       dispatch_lease_until = NULL,
       last_error = COALESCE(last_error, 'Pages-owned dispatch retired before this run was submitted.'),
       updated_at = now()
 WHERE status IN ('queued', 'dispatching', 'error');

DELETE FROM api_token_page_grants AS grant_row
 USING page_refreshes AS refresh, pages AS page
 WHERE grant_row.token_id = refresh.runtime_token_id
   AND page.id = refresh.page_id
   AND grant_row.slug = page.slug;
