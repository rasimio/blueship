-- Notifications are hints delivered after commit; periodic scanning remains the
-- recovery mechanism. Do not notify on running -> pending retry transitions.
CREATE OR REPLACE FUNCTION notify_agent_task_queue() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'INSERT' THEN
  PERFORM pg_notify('blueship_agent_task_queue', TG_TABLE_SCHEMA);
 ELSIF NEW.status IS DISTINCT FROM OLD.status AND
   (NEW.status IN ('done','failed','canceled') OR
    (NEW.status='pending' AND OLD.status IN ('paused','canceled'))) THEN
  PERFORM pg_notify('blueship_agent_task_queue', TG_TABLE_SCHEMA);
 END IF;
 RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS agent_task_queue_notify ON agent_tasks;
CREATE TRIGGER agent_task_queue_notify AFTER INSERT OR UPDATE OF status ON agent_tasks
 FOR EACH ROW EXECUTE FUNCTION notify_agent_task_queue();
