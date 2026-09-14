package handler

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// evaluationView is the rendered form of one recorded evaluation.
//
// Every number the evaluator computed is published, not just the verdict. A
// client that received only "behind" would have to derive the arithmetic to show
// by how much, and its version would eventually disagree with the code that
// decides whether an agent is woken.
type evaluationView struct {
	ID       string `json:"id"`
	GoalID   string `json:"goalId"`
	SampleID *int64 `json:"sampleId,omitempty"`

	ObservedValue float64 `json:"observedValue"`
	TargetValue   float64 `json:"targetValue"`
	BaselineValue float64 `json:"baselineValue"`
	ExpectedValue float64 `json:"expectedValue"`
	ProgressRatio float64 `json:"progressRatio"`
	ElapsedRatio  float64 `json:"elapsedRatio"`
	PaceRatio     float64 `json:"paceRatio"`

	OnTrack   bool   `json:"onTrack"`
	TargetMet bool   `json:"targetMet"`
	Decision  string `json:"decision"`
	Reason    string `json:"reason"`

	EvaluatedAt time.Time `json:"evaluatedAt"`
	CreatedAt   time.Time `json:"createdAt"`
}

func viewEvaluation(record repository.EvaluationRecord) evaluationView {
	return evaluationView{
		ID:            record.ID,
		GoalID:        record.GoalID,
		SampleID:      record.SampleID,
		ObservedValue: record.ObservedValue,
		TargetValue:   record.TargetValue,
		BaselineValue: record.BaselineValue,
		ExpectedValue: record.ExpectedValue,
		ProgressRatio: record.ProgressRatio,
		ElapsedRatio:  record.ElapsedRatio,
		PaceRatio:     record.PaceRatio,
		OnTrack:       record.OnTrack,
		TargetMet:     record.TargetMet,
		Decision:      string(record.Decision),
		Reason:        record.Reason,
		EvaluatedAt:   record.EvaluatedAt,
		CreatedAt:     record.CreatedAt,
	}
}

func viewEvaluations(records []repository.EvaluationRecord) []evaluationView {
	views := make([]evaluationView, 0, len(records))
	for _, record := range records {
		views = append(views, viewEvaluation(record))
	}
	return views
}

// ListGoalEvaluations reads one goal's evaluation history, newest first.
//
// Readable by both roles and writable by neither, the same shape of rule the audit
// log follows: these rows are written by a monitor tick as part of reaching the
// verdict, and the service exposes no way to add one.
func (h *Handler) ListGoalEvaluations(c *gin.Context) {
	limit, offset := page(c)
	records, total, err := h.evaluations.ListByGoal(c.Request.Context(), c.Param("id"), limit, offset)
	if err != nil {
		h.respondError(c, err)
		return
	}
	paged(c, "Goal evaluations listed.", viewEvaluations(records), limit, offset, total)
}

// LatestEvaluations reads where everything stands: the newest evaluation of every
// goal that has one.
//
// A goal the monitor has not reached yet is absent rather than present with zeroed
// numbers. Zero pace would render as catastrophically behind and a pace of one as
// on track, and neither is true of a goal that has never been evaluated.
func (h *Handler) LatestEvaluations(c *gin.Context) {
	limit, offset := page(c)
	records, total, err := h.evaluations.Latest(c.Request.Context(), limit, offset)
	if err != nil {
		h.respondError(c, err)
		return
	}
	paged(c, "Latest evaluations listed.", viewEvaluations(records), limit, offset, total)
}
