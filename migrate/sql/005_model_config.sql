-- Available models registry
CREATE TABLE IF NOT EXISTS available_models (
    provider    TEXT NOT NULL,
    name        TEXT NOT NULL,
    display_name TEXT,
    capabilities TEXT[] DEFAULT '{}',
    PRIMARY KEY (provider, name)
);

-- Current model assignments (role → model)
CREATE TABLE IF NOT EXISTS model_config (
    role        TEXT PRIMARY KEY,
    provider    TEXT NOT NULL,
    model_name  TEXT NOT NULL,
    updated_at  TIMESTAMPTZ DEFAULT NOW()
);

-- Seed available models
INSERT INTO available_models (provider, name, display_name, capabilities) VALUES
    ('anthropic', 'claude-opus-4-6',             'Claude Opus 4.6',   '{primary,background,compact}'),
    ('anthropic', 'claude-sonnet-4-6',           'Claude Sonnet 4.6', '{primary,background,compact}'),
    ('anthropic', 'claude-haiku-4-5-20251001',   'Claude Haiku 4.5',  '{primary,compact,emotion}'),
    ('gemini',    'gemini-2.5-flash',            'Gemini 2.5 Flash',  '{primary,compact,emotion}'),
    ('gemini',    'gemini-2.5-pro',              'Gemini 2.5 Pro',    '{primary,background,compact}'),
    ('openai',    'gpt-4o',                      'GPT-4o',            '{primary,background,compact}'),
    ('openai',    'gpt-4o-mini',                 'GPT-4o Mini',       '{primary,compact,emotion}'),
    ('openai',    'o3',                          'o3',                '{primary,background}'),
    ('openai',    'o4-mini',                     'o4 Mini',           '{primary,compact}'),
    ('openai',    'text-embedding-3-small',      'Embeddings Small',  '{embedding}'),
    ('openai',    'whisper-1',                   'Whisper',           '{transcription}')
ON CONFLICT DO NOTHING;

-- Seed current config (matches config.yaml defaults)
INSERT INTO model_config (role, provider, model_name) VALUES
    ('primary',    'anthropic', 'claude-haiku-4-5-20251001'),
    ('background', 'anthropic', 'claude-opus-4-6'),
    ('compact',    'gemini',    'gemini-2.5-flash'),
    ('emotion',    'gemini',    'gemini-2.5-flash')
ON CONFLICT DO NOTHING;
