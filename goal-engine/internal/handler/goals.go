package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// errInvalidPeriodEnd is returned when a patch's deadline will not parse. It is a
// 400 rather than a dropped field: silently ignoring an unparseable deadline
// would tell the caller their change applied when it did not.
var errInvalidPeriodEnd = errors.New("periodEnd must be an RFC 3339 timestamp")

// goalRequest is the wire shape of a new goal.
//
// It is a separate type from domain.Goal on purpose. Binding straight onto the
// domain struct would let a caller set BaselineValue and Status — the two fields
// that decide whether a goal reads as met — and the fields the engine owns must
// not be reachable from a request body at all.
type goalRequest struct {
	Product     string  `json:"product"`
	Title       string  `json:"title"`
	SourceText  string  `json:"sourceText"`
	MetricKey   string  `json:"metricKey"`
	Comparator  string  `json:"comparator"`
	TargetValue float64 `json:"targetValue"`
	// PeriodStart is optional: a goal stated without one starts now.
	PeriodStart *time.Time `json:"periodStart"`
	PeriodEnd   *time.Time `json:"periodEnd"`
	// ToleranceRatio widens the on-track band. Omitted means the default.
	ToleranceRatio float64 `json:"toleranceRatio"`
	// TriggerCooldownSeconds is expressed in seconds rather than a Go duration
	// string so a caller in any language can produce it.
	TriggerCooldownSeconds *int64 `json:"triggerCooldownSeconds"`
	MaxTriggersPerPeriod   *int   `json:"maxTriggersPerPeriod"`
	BotID                  string `json:"botId"`
	ChannelID              string `json:"channelId"`
}

// toGoal converts the request into a domain goal. Nothing is validated here —
// the service and the domain own every rule, so a caller cannot find a looser
// path in by going through a different transport.
func (r goalRequest) toGoal() domain.Goal {
	goal := domain.Goal{
		Product:        strings.TrimSpace(r.Product),
		Title:          strings.TrimSpace(r.Title),
		SourceText:     strings.TrimSpace(r.SourceText),
		MetricKey:      strings.TrimSpace(r.MetricKey),
		Comparator:     domain.Comparator(strings.TrimSpace(r.Comparator)),
		TargetValue:    r.TargetValue,
		ToleranceRatio: r.ToleranceRatio,
		BotID:          strings.TrimSpace(r.BotID),
		ChannelID:      strings.TrimSpace(r.ChannelID),
	}
	if r.PeriodStart != nil {
		goal.PeriodStart = *r.PeriodStart
	}
	if r.PeriodEnd != nil {
		goal.PeriodEnd = *r.PeriodEnd
	}
	if r.TriggerCooldownSeconds != nil {
		goal.TriggerCooldown = time.Duration(*r.TriggerCooldownSeconds) * time.Second
	}
	if r.MaxTriggersPerPeriod != nil {
		goal.MaxTriggersPerPeriod = *r.MaxTriggersPerPeriod
	}
	return goal
}

// goalPatchRequest is a partial update. Every field is a pointer so "not
// mentioned" stays distinguishable from "set to zero" — sending a zero cooldown
// is a deliberate act and has to survive the round trip.
type goalPatchRequest struct {
	Title       *string  `json:"title"`
	SourceText  *string  `json:"sourceText"`
	TargetValue *float64 `json:"targetValue"`
	// PeriodEnd is bound as a string rather than a time so an unparseable value
	// becomes a 400 instead of a field the caller believes they changed.
	PeriodEnd              *string  `json:"periodEnd"`
	Status                 *string  `json:"status"`
	ToleranceRatio         *float64 `json:"toleranceRatio"`
	TriggerCooldownSeconds *int64   `json:"triggerCooldownSeconds"`
	MaxTriggersPerPeriod   *int     `json:"maxTriggersPerPeriod"`
	BotID                  *string  `json:"botId"`
	ChannelID              *string  `json:"channelId"`
}

// toPatch converts the request into a repository patch.
func (r goalPatchRequest) toPatch() (repository.GoalPatch, error) {
	patch := repository.GoalPatch{
		Title:                r.Title,
		SourceText:           r.SourceText,
		TargetValue:          r.TargetValue,
		ToleranceRatio:       r.ToleranceRatio,
		MaxTriggersPerPeriod: r.MaxTriggersPerPeriod,
		BotID:                r.BotID,
		ChannelID:            r.ChannelID,
	}
	if r.Status != nil {
		status := domain.GoalStatus(strings.TrimSpace(*r.Status))
		patch.Status = &status
	}
	if r.TriggerCooldownSeconds != nil {
		cooldown := time.Duration(*r.TriggerCooldownSeconds) * time.Second
		patch.TriggerCooldown = &cooldown
	}
	if r.PeriodEnd != nil {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*r.PeriodEnd))
		if err != nil {
			return repository.GoalPatch{}, errInvalidPeriodEnd
		}
		patch.PeriodEnd = &parsed
	}
	return patch, nil
}

