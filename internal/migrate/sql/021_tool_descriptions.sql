-- Versioned native tool descriptions.
--
-- Tool names, schemas, handlers, and effect metadata remain code-owned. This
-- table stores only the reviewed prose presented to models.

CREATE TABLE IF NOT EXISTS tool_descriptions (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    version     TEXT NOT NULL DEFAULT 'v1',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT tool_descriptions_name_nonempty
        CHECK (BTRIM(name) <> ''),
    CONSTRAINT tool_descriptions_description_nonempty
        CHECK (BTRIM(description) <> ''),
    CONSTRAINT tool_descriptions_version_nonempty
        CHECK (BTRIM(version) <> '')
);
