-- Generic per-iteration tool-output store. Replaces the narrowly-named
-- agent_task_fetched_docs (010) — agent_task is a universal primitive
-- (research, coding, business analysis, etc.) and any tool with a bulky
-- output that downstream gates / forensics want to read in full should
-- write to one place. Today: browser_fetch (research grounding). Tomorrow:
-- code_repo_read for code-review grounding, db_query for analytics audit,
-- file_read for doc-grounding. Same table, filter by tool_name.
--
-- This is a SHADOW-MODE drop. 010 shipped on 2026-05-13; on macstudio
-- the table had 4 rows from one world-models task and no enforcement
-- path depended on them — Gate C reads the verdict but doesn't block,
-- and the calibration window hasn't started accumulating data yet.
-- Dropping is cheaper than a data migration to the new schema.
DROP TABLE IF EXISTS blueship.agent_task_fetched_docs CASCADE;

CREATE TABLE IF NOT EXISTS blueship.agent_task_tool_outputs (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id       uuid        NOT NULL REFERENCES blueship.agent_tasks(id) ON DELETE CASCADE,
    iteration     integer     NOT NULL,

    -- Which tool produced this output. Used by gates to filter the store
    -- to their tool of interest (research grounding looks at 'browser_fetch',
    -- a future code-grounding gate at 'code_repo_read', etc.). Plain text
    -- rather than an enum so new tools don't require a migration.
    tool_name     text        NOT NULL,

    -- Raw input the tool was called with. JSONB so any tool's shape fits
    -- without a schema change. Browser fetch: {"url":"...","wait_ms":3000}.
    -- Code read: {"repo":"...","path":"...","range":[10,50]}.
    tool_input    jsonb       NOT NULL DEFAULT '{}'::jsonb,

    -- The bulky body that wouldn't fit in agent_task_iterations.tool_calls
    -- (which truncates output to 500 chars in agent.Loop). Page text for
    -- browser_fetch, source code for code_repo_read, CSV for db_query etc.
    output        text        NOT NULL,

    -- Free-text format hint so consumers know what kind of body they have.
    -- Browser fetch sets "html" / "pdf"; code tools "code"; analytics
    -- "csv" / "json". Not authoritative — consumers should still treat
    -- output as untrusted text.
    output_format text        NOT NULL,

    -- Per-tool typed extras. Browser fetch: {requested_url, final_url,
    -- title, page_count}. Code read: {line_count, language, sha}.
    -- DB query: {row_count, columns}. Whatever the gate needs to render
    -- and the tool doesn't want to bake into its own column.
    metadata      jsonb       NOT NULL DEFAULT '{}'::jsonb,

    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_atto_task
    ON blueship.agent_task_tool_outputs(task_id);
CREATE INDEX IF NOT EXISTS idx_atto_task_iter
    ON blueship.agent_task_tool_outputs(task_id, iteration);
CREATE INDEX IF NOT EXISTS idx_atto_task_tool
    ON blueship.agent_task_tool_outputs(task_id, tool_name);