// goalView is the rendered form of a stored goal.
type goalView struct {
	ID                     string     `json:"id"`
	Product                string     `json:"product"`
	Title                  string     `json:"title"`
	SourceText             string     `json:"sourceText,omitempty"`
	MetricKey              string     `json:"metricKey"`
	Comparator             string     `json:"comparator"`
	TargetValue            float64    `json:"targetValue"`
	BaselineValue          *float64   `json:"baselineValue"`
	PeriodStart            time.Time  `json:"periodStart"`
	PeriodEnd              time.Time  `json:"periodEnd"`
	Status                 string     `json:"status"`
	ToleranceRatio         float64    `json:"toleranceRatio"`
	TriggerCooldownSeconds int64      `json:"triggerCooldownSeconds"`
	MaxTriggersPerPeriod   int        `json:"maxTriggersPerPeriod"`
	BotID                  string     `json:"botId,omitempty"`
	ChannelID              string     `json:"channelId,omitempty"`
	CreatedBy              string     `json:"createdBy"`
	CreatedAt              time.Time  `json:"createdAt"`
	UpdatedAt              *time.Time `json:"updatedAt,omitempty"`
}

func viewGoal(record repository.GoalRecord) goalView {
	view := goalView{
		ID:                     record.ID,
		Product:                record.Product,
		Title:                  record.Title,
		SourceText:             record.SourceText,
		MetricKey:              record.MetricKey,
		Comparator:             string(record.Comparator),
		TargetValue:            record.TargetValue,
		BaselineValue:          record.BaselineValue,
		PeriodStart:            record.PeriodStart,
		PeriodEnd:              record.PeriodEnd,
		Status:                 string(record.Status),
		ToleranceRatio:         record.ToleranceRatio,
		TriggerCooldownSeconds: int64(record.TriggerCooldown / time.Second),
		MaxTriggersPerPeriod:   record.MaxTriggersPerPeriod,
		BotID:                  record.BotID,
		ChannelID:              record.ChannelID,
		CreatedBy:              record.CreatedBy,
		CreatedAt:              record.CreatedAt,
	}
	if !record.UpdatedAt.IsZero() {
		updated := record.UpdatedAt
		view.UpdatedAt = &updated
	}
	return view
}

func viewGoals(records []repository.GoalRecord) []goalView {
	views := make([]goalView, 0, len(records))
	for _, record := range records {
		views = append(views, viewGoal(record))
	}
	return views
}

// CreateGoal records a new goal.
//
// Both roles may do this: an agent stating a goal it has worked out from a
// conversation is the loop the whole service exists for. Changing one afterwards
// is a different matter — see PatchGoal.
func (h *Handler) CreateGoal(c *gin.Context) {
	var req goalRequest
	if !bindJSON(c, &req) {
		return
	}

	record, err := h.goals.Create(c.Request.Context(), req.toGoal(), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusCreated, "Goal created.", viewGoal(record))
}

// GetGoal reads one goal.
func (h *Handler) GetGoal(c *gin.Context) {
	record, err := h.goals.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Goal found.", viewGoal(record))
}

// ListGoals reads a page of goals.
func (h *Handler) ListGoals(c *gin.Context) {
	limit, offset := page(c)
	filter := repository.GoalFilter{
		Product:   strings.TrimSpace(c.Query("product")),
		Status:    domain.GoalStatus(strings.TrimSpace(c.Query("status"))),
		MetricKey: strings.TrimSpace(c.Query("metricKey")),
		Search:    strings.TrimSpace(c.Query("search")),
		Limit:     limit,
		Offset:    offset,
	}

	records, total, err := h.goals.List(c.Request.Context(), filter)
	if err != nil {
		h.respondError(c, err)
		return
	}
	paged(c, "Goals listed.", viewGoals(records), limit, offset, total)
}

// PatchGoal changes an existing goal. Operator only, enforced both on the route
// and in the service: lowering a target and meeting it are indistinguishable
// afterwards, so an agent may not do either.
func (h *Handler) PatchGoal(c *gin.Context) {
	var req goalPatchRequest
	if !bindJSON(c, &req) {
		return
	}
	patch, err := req.toPatch()
	if err != nil {
		badRequest(c, err.Error())
		return
	}

	record, err := h.goals.Patch(c.Request.Context(), c.Param("id"), patch, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Goal updated.", viewGoal(record))
}
