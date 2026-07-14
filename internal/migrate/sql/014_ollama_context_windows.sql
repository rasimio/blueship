-- Ollama latency tuning for local models.
--
-- message_budget limits the stored chat history selected for a call, but
-- Ollama still needs an explicit num_ctx to avoid running every small request
-- in a huge model-context profile. context_window is provider-specific; cloud
-- providers ignore it, Ollama maps it to options.num_ctx.

ALTER TABLE model_config
    ADD COLUMN IF NOT EXISTS context_window INT NOT NULL DEFAULT 0;

INSERT INTO available_models (provider, name, display_name, capabilities, effort, thinking_mode) VALUES
  ('ollama', 'gemma4:26b-a4b-it-q4_K_M', 'Gemma 4 26B A4B IT · Q4_K_M', '{primary,background}', '', ''),
  ('ollama', 'gemma4:e4b',                'Gemma 4 E4B IT',              '{recurring,saver,compact}', '', '')
ON CONFLICT (provider, name, effort, thinking_mode)
DO UPDATE SET display_name = EXCLUDED.display_name, capabilities = EXCLUDED.capabilities;

UPDATE model_config
   SET provider        = 'ollama',
       model_name      = 'gemma4:26b-a4b-it-q4_K_M',
       max_tokens      = 2048,
       context_window  = 32768,
       thinking_budget = 0,
       thinking_mode   = '',
       effort          = '',
       updated_at      = NOW()
 WHERE role = 'cortex';

UPDATE model_config
   SET provider        = 'ollama',
       model_name      = 'gemma4:e4b',
       max_tokens      = 256,
       context_window  = 12288,
       temperature     = 0.3,
       thinking_budget = 0,
       thinking_mode   = '',
       effort          = '',
       updated_at      = NOW()
 WHERE role = 'recurring';

UPDATE model_config
   SET provider        = 'ollama',
       model_name      = 'gemma4:e4b',
       max_tokens      = 1024,
       context_window  = 8192,
       temperature     = 0.2,
       thinking_budget = 0,
       thinking_mode   = '',
       effort          = '',
       updated_at      = NOW()
 WHERE role = 'saver';

INSERT INTO model_config (
    role, provider, model_name, max_tokens, context_window,
    temperature, thinking_budget, thinking_mode, effort, updated_at
) VALUES (
    'agent-saver', 'ollama', 'gemma4:e4b', 1024, 8192,
    0.2, 0, '', '', NOW()
)
ON CONFLICT (role) DO UPDATE
   SET provider        = EXCLUDED.provider,
       model_name      = EXCLUDED.model_name,
       max_tokens      = EXCLUDED.max_tokens,
       context_window  = EXCLUDED.context_window,
       temperature     = EXCLUDED.temperature,
       thinking_budget = EXCLUDED.thinking_budget,
       thinking_mode   = EXCLUDED.thinking_mode,
       effort          = EXCLUDED.effort,
       updated_at      = NOW();
