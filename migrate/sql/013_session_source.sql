ALTER TABLE chat_sessions ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE chat_sessions ADD COLUMN IF NOT EXISTS source_id UUID;

CREATE INDEX IF NOT EXISTS idx_chat_sessions_source ON chat_sessions(source) WHERE source <> 'chat';
CREATE INDEX IF NOT EXISTS idx_chat_sessions_source_id ON chat_sessions(source_id) WHERE source_id IS NOT NULL;
