package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// runView is one execution of a task.
//
// provider and model record what actually answered, not what was asked for, and they are
// rendered for that reason: a run that fell back to a cheaper model produced output that
// has to be read differently.
type runView struct {
	ID     string `json:"id"`
	TaskID string `json:"taskId"`
	// Provider and Model are empty on a run that stopped before it reached one — halted
	// by the kill switch, say.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Stop is empty while the run is in flight, which is what isInFlight renders
	// explicitly so a client does not have to know that.
	Stop        string        `json:"stop,omitempty"`
	Reason      string        `json:"reason,omitempty"`
	IsInFlight  bool          `json:"isInFlight"`
	IsCancelled bool          `json:"isCancelled"`
	Limits      runLimitsView `json:"limits"`
	State       runStateView  `json:"state"`
	StartedAt   time.Time     `json:"startedAt"`
	FinishedAt  *time.Time    `json:"finishedAt"`
}

// runLimitsView is what bounded the run.
//
// It is rendered with the run rather than read from config, because the limits are
// snapshotted when the run starts: an operator who raised a cap yesterday should still see
// what last week's run was actually held to.
type runLimitsView struct {
	MaxIterations int   `json:"maxIterations"`
	MaxToolCalls  int   `json:"maxToolCalls"`
	MaxTokensRun  int64 `json:"maxTokensPerRun"`
	// MaxTokensUserDay is null when this instance sets no daily cap, rather than 0 or a
	// sentinel number. Zero would read as "nothing allowed", which is the opposite.
	MaxTokensUserDay *int64 `json:"maxTokensPerUserDay"`
	// The two timeouts are rendered in seconds. A Go duration marshals as nanoseconds,
	// which no client would read correctly by accident.
	StepTimeoutSeconds    float64 `json:"stepTimeoutSeconds"`
	SandboxTimeoutSeconds float64 `json:"sandboxTimeoutSeconds"`
}

// runStateView is what the run consumed.
type runStateView struct {
	Iterations int   `json:"iterations"`
	ToolCalls  int   `json:"toolCalls"`
	TokensUsed int64 `json:"tokensUsed"`
}

func viewRun(record repository.RunRecord) runView {
	run := record.Run

	limits := runLimitsView{
		MaxIterations:         run.Limits.MaxIterations,
		MaxToolCalls:          run.Limits.MaxToolCalls,
		MaxTokensRun:          run.Limits.MaxTokensPerRun,
		StepTimeoutSeconds:    run.Limits.StepTimeout.Seconds(),
		SandboxTimeoutSeconds: run.Limits.SandboxTimeout.Seconds(),
	}
	if run.Limits.MaxTokensPerUserDay != domain.NoUserDailyCap {
		daily := run.Limits.MaxTokensPerUserDay
		limits.MaxTokensUserDay = &daily
	}

	return runView{
		ID:          run.ID,
		TaskID:      run.TaskID,
		Provider:    run.Provider,
		Model:       run.Model,
		Stop:        string(run.Stop),
		Reason:      run.Reason,
		IsInFlight:  run.InFlight(),
		IsCancelled: record.Cancelled(),
		Limits:      limits,
		State: runStateView{
			Iterations: run.State.Iterations,
			ToolCalls:  run.State.ToolCalls,
			TokensUsed: run.State.TokensUsed,
		},
		StartedAt:  run.StartedAt,
		FinishedAt: run.FinishedAt,
	}
}

func viewRuns(records []repository.RunRecord) []runView {
	views := make([]runView, 0, len(records))
	for _, record := range records {
		views = append(views, viewRun(record))
	}
	return views
}

// stepView is one line of a transcript.
//
// err is rendered because a step that failed is part of the record. The service layer is
// what guarantees it is short and carries no provider payload — see domain.Step, where
// that rule is written down.
type stepView struct {
	Index    int    `json:"index"`
	Kind     string `json:"kind"`
	ToolName string `json:"toolName,omitempty"`
	Content  string `json:"content"`
	Err      string `json:"err,omitempty"`
	// TokensIn and TokensOut are both zero on a tool step. A sandbox does not bill tokens.
	TokensIn  int64     `json:"tokensIn"`
	TokensOut int64     `json:"tokensOut"`
	At        time.Time `json:"at"`
}

