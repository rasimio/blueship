CREATE TABLE IF NOT EXISTS system_prompts (
    key        TEXT PRIMARY KEY,
    content    TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO system_prompts (key) VALUES
    ('preamble'),
    ('soul'),
    ('agents'),
    ('heartbeat'),
    ('thinking')
ON CONFLICT (key) DO NOTHING;
