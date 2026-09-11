package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
)

// GoalRecord is a stored goal together with its storage metadata.
type GoalRecord struct {
	domain.Goal
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// GoalFilter narrows a goal listing.
type GoalFilter struct {
	Product   string
	Status    domain.GoalStatus
	MetricKey string
	// Search matches the title or the operator's original phrasing.
	Search string
	Limit  int
	Offset int
}

// GoalPatch is a partial update. Nil fields are left alone, which keeps an
// update from silently resetting a field the caller did not mention.
type GoalPatch struct {
	Title                *string
	SourceText           *string
	TargetValue          *float64
	PeriodEnd            *time.Time
	Status               *domain.GoalStatus
	ToleranceRatio       *float64
	TriggerCooldown      *time.Duration
	MaxTriggersPerPeriod *int
	BotID                *string
	ChannelID            *string
}

// IsEmpty reports whether the patch would change nothing.
func (p GoalPatch) IsEmpty() bool {
	return p.Title == nil && p.SourceText == nil && p.TargetValue == nil &&
		p.PeriodEnd == nil && p.Status == nil && p.ToleranceRatio == nil &&
		p.TriggerCooldown == nil && p.MaxTriggersPerPeriod == nil &&
		p.BotID == nil && p.ChannelID == nil
}

// GoalRepository stores the goal registry.
type GoalRepository struct {
	db *sqlx.DB
}

// NewGoalRepository builds a repository over the given pool.
func NewGoalRepository(db *sqlx.DB) *GoalRepository {
	return &GoalRepository{db: db}
}

const goalColumns = `id, product, title, source_text, metric_key, comparator,
	target_value, baseline_value, period_start, period_end, status,
	tolerance_ratio, trigger_cooldown_seconds, max_triggers_per_period,
	bot_id, channel_id, created_by, created_at, updated_at`

// goalRow mirrors the goals table.
type goalRow struct {
	ID                     string          `db:"id"`
	Product                string          `db:"product"`
	Title                  string          `db:"title"`
	SourceText             string          `db:"source_text"`
	MetricKey              string          `db:"metric_key"`
	Comparator             string          `db:"comparator"`
	TargetValue            float64         `db:"target_value"`
	BaselineValue          sql.NullFloat64 `db:"baseline_value"`
	PeriodStart            time.Time       `db:"period_start"`
	PeriodEnd              time.Time       `db:"period_end"`
	Status                 string          `db:"status"`
	ToleranceRatio         float64         `db:"tolerance_ratio"`
	TriggerCooldownSeconds int             `db:"trigger_cooldown_seconds"`
	MaxTriggersPerPeriod   int             `db:"max_triggers_per_period"`
	BotID                  string          `db:"bot_id"`
	ChannelID              string          `db:"channel_id"`
	CreatedBy              string          `db:"created_by"`
	CreatedAt              time.Time       `db:"created_at"`
	UpdatedAt              time.Time       `db:"updated_at"`
}

func (r goalRow) toRecord() GoalRecord {
	var baseline *float64
	if r.BaselineValue.Valid {
		value := r.BaselineValue.Float64
		baseline = &value
	}
	return GoalRecord{
		Goal: domain.Goal{
			ID:                   r.ID,
			Product:              r.Product,
			Title:                r.Title,
			SourceText:           r.SourceText,
			MetricKey:            r.MetricKey,
			Comparator:           domain.Comparator(r.Comparator),
			TargetValue:          r.TargetValue,
			BaselineValue:        baseline,
			PeriodStart:          r.PeriodStart,
			PeriodEnd:            r.PeriodEnd,
			Status:               domain.GoalStatus(r.Status),
			ToleranceRatio:       r.ToleranceRatio,
			TriggerCooldown:      time.Duration(r.TriggerCooldownSeconds) * time.Second,
			MaxTriggersPerPeriod: r.MaxTriggersPerPeriod,
			BotID:                r.BotID,
			ChannelID:            r.ChannelID,
		},
		CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

// Create stores a new goal and returns it as stored, including defaults the
// database filled in.
func (r *GoalRepository) Create(ctx context.Context, goal domain.Goal, createdBy string) (GoalRecord, error) {
	const query = `
		INSERT INTO goals (
			product, title, source_text, metric_key, comparator, target_value,
			baseline_value, period_start, period_end, status, tolerance_ratio,
			trigger_cooldown_seconds, max_triggers_per_period, bot_id, channel_id,
			created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING ` + goalColumns

	var baseline any
	if goal.BaselineValue != nil {
		baseline = *goal.BaselineValue
	}
	status := goal.Status
	if status == "" {
		status = domain.GoalStatusActive
	}
	tolerance := goal.ToleranceRatio
	if tolerance <= 0 {
		tolerance = domain.DefaultToleranceRatio
	}

	var row goalRow
	err := r.db.QueryRowxContext(ctx, query,
		goal.Product, goal.Title, goal.SourceText, goal.MetricKey, string(goal.Comparator),
		goal.TargetValue, baseline, goal.PeriodStart, goal.PeriodEnd, string(status),
		tolerance, int(goal.TriggerCooldown/time.Second), goal.MaxTriggersPerPeriod,
		goal.BotID, goal.ChannelID, createdBy,
	).StructScan(&row)
	if err != nil {
		return GoalRecord{}, fmt.Errorf("create goal: %w", classify(err))
	}
	return row.toRecord(), nil
}

// GetByID returns one goal, or ErrNotFound.
func (r *GoalRepository) GetByID(ctx context.Context, id string) (GoalRecord, error) {
	const query = `SELECT ` + goalColumns + ` FROM goals WHERE id = $1`

	var row goalRow
	if err := r.db.QueryRowxContext(ctx, query, id).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GoalRecord{}, ErrNotFound
		}
		return GoalRecord{}, fmt.Errorf("get goal %s: %w", id, classify(err))
	}
	return row.toRecord(), nil
}

// List returns a page of goals plus the total number of matches.
func (r *GoalRepository) List(ctx context.Context, filter GoalFilter) ([]GoalRecord, int, error) {
	where := []string{"1 = 1"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if filter.Product != "" {
		add("product = $%d", filter.Product)
	}
	if filter.Status != "" {
		add("status = $%d", string(filter.Status))
	}
	if filter.MetricKey != "" {
		add("metric_key = $%d", filter.MetricKey)
	}
	if trimmed := strings.TrimSpace(filter.Search); trimmed != "" {
		// One value, two placeholders, so this clause is built directly.
		args = append(args, "%"+trimmed+"%")
		where = append(where, fmt.Sprintf("(title ILIKE $%d OR source_text ILIKE $%d)", len(args), len(args)))
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM goals WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count goals: %w", classify(err))
	}

	limit, offset := normalisePage(filter.Limit, filter.Offset)
	args = append(args, limit, offset)
	query := fmt.Sprintf(
		`SELECT %s FROM goals WHERE %s ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		goalColumns, clause, len(args)-1, len(args),
	)

	rows := []goalRow{}
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, 0, fmt.Errorf("list goals: %w", classify(err))
	}

	records := make([]GoalRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, row.toRecord())
	}
	return records, total, nil
}

// ListDue returns active goals whose period has started, oldest deadline first.
// Goals whose period has already closed are included on purpose: the monitor
// still has to settle them as achieved or missed.
func (r *GoalRepository) ListDue(ctx context.Context, now time.Time, limit int) ([]GoalRecord, error) {
	const query = `
		SELECT ` + goalColumns + `
		FROM goals
		WHERE status = 'active' AND period_start <= $1
		ORDER BY period_end ASC, id ASC
		LIMIT $2`

	if limit <= 0 {
		limit = defaultPageLimit
	}
	rows := []goalRow{}
	if err := r.db.SelectContext(ctx, &rows, query, now, limit); err != nil {
		return nil, fmt.Errorf("list due goals: %w", classify(err))
	}

	records := make([]GoalRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, row.toRecord())
	}
	return records, nil
}

// SetBaseline records the opening value of the period. It only ever writes once:
// a baseline that moved would silently rewrite the definition of progress.
func (r *GoalRepository) SetBaseline(ctx context.Context, id string, value float64) error {
	const query = `UPDATE goals SET baseline_value = $2 WHERE id = $1 AND baseline_value IS NULL`

	if _, err := r.db.ExecContext(ctx, query, id, value); err != nil {
		return fmt.Errorf("set baseline for goal %s: %w", id, classify(err))
	}
	return nil
}

// UpdateStatus moves a goal to a new lifecycle state.
func (r *GoalRepository) UpdateStatus(ctx context.Context, id string, status domain.GoalStatus) error {
	const query = `UPDATE goals SET status = $2 WHERE id = $1`

	result, err := r.db.ExecContext(ctx, query, id, string(status))
	if err != nil {
		return fmt.Errorf("update goal %s status: %w", id, classify(err))
	}
	return requireOneRow(result, id)
}

// Patch applies a partial update and returns the goal as stored.
func (r *GoalRepository) Patch(ctx context.Context, id string, patch GoalPatch) (GoalRecord, error) {
	if patch.IsEmpty() {
		return r.GetByID(ctx, id)
	}

	sets := []string{}
	args := []any{id}
	set := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}

	if patch.Title != nil {
		set("title", *patch.Title)
	}
	if patch.SourceText != nil {
		set("source_text", *patch.SourceText)
	}
	if patch.TargetValue != nil {
		set("target_value", *patch.TargetValue)
	}
	if patch.PeriodEnd != nil {
		set("period_end", *patch.PeriodEnd)
	}
	if patch.Status != nil {
		set("status", string(*patch.Status))
	}
	if patch.ToleranceRatio != nil {
		set("tolerance_ratio", *patch.ToleranceRatio)
	}
	if patch.TriggerCooldown != nil {
		set("trigger_cooldown_seconds", int(*patch.TriggerCooldown/time.Second))
	}
	if patch.MaxTriggersPerPeriod != nil {
		set("max_triggers_per_period", *patch.MaxTriggersPerPeriod)
	}
	if patch.BotID != nil {
		set("bot_id", *patch.BotID)
	}
	if patch.ChannelID != nil {
		set("channel_id", *patch.ChannelID)
	}

	query := fmt.Sprintf(
		`UPDATE goals SET %s WHERE id = $1 RETURNING %s`,
		strings.Join(sets, ", "), goalColumns,
	)

	var row goalRow
	if err := r.db.QueryRowxContext(ctx, query, args...).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GoalRecord{}, ErrNotFound
		}
		return GoalRecord{}, fmt.Errorf("patch goal %s: %w", id, classify(err))
	}
	return row.toRecord(), nil
}
