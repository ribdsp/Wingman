-- Accounts, sessions and the channel identities linked to them.
--
-- One rule runs through this file: an account is a person, not an administrator.
-- There is no role column on users, and adding one would be an API change with an
-- argument attached. Privilege in core comes from where a credential lives —
-- CORE_API_KEYS makes an operator, CORE_BOT_KEYS makes a bot, and this table makes
-- neither. A row somebody can UPDATE must never be able to raise a role.

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Stored lower-cased so one person cannot hold two accounts differing only in
    -- capitalisation, which is also how they would end up with two token budgets.
    email         text NOT NULL UNIQUE,
    display_name  text NOT NULL DEFAULT '',
    -- An argon2id PHC string from internal/auth. Never a plaintext password, and
    -- never a reversible encoding of one.
    password_hash text NOT NULL,
    -- Deactivation rather than deletion: a run, its transcript and its spend are
    -- the record of money that was actually spent, and they reference this row.
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT users_email_lowercase CHECK (email = lower(email)),
    CONSTRAINT users_email_shaped CHECK (position('@' IN email) > 1),
    CONSTRAINT users_password_hash_is_argon2id CHECK (password_hash LIKE '$argon2id$%')
);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One row per signed-in device. Sessions are opaque: the column holds a SHA-256 of
-- the token, so a database dump is not a set of live logins.
CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The check is the invariant, not a formality: a 64-character hex digest is the
    -- only thing that fits, so a refactor that accidentally stores the plaintext
    -- token fails on insert instead of quietly working.
    token_hash   text NOT NULL UNIQUE,
    -- Recorded to help somebody recognise a session that is not theirs. Both are
    -- caller-supplied, so neither is trusted for anything.
    user_agent   text NOT NULL DEFAULT '',
    created_ip   inet,
    expires_at   timestamptz NOT NULL,
    -- Set when the person signs out or an operator revokes the session. Kept rather
    -- than deleted so "this session was ended" and "this session never existed" stay
    -- different facts.
    revoked_at   timestamptz,
    last_seen_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT sessions_token_hash_is_sha256 CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT sessions_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX sessions_user_time_idx ON sessions (user_id, created_at DESC);
-- Expiry sweeps read this; the unique index on token_hash serves authentication.
CREATE INDEX sessions_expiry_idx ON sessions (expires_at) WHERE revoked_at IS NULL;

-- A person's account on a connected channel. Stage 2 fills these in; the table lands
-- now because a run dispatched from a channel has to be attributable to an account
-- before it is allowed to spend anything.
CREATE TABLE channel_identities (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind         channel_kind NOT NULL,
    -- The channel's own id for the person — a Telegram user id, a Slack member id.
    external_id  text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    linked_at    timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT channel_identities_external_id_present CHECK (external_id <> ''),
    -- One channel account maps to at most one Wingman account. This constraint is
    -- what stops a second account claiming an identity that is already linked and
    -- inheriting its conversations.
    CONSTRAINT channel_identities_unique_per_channel UNIQUE (kind, external_id)
);

CREATE INDEX channel_identities_user_idx ON channel_identities (user_id, kind);
