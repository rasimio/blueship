-- agent_tasks.progress / .config may not be NULL.
--
-- Both are read back into json.RawMessage struct fields, and
-- json.RawMessage implements no sql.Scanner: a NULL lands in
-- convertAssign's fallthrough and fails the whole row scan with
-- "unsupported Scan, storing driver.Value type <nil> into type
-- *json.RawMessage". Every read of the table is `SELECT *`, so one NULL
-- progress does not degrade one task — it takes down PendingTasks, i.e.
-- the entire scheduler loop, on every tick until the row is repaired.
--
-- The write that produced it is ordinary: a handler whose iteration has
-- nothing to checkpoint returns IterationResult with no Progress, and the
-- nil json.RawMessage reaches the driver as SQL NULL. The DEFAULT '{}'
-- never applies, because it only fills a column that was OMITTED, not one
-- assigned NULL explicitly (same trap as tools/use_agents in Create).
-- The store now normalises to '{}' on every write path; this constraint is
-- what makes that invariant enforced rather than merely intended — `plan`
-- has been NOT NULL since it was added, and never had the problem.

UPDATE agent_tasks SET progress = '{}'::jsonb WHERE progress IS NULL;
UPDATE agent_tasks SET config   = '{}'::jsonb WHERE config   IS NULL;

ALTER TABLE agent_tasks
    ALTER COLUMN progress SET DEFAULT '{}'::jsonb,
    ALTER COLUMN progress SET NOT NULL,
    ALTER COLUMN config   SET DEFAULT '{}'::jsonb,
    ALTER COLUMN config   SET NOT NULL;
