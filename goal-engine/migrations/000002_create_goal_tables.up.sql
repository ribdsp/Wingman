-- Goal registry, metric samples, evaluations and trigger dispatches.

CREATE TABLE goals (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    product                  text NOT NULL,
    title                    text NOT NULL,
    -- The operator's own words, kept so an agent can be briefed in them.
    source_text              text NOT NULL DEFAULT '',
    -- References a key in the operator-declared metric registry. Goals never
    -- carry SQL of their own; see docs/goal-engine.md for why.
    metric_key               text NOT NULL,
    comparator               goal_comparator NOT NULL,
    target_value             double precision NOT NULL,
    -- NULL until the first observation of the period captures it.
    baseline_value           double precision,
    period_start             timestamptz NOT NULL,
    period_end               timestamptz NOT NULL,
    status                   goal_status NOT NULL DEFAULT 'active',
    tolerance_ratio          double precision NOT NULL DEFAULT 0.05,
    trigger_cooldown_seconds integer NOT NULL DEFAULT 21600,
    -- 0 means unlimited triggers per period.
    max_triggers_per_period  integer NOT NULL DEFAULT 3,
    bot_id                   text NOT NULL,
    channel_id               text NOT NULL,
    created_by               text NOT NULL DEFAULT 'system',
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT goals_period_valid CHECK (period_end > period_start),
    CONSTRAINT goals_tolerance_valid CHECK (tolerance_ratio >= 0 AND tolerance_ratio <= 1),
    CONSTRAINT goals_cooldown_valid CHECK (trigger_cooldown_seconds >= 0),
    CONSTRAINT goals_max_triggers_valid CHECK (max_triggers_per_period >= 0),
    CONSTRAINT goals_target_finite CHECK (target_value <> 'NaN'::double precision),
    CONSTRAINT goals_product_not_blank CHECK (btrim(product) <> ''),
    CONSTRAINT goals_metric_key_not_blank CHECK (btrim(metric_key) <> '')
);

CREATE TRIGGER goals_set_updated_at
    BEFORE UPDATE ON goals
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Due goals are read on every monitor tick, so index the exact predicate used.
CREATE INDEX goals_active_period_idx ON goals (period_end) WHERE status = 'active';
CREATE INDEX goals_product_idx ON goals (product);
CREATE INDEX goals_metric_key_idx ON goals (metric_key);

-- Raw metric observations. Kept as a time series so a human can audit what the
-- monitor actually saw when it decided to wake an agent.
CREATE TABLE metric_samples (
    id          bigserial PRIMARY KEY,
    metric_key  text NOT NULL,
    value       double precision NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now(),
    source      text NOT NULL DEFAULT 'monitor',
    duration_ms integer,

    -- PostgreSQL treats NaN as equal to NaN, so these comparisons do reject
    -- non-finite samples.
    CONSTRAINT metric_samples_value_finite CHECK (
        value <> 'NaN'::double precision
        AND value <> 'Infinity'::double precision
        AND value <> '-Infinity'::double precision
    )
);

CREATE INDEX metric_samples_key_time_idx ON metric_samples (metric_key, observed_at DESC);

-- One row per goal check, whether or not it triggered anything. This is the
-- business audit trail for autonomous decisions.
CREATE TABLE goal_evaluations (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    goal_id        uuid NOT NULL REFERENCES goals (id) ON DELETE CASCADE,
    sample_id      bigint REFERENCES metric_samples (id) ON DELETE SET NULL,
    observed_value double precision NOT NULL,
    target_value   double precision NOT NULL,
    baseline_value double precision NOT NULL,
    expected_value double precision NOT NULL,
    progress_ratio double precision NOT NULL,
    elapsed_ratio  double precision NOT NULL,
    pace_ratio     double precision NOT NULL,
    on_track       boolean NOT NULL,
    target_met     boolean NOT NULL,
    decision       evaluation_decision NOT NULL,
    reason         text NOT NULL,
    evaluated_at   timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX goal_evaluations_goal_time_idx ON goal_evaluations (goal_id, evaluated_at DESC);
CREATE INDEX goal_evaluations_decision_time_idx ON goal_evaluations (decision, evaluated_at DESC);

-- Each attempt to wake an agent through the Wingman API.
CREATE TABLE trigger_dispatches (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    goal_id          uuid NOT NULL REFERENCES goals (id) ON DELETE CASCADE,
    evaluation_id    uuid NOT NULL REFERENCES goal_evaluations (id) ON DELETE CASCADE,
    bot_id           text NOT NULL,
    channel_id       text NOT NULL,
    -- Derived from the goal and its period so a retry can never create a
    -- second agent task for the same shortfall.
    idempotency_key  text NOT NULL UNIQUE,
    brief            text NOT NULL,
    request_payload  jsonb NOT NULL DEFAULT '{}'::jsonb,
    status           dispatch_status NOT NULL DEFAULT 'pending',
    attempts         integer NOT NULL DEFAULT 0,
    response_status  integer,
    external_task_id text,
    last_error       text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    sent_at          timestamptz
);

CREATE INDEX trigger_dispatches_goal_time_idx ON trigger_dispatches (goal_id, created_at DESC);
CREATE INDEX trigger_dispatches_retryable_idx ON trigger_dispatches (created_at)
    WHERE status IN ('pending', 'failed');
