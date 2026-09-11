package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/goal-engine/internal/repository"
	"github.com/ribdsp/wingman/goal-engine/internal/service"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// errInvalidObservedAt is returned before the service is called, so a caller who
// meant to backdate a value learns their timestamp was rejected rather than
// silently replaced with now.
var errInvalidObservedAt = errors.New("observedAt must be an RFC 3339 timestamp")

// sampleRequest is one reported observation of a push metric.
//
// Value is a pointer because zero is a real reading — "no tickets closed today"
// is exactly the number a goal needs to see — and an absent field must not arrive
// as one.
type sampleRequest struct {
	Value *float64 `json:"value"`
	// ObservedAt is bound as a string for the same reason a goal's periodEnd is:
	// an unparseable timestamp has to become a 400, not a value the caller believes
	// they set.
	ObservedAt *string `json:"observedAt"`
	Note       string  `json:"note"`
}

// toService converts the request, leaving the value's own validity to the service.
func (r sampleRequest) toService(metricKey string) (service.SampleRequest, error) {
	req := service.SampleRequest{
		MetricKey: metricKey,
		Note:      strings.TrimSpace(r.Note),
	}
	if r.Value != nil {
		req.Value = *r.Value
	}
	if r.ObservedAt != nil {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*r.ObservedAt))
		if err != nil {
			return service.SampleRequest{}, errInvalidObservedAt
		}
		req.ObservedAt = parsed
	}
	return req, nil
}

// sampleView is a stored observation as the API renders it.
//
// The source is included because it is the one thing that tells a reader whether
// the engine measured this number or was told it.
type sampleView struct {
	ID         int64     `json:"id"`
	MetricKey  string    `json:"metricKey"`
	Value      float64   `json:"value"`
	ObservedAt time.Time `json:"observedAt"`
	Source     string    `json:"source"`
}

func viewSample(record repository.SampleRecord) sampleView {
	return sampleView{
		ID:         record.ID,
		MetricKey:  record.MetricKey,
		Value:      record.Value,
		ObservedAt: record.ObservedAt,
		Source:     record.Source,
	}
}

// RecordSample accepts a value for a metric the engine cannot read itself.
//
// Both roles may post here. An external feed needs a credential of its own, and
// the alternative — requiring an operator key — would hand a cron job the power to
// release the kill switch and clear its own spend. What bounds the trust instead is
// that the operator decides which metrics are push at all, and that every value is
// audited under the credential that sent it.
func (h *Handler) RecordSample(c *gin.Context) {
	var body sampleRequest
	if !bindJSON(c, &body) {
		return
	}
	if body.Value == nil {
		badRequest(c, "value is required.")
		return
	}

	req, err := body.toService(c.Param("key"))
	if err != nil {
		badRequest(c, err.Error())
		return
	}

	record, err := h.samples.Record(c.Request.Context(), req, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusCreated, "Value recorded.", viewSample(record))
}

// LatestSample reads the most recent observation of a metric.
//
// It is how a feed confirms its value landed, and how an operator sees what a goal
// on this metric would currently be judged against.
func (h *Handler) LatestSample(c *gin.Context) {
	record, err := h.samples.Latest(c.Request.Context(), c.Param("key"))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Latest observation.", viewSample(record))
}
