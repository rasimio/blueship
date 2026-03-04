CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS embeddings (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     TEXT,
    source_type TEXT NOT NULL,
    source_id   UUID NOT NULL,
    model       TEXT NOT NULL,
    dimension   INT NOT NULL,
    vector      vector(1536) NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_embeddings_source_model ON embeddings(source_type, source_id, model);
CREATE INDEX IF NOT EXISTS idx_embeddings_user_model ON embeddings(user_id, model);
CREATE INDEX IF NOT EXISTS idx_embeddings_vector_hnsw ON embeddings USING hnsw (vector vector_cosine_ops) WITH (m = 16, ef_construction = 64);

CREATE TABLE IF NOT EXISTS embedding_config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
INSERT INTO embedding_config (key, value) VALUES
    ('current_model', 'text-embedding-3-small'),
    ('current_dimension', '1536')
ON CONFLICT (key) DO NOTHING;
