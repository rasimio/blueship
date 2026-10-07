-- Origin is captured at creation. Later configuration edits cannot redirect
-- an already accepted task or its pending notification to another chat.
CREATE OR REPLACE FUNCTION preserve_agent_task_origin() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.config->'delivery_origin') IS DISTINCT FROM (OLD.config->'delivery_origin') THEN
  RAISE EXCEPTION 'task delivery origin is immutable';
 END IF;
 RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS agent_task_origin_immutable ON agent_tasks;
CREATE TRIGGER agent_task_origin_immutable BEFORE UPDATE OF config ON agent_tasks
FOR EACH ROW EXECUTE FUNCTION preserve_agent_task_origin();
