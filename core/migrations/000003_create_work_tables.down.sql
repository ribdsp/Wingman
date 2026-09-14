-- Reverse of the up migration: children before parents, so no drop needs CASCADE.
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS token_spend;
DROP TABLE IF EXISTS run_steps;
DROP TABLE IF EXISTS runs;

DROP TRIGGER IF EXISTS tasks_set_updated_at ON tasks;
DROP TABLE IF EXISTS tasks;

DROP TRIGGER IF EXISTS chats_set_updated_at ON chats;
DROP TABLE IF EXISTS chats;
