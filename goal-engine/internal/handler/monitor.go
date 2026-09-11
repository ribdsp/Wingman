package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
	"github.com/ribdsp/wingman/goal-engine/internal/service"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// tickView is the rendered result of one monitor pass.
type tickView struct {
	StartedAt time.Time `json:"startedAt"`
	// DurationMs is milliseconds rather than a Go duration string so a dashboard
	// can chart it without parsing.
	DurationMs int64 `json:"durationMs"`
	// Halted is true when the kill switch stopped the tick before any goal was
	// examined. It is not an error: it is the switch working.
	Halted    bool           `json:"halted"`
	Checked   int            `json:"checked"`
	Triggered int            `json:"triggered"`
	Settled   int            `json:"settled"`
	Failed    int            `json:"failed"`
	Decisions map[string]int `json:"decisions"`
	Errors    []string       `json:"errors,omitempty"`
}

func viewTick(result service.TickResult) tickView {
	decisions := make(map[string]int, len(result.Decisions))
	for decision, count := range result.Decisions {
		decisions[string(decision)] = count
	}
	return tickView{
		StartedAt:  result.StartedAt,
		DurationMs: result.Duration.Milliseconds(),
		Halted:     result.Halted,
		Checked:    result.Checked,
		Triggered:  result.Triggered,
		Settled:    result.Settled,
		Failed:     result.Failed,
		Decisions:  decisions,
		Errors:     result.Errors,
	}
}

// Tick runs one monitor pass now.
//
// The scheduled loop calls the same service method; this endpoint exists because
// "wait an hour to see whether the goal you just created behaves" is not a usable
// way to set one up. It answers 200 with the result even when individual goals
// failed — a tick that checked ten goals and could not read one metric is a
// partial success, and the caller needs the counts to tell which.
func (h *Handler) Tick(c *gin.Context) {
	result, err := h.monitor.Tick(c.Request.Context())
	if err != nil {
		h.respondError(c, err)
		return
	}

	message := "Monitor tick complete."
	if result.Halted {
		message = "Monitor halted by the kill switch. No goals were examined."
	}
	utils.Success(c, http.StatusOK, message, viewTick(result))
}

// Health reports that the process is up.
//
// It deliberately touches nothing: a liveness probe that queries the database
// restarts a healthy process whenever the database hiccups, which turns a brief
// outage into a restart loop. Readiness is the check that talks to dependencies.
func (h *Handler) Health(c *gin.Context) {
	utils.Success(c, http.StatusOK, "Alive.", gin.H{"status": "ok"})
}

// Ready reports whether the service can do its job right now.
//
// It reads the kill switch, which is the cheapest call that exercises the whole
// path this service depends on: the database is reachable, the flag table is
// readable, and the switch's state is known. An engaged switch is still ready —
// the operator engaged it on purpose, and reporting it as unready would take the
// service out of the load balancer and with it the endpoint that releases the
// switch.
func (h *Handler) Ready(c *gin.Context) {
	flag, err := h.flags.KillSwitch(c.Request.Context())
	if err != nil {
		h.log.Error().Err(err).Msg("readiness check failed")
		utils.Error(c, http.StatusServiceUnavailable, utils.ErrCodeUnavailable,
			"Dependencies are not reachable.")
		return
	}
	utils.Success(c, http.StatusOK, "Ready.", gin.H{
		"status":            "ready",
		"killSwitchEngaged": flag.Enabled,
		"metrics":           h.metrics.Len(),
	})
}

// decisionNames lists every decision the engine can reach, so a client building a
// dashboard does not have to discover them from traffic.
func decisionNames() []string {
	return []string{
		string(domain.DecisionNoop),
		string(domain.DecisionTrigger),
		string(domain.DecisionCooldownSkipped),
		string(domain.DecisionTriggerBudgetExhausted),
		string(domain.DecisionAchieved),
		string(domain.DecisionMissed),
		string(domain.DecisionSkippedNotStarted),
		string(domain.DecisionSkippedInactive),
		string(domain.DecisionSkippedInvalidSample),
		string(domain.DecisionHalted),
	}
}

// Reference reports the enumerations that make up this API's contract.
//
// It exists so a client is not obliged to hardcode string sets that the engine
// owns. Everything here is already public knowledge to an authenticated caller.
func (h *Handler) Reference(c *gin.Context) {
	utils.Success(c, http.StatusOK, "Reference values.", gin.H{
		"goalStatuses": []string{
			string(domain.GoalStatusActive),
			string(domain.GoalStatusPaused),
			string(domain.GoalStatusAchieved),
			string(domain.GoalStatusMissed),
			string(domain.GoalStatusArchived),
		},
		"comparators":         []string{string(domain.ComparatorGTE), string(domain.ComparatorLTE)},
		"decisions":           decisionNames(),
		"approvalOutcomes":    []string{string(domain.ApprovalAutoApproved), string(domain.ApprovalPending), string(domain.ApprovalDenied)},
		"approvalResolutions": []string{string(repository.ResolutionApproved), string(repository.ResolutionRejected)},
		// So a client can tell which calls will need a human before it tries one.
		"operatorOnlyRoutes": operatorOnlyRoutes,
	})
}
