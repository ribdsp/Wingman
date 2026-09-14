-- Conversations, the work they ask for, and what that work cost.
--
-- Two rules run through this file.
--
-- Every table holding a person's content carries owner_user_id or user_id directly,
-- even where it could be reached through a join. Scoping a query through a join is
-- scoping somebody can forget to write, and the failure mode of forgetting is one
-- account reading another's conversations.
--
-- A run's record is immutable once it ends. The transcript, the stop reason and the
-- spend are the only evidence of what an unattended agent did with somebody's money,
-- so the tables that hold them refuse a deleting account rather than following it.

CREATE TABLE chats (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title       text NOT NULL DEFAULT '',
    -- Archived rather than deleted, so a person can clear their list without
    -- destroying the context a later run may be asked about.
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER chats_set_updated_at
    BEFORE UPDATE ON chats
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX chats_user_time_idx ON chats (user_id, updated_at DESC)
    WHERE archived_at IS NULL;

-- One unit of work Wingman was asked to do, from any of the three sources.
CREATE TABLE tasks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Every task has an owner, including one the goal engine dispatched: unattended
    -- work is still somebody's, and a run whose tokens belong to nobody cannot be
    -- capped. RESTRICT because this row and its spend outlive an account's use of it.
    owner_user_id   uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    source          task_source NOT NULL,
    -- Where the answer goes when the work came from a chat.
    chat_id         uuid REFERENCES chats (id) ON DELETE SET NULL,
    -- Straight from the goal engine's dispatch payload: which agent persona to run
    -- as, and where to report.
    bot_id          text NOT NULL DEFAULT '',
    channel_id      text NOT NULL DEFAULT '',
    brief           text NOT NULL,
    -- NULL means the caller offered no key. It is nullable rather than defaulting to
    -- '' because the unique index below would then make every keyless task collide
    -- with the first one; and the check refuses '' so "no key" has exactly one
    -- spelling.
    idempotency_key text,
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    status          task_status NOT NULL DEFAULT 'queued',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    -- Mirrors domain.MaxBriefLength. A brief is an instruction, not a document.
    CONSTRAINT tasks_brief_bounded CHECK (char_length(brief) BETWEEN 1 AND 16000),
    -- Mirrors domain.MaxIdempotencyKeyLength.
    CONSTRAINT tasks_idempotency_key_bounded CHECK (
        idempotency_key IS NULL OR char_length(idempotency_key) BETWEEN 1 AND 200
    ),
    CONSTRAINT tasks_metadata_is_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE TRIGGER tasks_set_updated_at
    BEFORE UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- This is what makes a retry safe. The goal engine retries a dispatch it believes may
-- have failed, so the second attempt has to find the first task rather than start a
-- second one — the unique constraint is the guarantee, not the lookup that precedes it.
CREATE UNIQUE INDEX tasks_idempotency_key_idx ON tasks (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX tasks_owner_time_idx ON tasks (owner_user_id, created_at DESC);
CREATE INDEX tasks_queued_idx ON tasks (created_at) WHERE status = 'queued';

-- One execution of a task. A task can be run more than once — a retry after a
-- provider outage is a second run, not an edit of the first.
CREATE TABLE runs (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id                 uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    owner_user_id           uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    -- What actually answered, not what was asked for. A run that fell back to a
    -- cheaper model is a run whose output has to be read differently.
    provider                text NOT NULL DEFAULT '',
    model                   text NOT NULL DEFAULT '',

    -- The limits this run actually ran under, copied in rather than looked up. An old
    -- run judged against today's caps is a run judged against a bound it never had.
    max_iterations          integer NOT NULL,
    max_tool_calls          integer NOT NULL,
    max_tokens_per_run      bigint NOT NULL,
    max_tokens_per_user_day bigint NOT NULL,
    step_timeout_seconds    integer NOT NULL,
    sandbox_timeout_seconds integer NOT NULL,

    iterations              integer NOT NULL DEFAULT 0,
    tool_calls              integer NOT NULL DEFAULT 0,
    tokens_used             bigint NOT NULL DEFAULT 0,

    -- NULL while the run is in flight. domain.Run spells the same state as an empty
    -- StopReason; the repository maps between the two.
    stop                    run_stop_reason,
    reason                  text NOT NULL DEFAULT '',
    -- A cancellation is a request, not a state. The loop notices it at the top of its
    -- next iteration and stops with 'cancelled', so the transcript records how far the
    -- run got rather than ending mid-step with no explanation.
    cancel_requested_at     timestamptz,
    started_at              timestamptz NOT NULL DEFAULT now(),
    finished_at             timestamptz,

    -- Every limit is a ceiling and a zero would read as "no limit" to a loop, so a
    -- zero must not be storable. -1 in the daily cap is domain.NoUserDailyCap, spelled
    -- out because an allowance that is absent by choice and one that is zero by
    -- omission must not look the same.
    CONSTRAINT runs_limits_positive CHECK (
        max_iterations > 0
        AND max_tool_calls > 0
        AND max_tokens_per_run > 0
        AND step_timeout_seconds > 0
        AND sandbox_timeout_seconds > 0
    ),
    CONSTRAINT runs_user_day_cap_meaningful CHECK (
        max_tokens_per_user_day > 0 OR max_tokens_per_user_day = -1
    ),
    CONSTRAINT runs_counters_not_negative CHECK (
        iterations >= 0 AND tool_calls >= 0 AND tokens_used >= 0
    ),
    -- A finished run has both a reason and a time; an in-flight run has neither. The
    -- half-states are what a crashed worker would leave behind, and they would read
    -- as a run still going.
    CONSTRAINT runs_finished_together CHECK (
        (stop IS NULL AND finished_at IS NULL) OR (stop IS NOT NULL AND finished_at IS NOT NULL)
    )
);

CREATE INDEX runs_task_time_idx ON runs (task_id, started_at DESC);
CREATE INDEX runs_owner_time_idx ON runs (owner_user_id, started_at DESC);
-- What a starting worker reads to find work abandoned by a worker that died.
CREATE INDEX runs_in_flight_idx ON runs (started_at) WHERE stop IS NULL;

-- The transcript: one row per model call and per tool call, in order.
CREATE TABLE run_steps (
    id         bigserial PRIMARY KEY,
    run_id     uuid NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    -- Starts at 1, assigned by the caller, unique per run. A gap is then visible as a
    -- gap rather than being read as the beginning of the transcript.
    idx        integer NOT NULL,
    kind       step_kind NOT NULL,
    tool_name  text NOT NULL DEFAULT '',
    tokens_in  bigint NOT NULL DEFAULT 0,
    tokens_out bigint NOT NULL DEFAULT 0,
    -- Bounded by domain.TruncateContent before it arrives. There is no length check
    -- here on purpose: one that disagreed with the domain constant by the width of
    -- the truncation marker would reject a correctly truncated step.
    content    text NOT NULL DEFAULT '',
    -- A short message for a human reading the transcript. Never a provider payload,
    -- an API key, a DSN or a header.
    err        text NOT NULL DEFAULT '',
    at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT run_steps_index_from_one CHECK (idx >= 1),
    CONSTRAINT run_steps_index_unique_per_run UNIQUE (run_id, idx),
    CONSTRAINT run_steps_tokens_not_negative CHECK (tokens_in >= 0 AND tokens_out >= 0),
    -- A tool step names its tool and a model step does not, so a step cannot be
    -- ambiguous about which of the two it was.
    CONSTRAINT run_steps_tool_named CHECK ((kind = 'tool') = (tool_name <> '')),
    -- A sandbox does not bill tokens. Without this, a tool step charged for the model
    -- call that requested it would double-count against the run's allowance.
    CONSTRAINT run_steps_tools_are_free CHECK (
        kind <> 'tool' OR (tokens_in = 0 AND tokens_out = 0)
    )
);

-- No separate index on (run_id, idx): the unique constraint above already builds one,
-- and reading a transcript in order is exactly the query it serves.

-- The per-user token ledger. This is what the daily cap is enforced against, and what
-- core reports to the goal engine as the ops.tokens_spent metric.
--
-- The report carries the running total for the day rather than an increment, so
-- re-sending one after a failed push cannot inflate the number. That is why there is
-- no "reported" column here: at-least-once delivery of a total needs no bookkeeping.
--
-- The day boundary is not stored either. It depends on the deployment's timezone,
-- which lives in configuration, so the repository computes the boundary and this table
-- only records when something happened.
CREATE TABLE token_spend (
    id          bigserial PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    -- SET NULL rather than CASCADE: if a run record ever goes, the money it spent
    -- still went.
    run_id      uuid REFERENCES runs (id) ON DELETE SET NULL,
    provider    text NOT NULL DEFAULT '',
    model       text NOT NULL DEFAULT '',
    tokens_in   bigint NOT NULL DEFAULT 0,
    tokens_out  bigint NOT NULL DEFAULT 0,
    -- Generated so the sum cannot drift from its parts.
    tokens_total bigint GENERATED ALWAYS AS (tokens_in + tokens_out) STORED,
    occurred_at timestamptz NOT NULL DEFAULT now(),

    -- A negative row would refund an allowance nobody granted, and domain.Ledger
    -- already treats a negative total as unreadable rather than as credit.
    CONSTRAINT token_spend_not_negative CHECK (tokens_in >= 0 AND tokens_out >= 0)
);

CREATE INDEX token_spend_user_time_idx ON token_spend (user_id, occurred_at DESC);
CREATE INDEX token_spend_run_idx ON token_spend (run_id);

-- What was said, in a chat. A message is what a person reads back; run_steps is what
-- an operator audits. They are separate because a transcript full of tool output is
-- not a conversation.
CREATE TABLE messages (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id    uuid NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    -- Denormalised on purpose; see the note at the top of this file.
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The run that produced an assistant message, when one did. NULL on anything a
    -- person typed.
    run_id     uuid REFERENCES runs (id) ON DELETE SET NULL,
    role       message_role NOT NULL,
    content    text NOT NULL DEFAULT '',
    tokens_in  bigint NOT NULL DEFAULT 0,
    tokens_out bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT messages_tokens_not_negative CHECK (tokens_in >= 0 AND tokens_out >= 0),
    -- Only the assistant speaks for a run.
    CONSTRAINT messages_run_is_assistants CHECK (run_id IS NULL OR role = 'assistant')
);

CREATE INDEX messages_chat_time_idx ON messages (chat_id, created_at);
CREATE INDEX messages_user_time_idx ON messages (user_id, created_at DESC);
