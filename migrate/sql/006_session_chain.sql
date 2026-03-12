ALTER TABLE chat_sessions ADD COLUMN IF NOT EXISTS previous_id UUID REFERENCES chat_sessions(id);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_previous ON chat_sessions(previous_id) WHERE previous_id IS NOT NULL;
