DROP INDEX IF EXISTS chats_channel_conversation_idx;

ALTER TABLE chats DROP CONSTRAINT IF EXISTS chats_channel_pair;

ALTER TABLE chats
    DROP COLUMN IF EXISTS channel_conversation_id,
    DROP COLUMN IF EXISTS channel_kind;

DROP TABLE IF EXISTS channel_link_codes;
