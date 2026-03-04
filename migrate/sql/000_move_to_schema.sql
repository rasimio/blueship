-- Move BlueShip tables from public to blueship schema.
-- Production: moves existing tables (data, FK, indexes preserved).
-- Fresh install: no-op (tables don't exist in public yet).
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'user_profiles') THEN
        ALTER TABLE public.user_profiles SET SCHEMA blueship;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'chat_sessions') THEN
        ALTER TABLE public.chat_sessions SET SCHEMA blueship;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'chat_messages') THEN
        ALTER TABLE public.chat_messages SET SCHEMA blueship;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'embeddings') THEN
        ALTER TABLE public.embeddings SET SCHEMA blueship;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'embedding_config') THEN
        ALTER TABLE public.embedding_config SET SCHEMA blueship;
    END IF;

    -- Cleanup: drop old tracking table left in public schema
    DROP TABLE IF EXISTS public.blueship_migrations;
END $$;
