-- A prompt profile is the named bundle of prompt-stack settings that has to
-- change together when a role moves to a different class of model: which
-- prompt files it reads, how many tools it is shown, how much retrieved
-- memory is injected. Those knobs live in four different places (files on
-- disk, a Go map, scoring_config, this table), so before this column an
-- experiment could not be expressed as a single change and no two arms of
-- an A/B were guaranteed to differ in exactly one thing.
--
-- Only the POINTER lives here. The prompt text stays on disk, deploy-gated
-- and reviewable — see the comment on resolvePlatformPrompts in the host
-- for why moving prompt text into a table is a mistake this stack has
-- already paid for once. Keeping the selector next to provider/model_name
-- means the model and the manual it is written against change atomically,
-- and since the gateway refreshes this table on every chat turn, an arm
-- takes effect on the next message and rolls back with one UPDATE.
--
-- Empty string means "base prompts only", which is exactly today's
-- behaviour, so this migration is a no-op until a row opts in.
ALTER TABLE model_config
    ADD COLUMN IF NOT EXISTS prompt_profile TEXT NOT NULL DEFAULT '';
