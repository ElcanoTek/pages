-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 021_data_upload_target.sql — let the staged upload machinery carry managed
-- DATA, not only page HTML and template HTML.
--
-- HTML has had a path where the bytes never enter the model's context since
-- 014: create_upload_ticket → shell PUT → deploy_page_upload. Managed data — which
-- is generated on disk by run_python and is routinely LARGER than the HTML —
-- could only ever travel inline, as a tool argument. On 2026-08-17 the largest
-- live payload was 927 KB and its next refresh built 978 KB, five days apart;
-- #126 set the inline ceiling at 1.5 MB because past 2 MB express destroys the
-- connection rather than returning a diagnosable error. Raising the ceiling
-- moves that cliff without removing it.
--
-- The gap was already visible to callers: the run that built the 978 KB payload
-- spent six tool searches hunting for update_page_data_from_file, page_data_file,
-- arguments_file, an upload_id parameter — found none, flagged
-- file_backed_update_tool_unavailable, and aborted with the page unchanged
-- (fleet task 3d767956).
--
-- Widening this CHECK is the whole storage change. Keeping 'data' as a distinct
-- kind rather than reusing 'page' is what keeps assertTarget honest: JSON staged
-- for a data refresh must never be deployable as page HTML, and a document
-- staged as a page must never be parseable into a data envelope.

ALTER TABLE page_content_uploads
  DROP CONSTRAINT IF EXISTS page_content_uploads_target_kind_check;

ALTER TABLE page_content_uploads
  ADD CONSTRAINT page_content_uploads_target_kind_check
    CHECK (target_kind IN ('page', 'template', 'data'));