func viewStep(step domain.Step) stepView {
	return stepView{
		Index:     step.Index,
		Kind:      string(step.Kind),
		ToolName:  step.ToolName,
		Content:   step.Content,
		Err:       step.Err,
		TokensIn:  step.TokensIn,
		TokensOut: step.TokensOut,
		At:        step.At,
	}
}

func viewSteps(steps []domain.Step) []stepView {
	views := make([]stepView, 0, len(steps))
	for _, step := range steps {
		views = append(views, viewStep(step))
	}
	return views
}

// spendView is one charge against a run.
type spendView struct {
	RunID      string    `json:"runId"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	TokensIn   int64     `json:"tokensIn"`
	TokensOut  int64     `json:"tokensOut"`
	Total      int64     `json:"total"`
	OccurredAt time.Time `json:"occurredAt"`
}

func viewSpend(spend repository.Spend) spendView {
	return spendView{
		RunID:      spend.RunID,
		Provider:   spend.Provider,
		Model:      spend.Model,
		TokensIn:   spend.TokensIn,
		TokensOut:  spend.TokensOut,
		Total:      spend.Total(),
		OccurredAt: spend.OccurredAt,
	}
}

// costView is a run's charges and what they add up to.
type costView struct {
	Charges []spendView `json:"charges"`
	Total   int64       `json:"total"`
}

// ledgerView is one account's spend today.
//
// isReadable is rendered rather than hidden. A false is not "nothing spent" — it is "no
// answer", and a client that showed 0 for it would tell somebody their budget is untouched
// when what happened is that the counter could not be read.
type ledgerView struct {
	IsReadable  bool  `json:"isReadable"`
	TokensToday int64 `json:"tokensToday"`
}

// GetRun reads one run.
func (h *Handler) GetRun(c *gin.Context) {
	record, err := h.runs.Get(c.Request.Context(), c.Param("id"), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Run found.", viewRun(record))
}

// RunSteps renders a run's transcript, oldest step first.
func (h *Handler) RunSteps(c *gin.Context) {
	limit, offset := page(c)

	steps, err := h.runs.Steps(c.Request.Context(), c.Param("id"), limit, offset, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	// No total: the service does not count the transcript, and a pagination block with an
	// invented total is worse than none — a client paging on it would stop early.
	utils.Success(c, http.StatusOK, "Transcript listed.", viewSteps(steps))
}

// TaskRuns renders the runs of one task. There is usually one; a retry after a provider
// outage is a second run rather than an edit of the first.
func (h *Handler) TaskRuns(c *gin.Context) {
	limit, offset := page(c)

	records, err := h.runs.ListForTask(c.Request.Context(), c.Param("id"), limit, offset, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Runs listed.", viewRuns(records))
}

// RunCost renders what a run charged.
func (h *Handler) RunCost(c *gin.Context) {
	charges, err := h.runs.Cost(c.Request.Context(), c.Param("id"), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}

	view := costView{Charges: make([]spendView, 0, len(charges))}
	for _, charge := range charges {
		view.Charges = append(view.Charges, viewSpend(charge))
		view.Total += charge.Total()
	}
	utils.Success(c, http.StatusOK, "Cost listed.", view)
}

// CancelRun asks a run to stop at its next step boundary.
//
// A request rather than a kill, so the transcript and the counters stay consistent with
// what was actually spent. Asking twice is 200 — somebody clicking again because nothing
// has visibly happened is not making a mistake. A run that already finished is 409, which
// is the one case where the answer is "that is not a thing you can do now".
func (h *Handler) CancelRun(c *gin.Context) {
	if err := h.runs.Cancel(c.Request.Context(), c.Param("id"), actorFrom(c)); err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Cancellation requested. The run stops at its next step.",
		gin.H{"id": c.Param("id"), "cancelRequested": true})
}

// Ledger renders the caller's own token spend for today, operators included: the ledger is
// per account, so an operator asking without naming one would be asking a question with no
// answer. Instance-wide totals belong to the goal engine, through the samples core pushes.
func (h *Handler) Ledger(c *gin.Context) {
	ledger, err := h.runs.Ledger(c.Request.Context(), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Ledger read.", ledgerView{
		IsReadable:  ledger.Readable,
		TokensToday: ledger.TokensToday,
	})
}
