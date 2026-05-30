-- Phase 7 — Fleet integration.
--
-- Ships register with BlueFleet and periodically refresh a local cache of
-- discovered peers. This table is a pure read-through cache: Fleet remains
-- the source of truth, the Ship stores the last snapshot so tool calls do
-- not depend on Fleet being reachable for every invocation.
--
-- One row per (agent_id, Fleet instance). Agent identity is a UUID minted
-- by Fleet; name is the human-friendly slug for logs.
CREATE TABLE IF NOT EXISTS fleet_peer_cache (
    agent_id         UUID PRIMARY KEY,
    name             TEXT NOT NULL,
    display_name     TEXT NOT NULL,
    description      TEXT,
    endpoint_url     TEXT,
    public_key       TEXT,
    status           TEXT NOT NULL DEFAULT 'active',
    capabilities     JSONB NOT NULL DEFAULT '[]'::jsonb,
    tools            JSONB NOT NULL DEFAULT '[]'::jsonb,
    last_refreshed   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_fleet_peer_cache_name ON fleet_peer_cache(name);
CREATE INDEX IF NOT EXISTS idx_fleet_peer_cache_status ON fleet_peer_cache(status) WHERE status = 'active';
