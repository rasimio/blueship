-- Claim-level grounding gate (Gate C). The cross-reference gate added
-- in evaluator.go already proves "this URL was actually opened during
-- the task" but cannot tell whether the report's claim is supported by
-- the page's content — the model can fetch arxiv.org/abs/X (gets a
-- ~200-word abstract) and then write four paragraphs of paper details
-- from training data. Gate C closes that loop: a separate audit LLM
-- inspects every claim against the full fetched text.
--
-- The 500-char ToolTrace truncation in agent.Loop strips fetched body
-- before the evaluator can see it, so the document text is persisted
-- into its own table at fetch time. The browser_fetch tool reads task_id
-- and iteration out of the request context and writes here directly.

CREATE TABLE IF NOT EXISTS blueship.agent_task_fetched_docs (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id       uuid        NOT NULL REFERENCES blueship.agent_tasks(id) ON DELETE CASCADE,
    iteration     integer     NOT NULL,
    requested_url text        NOT NULL,                       -- URL the agent asked for (pre-rewrite)
    final_url     text        NOT NULL,                       -- URL after redirects + abstract→PDF rewrite
    title         text,
    text          text        NOT NULL,
    source_kind   text        NOT NULL,                       -- 'html' | 'pdf'
    page_count    integer,
    fetched_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_atfd_task
    ON blueship.agent_task_fetched_docs(task_id);
CREATE INDEX IF NOT EXISTS idx_atfd_task_iter
    ON blueship.agent_task_fetched_docs(task_id, iteration);

-- Three new audit columns on the iteration log. Nullable so iterations
-- where grounding wasn't run (Gate A/B reject, handler error, non-final
-- iter) remain valid rows.
ALTER TABLE blueship.agent_task_iterations
    ADD COLUMN IF NOT EXISTS grounded_count    integer,
    ADD COLUMN IF NOT EXISTS ungrounded_count  integer,
    ADD COLUMN IF NOT EXISTS grounding_verdict jsonb;

-- Recheck list set by Gate C on reject. The next iteration's evaluator
-- (Gate B') verifies these URLs were re-fetched IN THAT iteration before
-- accepting another submit. Without this guard, cortex would just
-- s/Zhang/Xiong/ in the report on the next iter and Gate C would pass
-- — the corrected name matches a fetched doc, but no re-verification
-- actually happened. Persisted on agent_tasks so it survives scheduler
-- restarts between iterations.
ALTER TABLE blueship.agent_tasks
    ADD COLUMN IF NOT EXISTS required_recheck_urls text[] NOT NULL DEFAULT '{}';

-- Audit model for Gate C. Sonnet 4.6 (200K context, deterministic at
-- temperature 0.2). 8K output covers a long claims-array JSON.
-- ON CONFLICT keeps any operator-set override.
INSERT INTO blueship.model_config (role, provider, model_name, max_tokens, thinking_budget, temperature)
VALUES ('grounding_evaluator', 'anthropic', 'claude-sonnet-4-6', 8192, 0, 0.2)
ON CONFLICT (role) DO NOTHING;
