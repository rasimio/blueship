-- Role-based tool assignment: controls which tools each model role can use.
-- Empty role (no rows) = all tools (backwards-compatible for cloud models).
CREATE TABLE IF NOT EXISTS role_tools (
    role       TEXT NOT NULL,
    tool_name  TEXT NOT NULL,
    sort_order INT  DEFAULT 0,
    PRIMARY KEY (role, tool_name)
);

-- primary: chat tools (9) — optimized for small local models
INSERT INTO role_tools (role, tool_name, sort_order) VALUES
    ('primary', 'memory_save',      1),
    ('primary', 'memory_self_save',  2),
    ('primary', 'memory_search',     3),
    ('primary', 'memory_associate',  4),
    ('primary', 'memory_correct',    5),
    ('primary', 'tasks_list',        6),
    ('primary', 'tasks_create',      7),
    ('primary', 'tasks_update',      8),
    ('primary', 'deadlines_check',   9)
ON CONFLICT DO NOTHING;

-- background: heartbeat tools (12) — same as primary + monitoring
INSERT INTO role_tools (role, tool_name, sort_order) VALUES
    ('background', 'memory_save',          1),
    ('background', 'memory_self_save',     2),
    ('background', 'memory_search',        3),
    ('background', 'memory_associate',     4),
    ('background', 'memory_correct',       5),
    ('background', 'tasks_list',           6),
    ('background', 'tasks_create',         7),
    ('background', 'tasks_update',         8),
    ('background', 'deadlines_check',      9),
    ('background', 'deadlines_overdue',   10),
    ('background', 'memory_state_latest', 11),
    ('background', 'current_time',        12)
ON CONFLICT DO NOTHING;
