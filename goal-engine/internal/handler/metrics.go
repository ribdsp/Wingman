package handler

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// metricView is what the API is willing to say about a metric.
//
// The four fields here are deliberately all of them. A metrics.Definition also
// carries the SQL query it runs, the datasource it runs against, the internal URL
// it fetches and the header it authenticates with — publishing any of those hands
// a caller the shape of the production schema or an endpoint reachable from inside
// the network. The registry is operator-owned and read-only for the life of the
// process; this endpoint exists so a client can discover which metric keys a goal
// may reference, and nothing more.
type metricView struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Source      string `json:"source"`
}

func viewMetric(def metrics.Definition) metricView {
	return metricView{
		Key:         def.Key,
		Description: def.Description,
		Unit:        def.Unit,
		Source:      string(def.Source),
	}
}

// ListMetrics reads the declared metric registry.
//
// There is no create or update counterpart, and there will not be one: a metric
// definition contains a query, so an agent that could write one could read
// anything the datasource user can reach. Metrics are declared in the operator's
// YAML file and loaded once at startup.
func (h *Handler) ListMetrics(c *gin.Context) {
	defs := h.metrics.Definitions()
	views := make([]metricView, 0, len(defs))
	for _, def := range defs {
		views = append(views, viewMetric(def))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Key < views[j].Key })

	utils.Success(c, http.StatusOK, "Metrics listed.", views)
}

// GetMetric reads one declared metric.
func (h *Handler) GetMetric(c *gin.Context) {
	def, ok := h.metrics.Get(c.Param("key"))
	if !ok {
		// The message names the key the caller asked for, which is theirs already.
		// It does not list the keys that do exist: that is what ListMetrics is for,
		// and it is an authenticated call rather than a 404 body.
		utils.Error(c, http.StatusNotFound, utils.ErrCodeUnknownMetric, "No such metric is declared.")
		return
	}
	utils.Success(c, http.StatusOK, "Metric found.", viewMetric(def))
}
