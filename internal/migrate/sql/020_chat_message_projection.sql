-- Additive, non-destructive transcript projection.
--
-- Raw content remains the audit/provider payload. Existing rows deliberately
-- start as unprojectable_legacy: a later conservative projector may promote
-- only rows whose visible transport text can be recovered without guessing.

ALTER TABLE chat_messages
    ADD COLUMN IF NOT EXISTS visible_text TEXT,
    ADD COLUMN IF NOT EXISTS projection_status TEXT NOT NULL DEFAULT 'unprojectable_legacy',
    ADD COLUMN IF NOT EXISTS projection_reason TEXT DEFAULT 'legacy_not_projected',
    ADD COLUMN IF NOT EXISTS projector_version TEXT NOT NULL DEFAULT 'legacy-unprojected';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'chat_messages'::regclass
          AND conname = 'chat_messages_projection_status_check'
    ) THEN
        ALTER TABLE chat_messages
            ADD CONSTRAINT chat_messages_projection_status_check
            CHECK (projection_status IN (
                'projected',
                'non_dialogue',
                'unprojectable_legacy'
            )) NOT VALID;
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'chat_messages'::regclass
          AND conname = 'chat_messages_projection_shape_check'
    ) THEN
        ALTER TABLE chat_messages
            ADD CONSTRAINT chat_messages_projection_shape_check
            CHECK (
                (
                    projection_status = 'projected'
                    AND visible_text IS NOT NULL
                )
                OR
                (
                    projection_status <> 'projected'
                    AND visible_text IS NULL
                    AND NULLIF(BTRIM(projection_reason), '') IS NOT NULL
                )
            ) NOT VALID;
    END IF;
END
$$;
