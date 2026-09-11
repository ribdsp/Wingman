-- Governance: approvals, the spend ledger, the business audit log and the
-- kill switch.

-- Money is stored as numeric so the ledger is exact. It is converted to float64
-- only at the moment a threshold is compared, which is safe for any amount
-- below 2^53; see docs/goal-engine.md.
CREATE TABLE approval_requests (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    action_type     text NOT NULL,
    amount          numeric(20, 4) NOT NULL,
    currency        char(3) NOT NULL,
    -- The bot that asked. Never trusted for policy decisions, only recorded.
    requested_by    text NOT NULL,
    goal_id         uuid REFERENCES goals (id) ON DELETE SET NULL,
    idempotency_key text NOT NULL UNIQUE,
    outcome         approval_outcome NOT NULL,
    policy_reason   text NOT NULL,
    resolution      approval_resolution,
    resolved_by     text,
    resolved_at     timestamptz,
    resolution_note text NOT NULL DEFAULT '',
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz,

    CONSTRAINT approval_requests_amount_positive CHECK (amount > 0),
    CONSTRAINT approval_requests_currency_upper CHECK (currency = upper(currency)),
    -- Only a pending request can be resolved by a human, and a resolution must
    -- always record who and when.
    CONSTRAINT approval_requests_resolution_valid CHECK (
        (resolution IS NULL AND resolved_by IS NULL AND resolved_at IS NULL)
        OR (resolution IS NOT NULL AND resolved_at IS NOT NULL AND outcome = 'pending')
    )
);

CREATE INDEX approval_requests_open_idx ON approval_requests (created_at DESC)
    WHERE outcome = 'pending' AND resolution IS NULL;
CREATE INDEX approval_requests_action_time_idx ON approval_requests (action_type, created_at DESC);

-- Committed spend, used to enforce daily caps.
CREATE TABLE spend_ledger (
    id          bigserial PRIMARY KEY,
    action_type text NOT NULL,
    amount      numeric(20, 4) NOT NULL,
    currency    char(3) NOT NULL,
    approval_id uuid REFERENCES approval_requests (id) ON DELETE SET NULL,
    bot_id      text NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL DEFAULT now(),
    note        text NOT NULL DEFAULT '',

    CONSTRAINT spend_ledger_amount_positive CHECK (amount > 0),
    CONSTRAINT spend_ledger_currency_upper CHECK (currency = upper(currency))
);

CREATE INDEX spend_ledger_action_time_idx ON spend_ledger (action_type, occurred_at DESC);

-- The business audit log: deliberately separate from Wingman core's internal
-- logs so it can be reviewed and retained on its own terms.
CREATE TABLE audit_events (
    id           bigserial PRIMARY KEY,
    at           timestamptz NOT NULL DEFAULT now(),
    actor_type   audit_actor_type NOT NULL,
    actor_id     text NOT NULL DEFAULT '',
    action       text NOT NULL,
    subject_type text NOT NULL,
    subject_id   text NOT NULL DEFAULT '',
    outcome      text NOT NULL DEFAULT '',
    detail       jsonb NOT NULL DEFAULT '{}'::jsonb,
    request_id   text NOT NULL DEFAULT ''
);

CREATE INDEX audit_events_at_idx ON audit_events (at DESC);
CREATE INDEX audit_events_subject_idx ON audit_events (subject_type, subject_id, at DESC);
CREATE INDEX audit_events_action_idx ON audit_events (action, at DESC);

-- Operational flags. The kill switch lives here rather than in an env var so it
-- can be flipped without a redeploy.
CREATE TABLE system_flags (
    key        text PRIMARY KEY,
    enabled    boolean NOT NULL DEFAULT false,
    reason     text NOT NULL DEFAULT '',
    updated_by text NOT NULL DEFAULT 'system',
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER system_flags_set_updated_at
    BEFORE UPDATE ON system_flags
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO system_flags (key, enabled, reason)
VALUES ('kill_switch', false, 'seeded disabled at install');
