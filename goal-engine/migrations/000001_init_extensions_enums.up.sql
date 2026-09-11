-- Wingman goal-engine: enums and shared helpers.
--
-- Requires PostgreSQL 13+ for gen_random_uuid(). Tested against 16.

CREATE TYPE goal_status AS ENUM ('active', 'paused', 'achieved', 'missed', 'archived');

CREATE TYPE goal_comparator AS ENUM ('gte', 'lte');

-- Mirrors domain.Decision. Every evaluation is stored, so every one of these
-- values must be representable.
CREATE TYPE evaluation_decision AS ENUM (
    'noop',
    'trigger',
    'cooldown_skipped',
    'trigger_budget_exhausted',
    'achieved',
    'missed',
    'skipped_not_started',
    'skipped_inactive',
    'skipped_invalid_sample',
    'halted'
);

CREATE TYPE dispatch_status AS ENUM ('pending', 'sent', 'failed', 'abandoned');

-- The policy verdict, produced by domain.DecideApproval.
CREATE TYPE approval_outcome AS ENUM ('auto_approved', 'pending', 'denied');

-- What a human later did with a pending request.
CREATE TYPE approval_resolution AS ENUM ('approved', 'rejected', 'expired');

CREATE TYPE audit_actor_type AS ENUM ('system', 'user', 'bot');

CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
