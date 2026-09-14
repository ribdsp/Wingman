-- What a connected channel needs beyond the identity table migration 000002 left
-- ready: a way to prove who you are from inside a chat app, and somewhere to keep the
-- conversation.
--
-- The rule that shapes both is that a channel is somebody else's system. Wingman does
-- not control who can send it a message there, so nothing here may grant more than the
-- account it resolves to already has, and an unlinked sender must be able to reach
-- exactly one thing: the link check.

-- A short-lived code a person mints while signed in and then pastes into Telegram,
-- Slack or Discord.
--
-- It is the only credential in the system that travels through a third party's servers,
-- which is why it is the shortest-lived one: minutes, single use, and stored as a hash
-- like a session token. A code readable from a database dump would be an account
-- takeover that needs no password.
--
-- It is not scoped to a channel kind. A person pastes it where they happen to be, and
-- making them mint a Telegram code and a Discord code separately would be two
-- opportunities to paste the wrong one into a public room.
CREATE TABLE channel_link_codes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- CASCADE: an account that is gone cannot be linked to, so its outstanding codes
    -- are not evidence of anything.
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- sha256 of the code as the person sees it. Never the code itself.
    code_hash   text NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    -- Set the moment a code is used, in the same statement that reads it. Single use is
    -- enforced by that UPDATE, not by a check-then-write.
    consumed_at timestamptz,

    CONSTRAINT channel_link_codes_hash_present CHECK (code_hash <> ''),
    CONSTRAINT channel_link_codes_expiry_after_creation CHECK (expires_at > created_at)
);

-- Minting supersedes whatever the same person had outstanding, so this index serves the
-- sweep as well as the list.
CREATE INDEX channel_link_codes_user_idx ON channel_link_codes (user_id, created_at DESC);

-- Where a channel conversation lives on this side.
--
-- A Telegram thread is a chat. It could have been a table of its own mapping a
-- conversation to a chat id, but that table would hold one row per chat and nothing
-- else: the conversation *is* the chat, and the alternative is a join on every message
-- to answer "which thread is this".
ALTER TABLE chats
    ADD COLUMN channel_kind            channel_kind,
    ADD COLUMN channel_conversation_id text NOT NULL DEFAULT '';

-- Both or neither. A chat with a kind and no conversation id has nowhere to send an
-- answer, and one with a conversation id and no kind does not know which platform to
-- send it on — either way the reply is lost, so the row is refused instead.
ALTER TABLE chats ADD CONSTRAINT chats_channel_pair CHECK (
    (channel_kind IS NULL AND channel_conversation_id = '')
    OR (channel_kind IS NOT NULL AND channel_conversation_id <> '')
);

-- One chat per person per conversation, which is what makes an inbound message find the
-- history of the last one instead of starting afresh.
--
-- Scoped by user_id rather than globally unique on the conversation, because a group
-- conversation can hold two linked people. Each gets their own thread on this side:
-- their runs, their transcript, their budget. Two people sharing one chat row would put
-- one person's agent output in the other's conversation history.
CREATE UNIQUE INDEX chats_channel_conversation_idx
    ON chats (user_id, channel_kind, channel_conversation_id)
    WHERE channel_kind IS NOT NULL;
