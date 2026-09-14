package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// Health reports that the process is up. It touches nothing, so it stays 200 through a
// database outage — which is the point: a liveness probe that fails when a dependency does
// gets the container restarted instead of the dependency fixed.
func (h *Handler) Health(c *gin.Context) {
	utils.Success(c, http.StatusOK, "Alive.", gin.H{"status": "ok"})
}

// Ready reports whether core can do its job right now.
//
// What that means is cmd's to decide — it owns the pool and knows what a working instance
// needs — so the check is injected. Two things are deliberately *not* part of it: the goal
// engine being reachable, and the kill switch being released. An instance whose engine is
// down still has to serve the routes a person uses to see what their agent did, and an
// engaged switch was engaged on purpose. Reporting either as unready would pull core out of
// the load balancer for a condition that is not core's.
func (h *Handler) Ready(c *gin.Context) {
	if err := h.ready(c); err != nil {
		// The detail is logged, not returned. A readiness probe's body is one of the
		// easier things to end up in a public status page, and a failing pool's error
		// carries the DSN.
		h.log.Error().Err(err).Str("requestId", utils.RequestID(c)).Msg("readiness check failed")
		utils.Error(c, http.StatusServiceUnavailable, utils.ErrCodeUnavailable,
			"Dependencies are not reachable.")
		return
	}
	utils.Success(c, http.StatusOK, "Ready.", gin.H{"status": "ready"})
}

// Reference is every enumerated value a client would otherwise have to discover from
// traffic, plus which routes need an operator.
//
// It is authenticated but needs no particular role: it names no goal, no account and no
// run, and a client that cannot read it ends up hardcoding a list that drifts.
func (h *Handler) Reference(c *gin.Context) {
	utils.Success(c, http.StatusOK, "Reference values.", gin.H{
		"taskSources":  []string{string(domain.TaskSourceGoalEngine), string(domain.TaskSourceUser), string(domain.TaskSourceChannel)},
		"taskStatuses": []string{string(domain.TaskStatusQueued), string(domain.TaskStatusRunning), string(domain.TaskStatusSucceeded), string(domain.TaskStatusFailed)},
		"stopReasons":  stopReasonNames(),
		"stepKinds":    []string{string(domain.StepKindModel), string(domain.StepKindTool)},
		"messageRoles": []string{
			string(repository.MessageRoleUser),
			string(repository.MessageRoleAssistant),
			string(repository.MessageRoleSystem),
			string(repository.MessageRoleTool),
		},
		// Every platform this build can talk to, and then the ones this instance is
		// actually connected to. Both, because they answer different questions: the first
		// is what a kind field may hold, the second is what a person can usefully be told
		// to connect. A link code minted for a platform nothing is listening on can never
		// be redeemed.
		"channelKinds":      channelKindNames(),
		"channelsConnected": h.connectedChannels(),
		// So a client can tell which calls will need whoever runs the instance before it
		// tries one.
		"operatorOnlyRoutes": operatorOnlyRoutes,
	})
}

// stopReasonNames lists every way a run can end.
//
// Derived from domain.AllStopReasons rather than spelled out again here. That list is
// the closed set's one definition, and a second copy in the rendering layer is a copy
// that goes stale the first time the ladder is reordered — which is exactly the change
// nothing else would catch, because a reordering compiles and every test that only
// checks membership still passes.
//
// The order is the ladder's, not alphabetical, because that order *is* the safety
// model: domain.Decide checks in this sequence, and a client rendering them as given
// shows a reader the same precedence the code applies. The last two are the reasons the
// ladder does not produce — one decided per tool call, one recorded by the sweep for a
// run whose worker died.
func stopReasonNames() []string {
	reasons := domain.AllStopReasons()
	names := make([]string, len(reasons))
	for i, reason := range reasons {
		names[i] = string(reason)
	}
	return names
}

// channelKindNames lists every platform this build can talk to, derived from
// domain.AllChannelKinds for the same reason stopReasonNames derives its list: the closed
// set has one definition, and a second copy here would go stale the first time a platform
// is added.
func channelKindNames() []string {
	kinds := domain.AllChannelKinds()
	names := make([]string, len(kinds))
	for i, kind := range kinds {
		names[i] = string(kind)
	}
	return names
}

// connectedChannels is what cmd said is connected, never nil.
//
// An instance with no channel configured renders an empty array rather than null, because a
// client that has to distinguish "none" from "the field is missing" will get it wrong once.
func (h *Handler) connectedChannels() []string {
	if h.connected == nil {
		return []string{}
	}
	return h.connected
}
