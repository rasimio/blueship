-- Reversal of migration 007.
--
-- Migration 007 briefly resurrected `role_tools` as a DB-driven config
-- table. We reverted that decision the same day — agent role allowlists
-- belong in code (reproducibility, PR review, no separate seed step on
-- fresh DBs). Drop the table on every DB where 007 already ran;
-- migration 007's file has been removed so fresh DBs never see it.
--
-- Yes, this means the migration ledger keeps both 007 (applied) and
-- 008 (applied) markers but the source-of-truth file for 007 is gone.
-- The migration runner is name-keyed, so a missing file for an already-
-- applied version is a no-op. Future devs reading git history can see
-- the full arc in commits.

DROP TABLE IF EXISTS role_tools;
