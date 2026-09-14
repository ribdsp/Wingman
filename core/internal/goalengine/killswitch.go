package goalengine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Engaged reports whether the goal engine's kill switch is on.
//
// The signature is the agent loop's Halt port, so *Client satisfies it with no
// adapter. Its shape is the safety property: an error is not a false. The loop reads
// an unreadable switch as engaged and halts, which is the rule the goal engine
// already applies to its own flag — see its FlagRepository, which refuses to assume
// a missing row means off.
//
// enabled: true means engaged, which reads backwards until you remember the flag is
// named kill_switch. It is the engine's own field name and is not translated here,
// because a translation is a place for a negation to be lost.
func (c *Client) Engaged(ctx context.Context) (bool, error) {
	var envelope struct {
		Data struct {
			Key     string `json:"key"`
			Enabled bool   `json:"enabled"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, KillSwitchPath, nil, &envelope); err != nil {
		return false, fmt.Errorf("goalengine: read the kill switch: %w", err)
	}

	// A 200 carrying no flag is not a released switch. A proxy, a login page or a
	// different service on that address can all answer 200 with JSON that decodes
	// into an empty struct, and every one of those would otherwise read as "nothing
	// is halted" — the one answer this call must never invent.
	if strings.TrimSpace(envelope.Data.Key) == "" {
		return false, errors.New("goalengine: the kill switch response carried no flag; refusing to read that as released")
	}
	return envelope.Data.Enabled, nil
}

// Absent is the kill switch of an instance that has no goal engine configured.
//
// It answers "not engaged" — the one place in this package that answers permissively,
// and it does so because the question was never asked. internal/config draws the
// distinction it relies on: an empty GOAL_ENGINE_BASE_URL is an operator choosing to
// run core standalone, while a base URL that is set and unanswerable is a fault. Only
// the second is fail-closed, and cmd logs a startup warning for the first so the
// choice is visible rather than inferred.
//
// There is deliberately no Absent spending gate to go with it. A standalone instance
// has no gate at all, and the loop refuses a spending call when its gate is nil —
// "no goal engine" means nothing may spend money, not that spending is unguarded.
type Absent struct{}

// Engaged always reports that nothing is halted.
func (Absent) Engaged(context.Context) (bool, error) { return false, nil }
