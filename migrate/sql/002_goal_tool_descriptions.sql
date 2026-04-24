-- Goal platform tools (registered by blueship/tool/goal_tools.go) require
-- rows in blueship.tools with their descriptions. Phase 1 of the BlueFleet
-- migration moves goal_create + goal_approve from Arlene into BlueShip core
-- and introduces three new ones: goal_status, goal_list, goal_cancel.
-- Descriptions are the only things tool-registry code loads from this
-- table; schemas + handlers live in code.

INSERT INTO tools (name, description, mode, exposed, schema)
VALUES
    ('goal_create',
     'Создать автономную цель (примитив BlueShip). strategy: structured (LLM планирует JSON-план, executor исполняет), direct (один LLM цикл со всеми инструментами), delegate (цель отдаётся peer-агенту). Возвращает id цели, статус, strategy.',
     'sync', false, '{}'),
    ('goal_status',
     'Прочитать полный статус цели по id: статус (pending/running/paused/done/failed/canceled), iteration, progress JSONB, результат. Используй когда надо проверить что с конкретной целью.',
     'sync', false, '{}'),
    ('goal_list',
     'Список целей текущего агента, опционально по статусу. Без status= — все. Используй чтобы увидеть что в работе.',
     'sync', false, '{}'),
    ('goal_cancel',
     'Отменить активную цель (pending/running/paused). Принимает id и опциональный reason. Терминальные цели (done/failed/canceled) не меняются.',
     'sync', false, '{}'),
    ('goal_approve',
     'Снять паузу с цели (например после ревью milestone). Переводит статус paused → pending, ближайший тик scheduler-а подхватит.',
     'sync', false, '{}')
ON CONFLICT (name) DO UPDATE SET
    description = EXCLUDED.description,
    mode = EXCLUDED.mode,
    exposed = EXCLUDED.exposed;

-- Expose goal tools to cortex role (main decision-making agent loop).
-- goal_create + goal_approve were already mapped before Phase 1; this
-- backfills goal_status, goal_list, goal_cancel.
INSERT INTO role_tools (role, tool_name, sort_order)
VALUES
    ('cortex', 'goal_create',  10),
    ('cortex', 'goal_status',  11),
    ('cortex', 'goal_list',    12),
    ('cortex', 'goal_cancel',  13),
    ('cortex', 'goal_approve', 14)
ON CONFLICT (role, tool_name) DO NOTHING;

