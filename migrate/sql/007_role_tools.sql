-- Restore role_tools as a DB-driven config table.
--
-- Migration 004 dropped this table when role allowlists moved into agent
-- code. That decision is being reversed: agent-specific role lists do
-- not belong in the framework's Go map, and adding/removing tools per
-- role should be a DB UPDATE rather than a redeploy. Tool *descriptions*
-- and *implementations* still live in code (`reg.Register(...)`); only
-- the role→tool allowlist comes from this table.
--
-- Roles without a row default to "no allowlist" — handlers see every
-- registered tool. Seed rows here cover Arlene's current defaults; the
-- host can UPDATE / INSERT freely after deploy.

CREATE TABLE IF NOT EXISTS role_tools (
    role       TEXT PRIMARY KEY,
    tools      TEXT[] NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO role_tools (role, tools) VALUES
    ('background', ARRAY[
        'memory_search',
        'memory_update',
        'memory_associate',
        'temporal_recall',
        'agent_task_list',
        'browser_fetch',
        'trace_recall',
        'web_search',
        'current_time',
        'notes'
    ]),
    ('control_dump', ARRAY[
        'telegram_call',
        'telegram_message_read',
        'telegram_message_send'
    ]),
    ('cortex', ARRAY[
        'agent_task_create',
        'agent_task_approve',
        'agent_task_list',
        'agent_task_status',
        'agent_task_cancel',
        'peer:liya',
        'note_close',
        'memory_search',
        'memory_update',
        'memory_associate',
        'web_search',
        'current_time',
        'message_send',
        'temporal_recall',
        'notes',
        'browser_fetch'
    ])
ON CONFLICT (role) DO NOTHING;
