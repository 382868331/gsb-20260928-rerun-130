// Package engine: build reports, explanations and dependency inspection.
//
// This file adds a reporting layer on top of the core engine: BuildReport
// runs a build that keeps going past failed targets, honours context
// cancellation, and records exactly which inputs and outputs (by content
// hash) participated in every action. Explain answers, at any time and
// across restarts, why an action's products are reusable or which input
// change forces a rebuild.
package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

// Action outcome statuses recorded in a BuildReport.
const (
	StatusRan       = "ran"       // executed during this build
	StatusReused    = "reused"    // fingerprint and outputs still valid
	StatusFailed    = "failed"    // executed but did not complete
	StatusSkipped   = "skipped"   // a prerequisite did not complete
	StatusCancelled = "cancelled" // not reached before cancellation
)

// ActionReport is the provenance record for one action: what the build
// actually used and produced, by content hash, or why it did not run.
//
// Started distinguishes "never began" (cancelled before scheduling, or
// skipped because a prerequisite did not complete) from "began but did
// not succeed" (failed, or its inputs drifted mid-build). For every
// started action the report keeps the fingerprint and input content
// hashes captured at the moment execution began, so a failed or drifted
// attempt still explains which input versions it read. DependsOn and
// InputFrom record where each input came from.
type ActionReport struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	Reason      string            `json:"reason,omitempty"`
	Started     bool              `json:"started,omitempty"`
	Tool        string            `json:"tool,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
	Fingerprint string            `json:"fingerprint,omitempty"`
	DependsOn   []string          `json:"depends_on,omitempty"` // producing action IDs
	InputFrom   map[string]string `json:"input_from,omitempty"` // input path -> producing action ID
	Inputs      map[string]string `json:"inputs,omitempty"`     // path -> content hash
	Outputs     map[string]string `json:"outputs,omitempty"`    // path -> content hash
}

// BuildReport is the outcome of one BuildReport call, in execution order.
type BuildReport struct {
	Targets   []string        `json:"targets"`
	StartedAt time.Time       `json:"started_at"`
	Actions   []*ActionReport `json:"actions"`
	Cancelled bool            `json:"cancelled,omitempty"`
	Err       error           `json:"-"` // first failure, if any
}

// OK reports whether every needed action completed or was reused.
func (r *BuildReport) OK() bool { return r.Err == nil && !r.Cancelled }

// Failed returns the IDs of actions that failed.
func (r *BuildReport) Failed() []string {
	var out []string
	for _, a := range r.Actions {
		if a.Status == StatusFailed {
			out = append(out, a.ID)
		}
	}
	return out
}

// Get returns the report for one action, or nil.
func (r *BuildReport) Get(id string) *ActionReport {
	for _, a := range r.Actions {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// Dependencies returns the IDs of the actions producing id's inputs, in
// sorted order. Shared prerequisites appear in every consumer's list but
// execute only once per build.
func (e *Engine) Dependencies(id string) ([]string, error) {
	a, ok := e.actions[id]
	if !ok {
		return nil, fmt.Errorf("unknown action %q", id)
	}
	return e.deps(a), nil
}

// BuildReport builds the targets like Build, but keeps going past failures
// as long as unaffected targets can still proceed, and stops scheduling new
// actions when ctx is cancelled. Actions that already completed stay
// completed (state is persisted per action), so a later build resumes where
// the cancelled one left off. The report records, for every action in the
// dependency closure, the exact input and output content hashes involved.
func (e *Engine) BuildReport(ctx context.Context, targets ...string) *BuildReport {
	rep := &BuildReport{Targets: append([]string(nil), targets...), StartedAt: time.Now()}
	e.Ran = nil
	need, err := e.closure(targets)
	if err != nil {
		rep.Err = err
		return rep
	}
	order, err := e.topoSort(need)
	if err != nil {
		rep.Err = err
		return rep
	}
	status := map[string]string{}
	for _, id := range order {
		a := e.actions[id]
		ar := &ActionReport{ID: id, Tool: a.Tool, Params: a.Params}
		ar.DependsOn = e.deps(a)
		for _, in := range a.Inputs {
			if pid, ok := e.producer[in]; ok && pid != a.ID {
				if ar.InputFrom == nil {
					ar.InputFrom = map[string]string{}
				}
				ar.InputFrom[in] = pid
			}
		}
		rep.Actions = append(rep.Actions, ar)

		if ctx.Err() != nil {
			rep.Cancelled = true
			ar.Status = StatusCancelled
			ar.Reason = "build cancelled before this action started"
			status[id] = ar.Status
			continue
		}
		// A failed, skipped or cancelled prerequisite blocks this action
		// but nothing else in the graph.
		blockedBy := ""
		for _, d := range e.deps(a) {
			if s := status[d]; s == StatusFailed || s == StatusSkipped || s == StatusCancelled {
				blockedBy = d
				break
			}
		}
		if blockedBy != "" {
			ar.Status = StatusSkipped
			ar.Reason = fmt.Sprintf("dependency %q did not complete", blockedBy)
			status[id] = ar.Status
			continue
		}

		ok, err := e.upToDate(a)
		if err != nil {
			ar.Status = StatusFailed
			ar.Reason = err.Error()
			status[id] = ar.Status
			if rep.Err == nil {
				rep.Err = err
			}
			continue
		}
		if ok {
			st := e.state.Actions[id]
			ar.Status = StatusReused
			ar.Reason = "inputs, parameters and outputs unchanged"
			ar.Fingerprint = st.Fingerprint
			ar.Inputs = st.Inputs
			ar.Outputs = st.Outputs
			status[id] = ar.Status
			continue
		}

		inputs, err := e.run(a)
		if err != nil {
			ar.Status = StatusFailed
			ar.Reason = err.Error()
			if inputs != nil {
				// The attempt began: record exactly which input
				// versions it read, captured at start, never
				// re-hashed from the workspace afterwards.
				ar.Started = true
				ar.Fingerprint = fingerprintFrom(a, inputs)
				ar.Inputs = inputs
			}
			status[id] = ar.Status
			if rep.Err == nil {
				rep.Err = err
			}
			continue
		}
		st := e.state.Actions[id]
		ar.Status = StatusRan
		ar.Started = true
		ar.Fingerprint = st.Fingerprint
		ar.Inputs = inputs
		ar.Outputs = st.Outputs
		status[id] = ar.Status
		e.Ran = append(e.Ran, id)
	}
	return rep
}

// Explanation describes why an action's current products are reusable, or
// exactly which inputs, parameters or outputs changed and force a rebuild.
// It works from the persisted state, so it is available after a restart.
type Explanation struct {
	ID       string   `json:"id"`
	Reusable bool     `json:"reusable"`
	Reasons  []string `json:"reasons,omitempty"`
}

// Explain compares the recorded state of action id with the workspace as it
// is now. Every reason lists the concrete difference found, so a user can
// tell which input change actually causes a rebuild.
func (e *Engine) Explain(id string) (*Explanation, error) {
	a, ok := e.actions[id]
	if !ok {
		return nil, fmt.Errorf("unknown action %q", id)
	}
	ex := &Explanation{ID: id}
	st, ok := e.state.Actions[id]
	if !ok {
		ex.Reasons = append(ex.Reasons, "no recorded state: never built")
		return ex, nil
	}
	if st.Tool != "" && st.Tool != a.Tool {
		ex.Reasons = append(ex.Reasons, fmt.Sprintf("tool changed: %q -> %q", st.Tool, a.Tool))
	}
	keys := map[string]bool{}
	for k := range st.Params {
		keys[k] = true
	}
	for k := range a.Params {
		keys[k] = true
	}
	var sortedKeys []string
	for k := range keys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	for _, k := range sortedKeys {
		old, hadOld := st.Params[k]
		cur, hasCur := a.Params[k]
		switch {
		case !hadOld:
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("parameter %q added: %q", k, cur))
		case !hasCur:
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("parameter %q removed (was %q)", k, old))
		case old != cur:
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("parameter %q changed: %q -> %q", k, old, cur))
		}
	}
	for _, in := range a.Inputs {
		h, err := hashFile(filepath.Join(e.root, in))
		recorded, had := st.Inputs[in]
		switch {
		case err != nil:
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("input %q is missing or unreadable", in))
		case !had:
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("input %q was not recorded in the previous build", in))
		case h != recorded:
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("input %q content changed", in))
		}
	}
	for _, out := range a.Outputs {
		recorded, ok := st.Outputs[out]
		if !ok {
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("output %q has no recorded hash", out))
			continue
		}
		h, err := hashFile(filepath.Join(e.root, out))
		switch {
		case err != nil:
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("output %q is missing on disk", out))
		case h != recorded:
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("output %q was modified on disk", out))
		}
	}
	ex.Reusable = len(ex.Reasons) == 0
	return ex, nil
}
