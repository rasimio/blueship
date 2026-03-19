-- Rename "primary" role to "cortex" and add "reflex" role.
-- Reflex = fast classifier (Gemini Flash), Cortex = response generator (Qwen/Claude).

-- model_config: rename primary → cortex
UPDATE model_config SET role = 'cortex' WHERE role = 'primary';

-- Add reflex role (Gemini Flash 2.5)
INSERT INTO model_config (role, provider, model_name)
VALUES ('reflex', 'gemini', 'gemini-2.5-flash')
ON CONFLICT DO NOTHING;

-- role_tools: rename primary → cortex
UPDATE role_tools SET role = 'cortex' WHERE role = 'primary';

-- Add web tools to cortex (needed for "взлетай" autonomous research)
INSERT INTO role_tools (role, tool_name, sort_order) VALUES
    ('cortex', 'web_search', 10),
    ('cortex', 'web_fetch', 11)
ON CONFLICT DO NOTHING;
