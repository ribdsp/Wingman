package tool

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Registry assembles the tools a run may be offered.
//
// It holds the operator's grants and every runner. It holds no state about any
// particular run: the per-run value is the Offering that Offer returns.
type Registry struct {
	grants  *Grants
	runners []Runner
}

// NewRegistry wires the grants to the runners that can execute them.
func NewRegistry(grants *Grants, runners ...Runner) *Registry {
	if grants == nil {
		// An empty set rather than a nil map, so a caller that forgot to load the
		// file gets a run where nothing is callable instead of a nil dereference
		// somewhere inside the loop.
		grants = NewGrants()
	}
	return &Registry{grants: grants, runners: runners}
}

// Grants exposes the declarations, so a caller doing the per-call check does not
// need a second reference to them.
func (r *Registry) Grants() *Grants { return r.grants }

// Offer lists what this run may use, once.
//
// Once per run, not once per call, and the result is immutable — that is the point.
// Tools arrive from servers that can change their list at any moment, so a run that
// re-listed before every call could be handed a tool halfway through that it was
// never planned with. The snapshot means the set a run is judged against is the set
// it was given.
//
// A source that cannot be listed does not stop the run. It is recorded in
// Unavailable and the run proceeds with fewer tools, because the failure mode of
// proceeding is an agent that cannot do something and says so, while the failure
// mode of refusing is one unreachable MCP server halting every run on the instance.
// Nothing is silent: Unavailable and Conflicting are named in the run's system
// prompt, so the model knows a tool it might expect is absent, and logged, so the
// operator knows which source to fix.
func (r *Registry) Offer(ctx context.Context) (*Offering, error) {
	offering := &Offering{routes: map[string]Runner{}}
	claimed := map[string][]string{}
	definitions := map[string]Definition{}

	for _, runner := range r.runners {
		listed, err := runner.Tools(ctx)
		if err != nil {
			offering.unavailable = append(offering.unavailable, RunnerFailure{
				Label: runner.Label(),
				Err:   err,
			})
			continue
		}

		for _, definition := range listed {
			name := strings.TrimSpace(definition.Name)
			grant, granted := r.grants.Get(name)
			if !granted || !grant.Enabled {
				// Dropped without comment, and this is the load-bearing line of the
				// package. A tool the operator never wrote down — or wrote down and
				// switched off — is not offered, whatever a server says it can do.
				// domain.ClassifyTool would deny the call anyway; not showing it
				// means the model never plans around a capability it cannot use.
				continue
			}
			definition.Name = name
			claimed[name] = append(claimed[name], runner.Label())
			definitions[name] = definition
			offering.routes[name] = runner
		}
	}

	for name, labels := range claimed {
		if len(labels) < 2 {
			continue
		}
		// Two sources claiming one granted name is withdrawn rather than resolved by
		// order. The grant says what class the tool has and therefore whether it
		// needs a human; it does not say whose implementation runs. Picking the
		// first would let a source added later quietly take over a name the operator
		// approved for another one.
		sort.Strings(labels)
		delete(definitions, name)
		delete(offering.routes, name)
		offering.conflicting = append(offering.conflicting, Conflict{Name: name, Labels: labels})
	}

	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	// Sorted, so the tool list a model sees does not reorder between runs. Map order
	// would make one prompt several prompts as far as caching is concerned, and it
	// would make two identical runs produce different transcripts.
	for _, name := range names {
		offering.definitions = append(offering.definitions, definitions[name])
	}
	sort.Slice(offering.conflicting, func(i, j int) bool {
		return offering.conflicting[i].Name < offering.conflicting[j].Name
	})

	return offering, nil
}

// RunnerFailure is a source that could not be listed.
type RunnerFailure struct {
	Label string
	Err   error
}

// Conflict is a granted name more than one source claimed. Both are withdrawn; see
// Registry.Offer.
type Conflict struct {
	Name   string
	Labels []string
}

// Offering is the immutable set of tools one run may use, and how to reach them.
type Offering struct {
	definitions []Definition
	routes      map[string]Runner
	unavailable []RunnerFailure
	conflicting []Conflict
}

// Definitions are the tools to put in front of the model, sorted by name.
func (o *Offering) Definitions() []Definition {
	// A copy: the caller passes these to a provider adapter, and an adapter that
	// edited a schema in place would change what every later run is offered.
	out := make([]Definition, len(o.definitions))
	copy(out, o.definitions)
	return out
}

// Names are the offered tool names, sorted.
func (o *Offering) Names() []string {
	names := make([]string, 0, len(o.definitions))
	for _, definition := range o.definitions {
		names = append(names, definition.Name)
	}
	return names
}

// Unavailable lists the sources that could not be listed when this run started.
func (o *Offering) Unavailable() []RunnerFailure {
	out := make([]RunnerFailure, len(o.unavailable))
	copy(out, o.unavailable)
	return out
}

// Conflicting lists the granted names withdrawn because two sources claimed them.
func (o *Offering) Conflicting() []Conflict {
	out := make([]Conflict, len(o.conflicting))
	copy(out, o.conflicting)
	return out
}

// Has reports whether this run was offered a tool.
func (o *Offering) Has(name string) bool {
	_, ok := o.routes[strings.TrimSpace(name)]
	return ok
}

// Call runs one invocation and bounds what comes back.
//
// The truncation happens here rather than in each runner so there is one place that
// decides how much of a tool's output a run is allowed to pay for, and so a runner
// added later cannot forget to do it.
func (o *Offering) Call(ctx context.Context, invocation Invocation) (Result, error) {
	name := strings.TrimSpace(invocation.Name)
	runner, ok := o.routes[name]
	if !ok {
		// Not a model mistake — the loop checks the grant before it gets here, so
		// reaching this means the loop called something this run was never offered.
		return Result{}, fmt.Errorf("tool %q was not offered to this run", invocation.Name)
	}

	invocation.Name = name
	result, err := runner.Call(ctx, invocation)
	if err != nil {
		return Result{}, fmt.Errorf("tool %s (%s): %w", name, runner.Label(), err)
	}

	result.Content, result.Truncated = truncate(result.Content, MaxOutputBytes)
	return result, nil
}
