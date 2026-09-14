package goalengine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenSample is one observation of what core spent.
//
// Tokens is the value; how those observations become a number a goal is judged
// against — a sum over the day, a latest reading — is the operator's business and
// lives in the engine's config/metrics.yaml. Core reports, it does not aggregate.
type TokenSample struct {
	Tokens int64
	// ObservedAt is when the tokens were spent. Zero means "now", which is what the
	// engine records when the field is absent; sending a zero timestamp instead
	// would be a caller believing they backdated something.
	ObservedAt time.Time
	// Note is what a person reading the sample list needs to know — which run, which
	// model. Never a credential: the engine stores it and renders it back.
	Note string
}

// sampleBody is the engine's POST /v1/metrics/<key>/samples request.
//
// Value is a pointer for the engine's own reason: zero is a real reading, so an
// absent field must not arrive as one.
type sampleBody struct {
	Value      *float64 `json:"value"`
	ObservedAt *string  `json:"observedAt,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// ReportTokens files a run's token usage as a push sample.
//
// This is what makes cost a metric a goal can be written against: an operator can set
// a target on ops.tokens_spent and have the engine notice when the assistant starts
// costing more than it saves. It is reporting, not permission — nothing about a run
// depends on the sample landing, which is why a failure here is logged by the caller
// rather than allowed to stop work.
func (c *Client) ReportTokens(ctx context.Context, sample TokenSample) error {
	if c.metric == "" {
		return errors.New("goalengine: no spend metric is configured; nothing to report tokens to")
	}
	// Zero is allowed and is not pointless: a run that answered from cache spent
	// nothing, and a sample saying so keeps the feed fresh. The engine stops
	// evaluating a metric whose latest sample is older than METRIC_MAX_SAMPLE_AGE
	// rather than reading a dead feed's last number as on-track.
	if sample.Tokens < 0 {
		return fmt.Errorf("goalengine: token spend cannot be negative, got %d", sample.Tokens)
	}

	value := float64(sample.Tokens)
	body := sampleBody{Value: &value, Note: strings.TrimSpace(sample.Note)}
	if !sample.ObservedAt.IsZero() {
		observed := sample.ObservedAt.UTC().Format(time.RFC3339)
		body.ObservedAt = &observed
	}

	// Escaped because the key comes from configuration rather than from this package.
	// A key with a slash in it would otherwise post to a path nobody declared.
	path := "/v1/metrics/" + url.PathEscape(c.metric) + "/samples"
	if err := c.do(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("goalengine: report token spend: %w", err)
	}
	return nil
}
