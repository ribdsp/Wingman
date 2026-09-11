package metrics

import (
	"context"
	"fmt"
)

// MuxSampler routes each metric to the sampler for its source type.
type MuxSampler struct {
	sql  Sampler
	http Sampler
}

// NewMuxSampler wires the per-source samplers. Either may be nil if no metric of
// that source is configured; the error surfaces only when such a metric is read.
func NewMuxSampler(sqlSampler, httpSampler Sampler) *MuxSampler {
	return &MuxSampler{sql: sqlSampler, http: httpSampler}
}

// Sample dispatches on the definition's source.
func (m *MuxSampler) Sample(ctx context.Context, def Definition) (Sample, error) {
	switch def.Source {
	case SourceSQL:
		if m.sql == nil {
			return Sample{}, fmt.Errorf("metric %s: no sql sampler configured", def.Key)
		}
		return m.sql.Sample(ctx, def)
	case SourceHTTP:
		if m.http == nil {
			return Sample{}, fmt.Errorf("metric %s: no http sampler configured", def.Key)
		}
		return m.http.Sample(ctx, def)
	case SourcePush:
		return Sample{}, fmt.Errorf("metric %s: %w", def.Key, ErrNotPullable)
	default:
		return Sample{}, fmt.Errorf("metric %s: unknown source %q", def.Key, def.Source)
	}
}
