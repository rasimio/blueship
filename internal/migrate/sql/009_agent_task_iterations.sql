-- agent_task_iterations: append-only audit log for every iteration of
-- every agent_task. Until now the per-iteration "what did the handler
-- actually do" lived split across three places — agent_tasks.progress
-- (truncated to 500 chars), chat_messages (physically DELETE'd by the
-- compactor when token_count grew), and memory rows from AgentSaver
-- (atomic findings, no narrative). Operators investigating a failed or
-- long-running task had no clean place to read the full per-iteration
-- output back. This table fixes that: one row per iteration, written
-- exactly once after the scheduler decides the iteration's terminal
-- state, never updated.

CREATE TABLE IF NOT EXISTS blueship.agent_task_iterations (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id         uuid        NOT NULL REFERENCES blueship.agent_tasks(id) ON DELETE CASCADE,
    iteration       integer     NOT NULL,
    started_at      timestamptz NOT NULL,
    completed_at    timestamptz NOT NULL DEFAULT now(),
    duration_ms     integer,

    -- outcome buckets what the scheduler did with the iteration's result:
    --   'done'     — handler returned Done=true AND acceptance passed
    --                (or no criteria) → task completed
    --   'rejected' — handler returned Done=true but acceptance gate
    --                rejected; the draft is preserved here even though
    --                store.Complete didn't write it to agent_tasks.result
    --   'pause'    — handler returned Pause=true (e.g. waiting for A2A
    --                callback / structured plan wait step)
    --   'continue' — neither Done nor Pause, normal mid-task iteration
    --   'failed'   — handler.Run returned an error
    outcome         text        NOT NULL,
    is_final        boolean     NOT NULL DEFAULT false,
    acceptance_met  boolean,                      -- nil = no criteria / not evaluated this iter
    acceptance_reason text,                       -- evaluator's text when met=false

    output          text,                          -- full result.Output (handler's reply text)
    notify          text,                          -- result.Notify (may differ from output)
    tool_calls      jsonb       NOT NULL DEFAULT '[]'::jsonb,
                                                  -- [{name, input_size, output_size, is_error, duration_ms}, ...]
    progress        jsonb,                         -- handler progress snapshot at end of iter

    error           text,                          -- handler error message if outcome='failed'
    trace_id        text,                          -- OTLP trace id for cross-system lookup
    span_id         text,

    UNIQUE (task_id, iteration, started_at)
);

CREATE INDEX IF NOT EXISTS idx_agent_task_iterations_task
    ON blueship.agent_task_iterations(task_id, iteration);
CREATE INDEX IF NOT EXISTS idx_agent_task_iterations_outcome
    ON blueship.agent_task_iterations(outcome)
    WHERE outcome IN ('rejected', 'failed');
