CREATE TABLE IF NOT EXISTS agent_task_deliveries (
    task_id      UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    input_id     TEXT NOT NULL,
    item_key     TEXT NOT NULL CHECK (octet_length(item_key) <= 512),
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (task_id, input_id, item_key)
);
