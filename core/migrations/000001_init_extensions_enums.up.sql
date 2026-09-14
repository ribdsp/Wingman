-- Wingman core: enums and shared helpers.
--
-- Requires PostgreSQL 13+ for gen_random_uuid(). Tested against 16.
--
-- Every enum below mirrors a closed set in internal/domain. That is the point of
-- using enums rather than text: a stop reason the code cannot produce is a row the
-- database refuses, so a mistyped constant fails on write instead of being read back
-- weeks later as a run that ended for no known reason.
--
-- There is deliberately no system_flags table here. The kill switch lives in the goal
-- engine and core reads it over HTTP; a second copy in this database would be a second
-- answer to "are we halted", and the two would disagree at exactly the wrong moment.

-- What asked for the work. Mirrors domain.TaskSource, and TaskSource.Attended()
-- decides which of these means somebody is watching.
CREATE TYPE task_source AS ENUM ('goal_engine', 'user', 'channel');

CREATE TYPE task_status AS ENUM ('queued', 'running', 'succeeded', 'failed');

-- Mirrors domain.StopReason, in the order domain.AllStopReasons lists it. Every
-- finished run stores exactly one, so every value has to be representable — including
-- the ones that mean "I could not tell", which are kept distinct from the ones that
-- mean "nothing was wrong".
--
-- The last two are not the continuation ladder's: 'tool_denied' is decided per tool
-- call, and 'abandoned' is written by the sweep that finds runs whose worker died. A
-- crash is filed under its own reason rather than under 'provider_error' because those
-- send an operator to two different places.
CREATE TYPE run_stop_reason AS ENUM (
    'completed',
    'cancelled',
    'provider_error',
    'halted',
    'budget_unreadable',
    'run_budget_exhausted',
    'user_budget_exhausted',
    'iteration_cap',
    'tool_call_cap',
    'tool_denied',
    'abandoned'
);

-- Mirrors domain.StepKind. Thinking and acting are separated because "reached the
-- iteration cap having done nothing" is a different story from "reached the
-- tool-call cap".
CREATE TYPE step_kind AS ENUM ('model', 'tool');

-- Who or what produced a message in a chat.
CREATE TYPE message_role AS ENUM ('user', 'assistant', 'system', 'tool');

-- The channels a person can talk to Wingman through. WhatsApp is absent because the
-- account-risk decision behind it is unresolved; adding it later is
-- ALTER TYPE channel_kind ADD VALUE 'whatsapp', not a table rewrite.
CREATE TYPE channel_kind AS ENUM ('telegram', 'slack', 'discord');

CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
