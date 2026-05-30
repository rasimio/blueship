-- Phase 10 retirement.
--
-- Tools, system prompts, and role allowlists moved from DB into agent
-- code (file-backed prompt store + inline tool descriptions + Go map for
-- role_tools). The tables that backed those layers are now dead weight;
-- drop them. This migration is intentionally one-way — there is no
-- rollback path because the data they held is now committed in the
-- agent repos.
--
-- Migration 002_goal_tool_descriptions.sql seeded rows into the `tools`
-- table; that seed becomes a no-op once the table is gone. Keeping that
-- file in place is harmless because the migration runner records each
-- file by name once and never re-applies.

DROP TABLE IF EXISTS tools;
DROP TABLE IF EXISTS system_prompts;
DROP TABLE IF EXISTS role_tools;
