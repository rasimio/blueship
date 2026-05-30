-- 012_opus48_effort.sql — Opus 4.8 + reasoning-effort support.
--
-- Adds two reasoning controls, plumbed end-to-end in this release:
--   model_config.thinking_mode   '' (legacy budget) | 'adaptive' | 'off'
--   model_config.effort          '' | low | medium | high | xhigh | max
-- effort maps to Anthropic's output_config.effort; thinking_mode 'adaptive'
-- maps to thinking:{type:"adaptive"} (Claude 4.6+), which supersedes the
-- manual thinking_budget. Verified live on the OAuth/Claude-Code surface
-- 2026-05-28: claude-opus-4-8 accepts output_config.effort and
-- thinking:{type:"adaptive"} (HTTP 200). Provider in prod is "anthropic-oauth".
--
-- available_models is the picker catalog: a single API model can be exposed as
-- several nicely-named reasoning profiles, so its PK widens to include the two
-- new columns. Fully idempotent (safe to apply by hand before deploy, then let
-- blueship's auto-migrate no-op + record it).

-- 1. Runtime per-role controls.
ALTER TABLE model_config ADD COLUMN IF NOT EXISTS thinking_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE model_config ADD COLUMN IF NOT EXISTS effort        TEXT NOT NULL DEFAULT '';

-- 2. Catalog controls + widened key (multiple profiles per provider+model).
ALTER TABLE available_models ADD COLUMN IF NOT EXISTS thinking_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE available_models ADD COLUMN IF NOT EXISTS effort        TEXT NOT NULL DEFAULT '';
ALTER TABLE available_models DROP CONSTRAINT IF EXISTS available_models_pkey;
ALTER TABLE available_models ADD CONSTRAINT available_models_pkey
    PRIMARY KEY (provider, name, effort, thinking_mode);

-- 3. The three Opus 4.8 reasoning profiles (the proposed variants), nicely
--    named for mapping roles to them per situation.
INSERT INTO available_models (provider, name, display_name, capabilities, effort, thinking_mode) VALUES
  ('anthropic-oauth', 'claude-opus-4-8', 'Opus 4.8 · xHigh (adaptive)',    '{primary,background}',         'xhigh', 'adaptive'),
  ('anthropic-oauth', 'claude-opus-4-8', 'Opus 4.8 · High (adaptive)',     '{primary,background,compact}', 'high',  'adaptive'),
  ('anthropic-oauth', 'claude-opus-4-8', 'Opus 4.8 · xHigh (no thinking)', '{primary}',                    'xhigh', 'off')
ON CONFLICT (provider, name, effort, thinking_mode)
DO UPDATE SET display_name = EXCLUDED.display_name, capabilities = EXCLUDED.capabilities;

-- 4. Switch cortex to Opus 4.8 + xHigh + adaptive thinking. Manual budget is
--    superseded by adaptive (zero it). Bump max_tokens: at xhigh, thinking
--    counts toward max_tokens, so 8192 would truncate replies (stop_reason
--    = max_tokens). 32000 leaves headroom; tune if truncation appears.
UPDATE model_config
   SET model_name      = 'claude-opus-4-8',
       thinking_mode   = 'adaptive',
       effort          = 'xhigh',
       thinking_budget = 0,
       max_tokens      = 32000,
       updated_at      = NOW()
 WHERE role = 'cortex';
