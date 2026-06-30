-- Per-role recent-message history budget.

ALTER TABLE model_config
    ADD COLUMN IF NOT EXISTS message_budget INT NOT NULL DEFAULT 0;

UPDATE model_config
   SET message_budget = 6000,
       updated_at = NOW()
 WHERE role IN ('cortex', 'background');

UPDATE model_config
   SET message_budget = 3000,
       updated_at = NOW()
 WHERE role = 'recurring';

UPDATE model_config
   SET message_budget = 4000,
       updated_at = NOW()
 WHERE role = 'reflex';

UPDATE model_config
   SET message_budget = 8192,
       updated_at = NOW()
 WHERE role IN ('saver', 'agent-saver', 'compact', 'grounding_evaluator');
