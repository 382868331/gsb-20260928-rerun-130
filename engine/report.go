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
	"strings"
	"time"
)

// Action outcome statuses recorded in a BuildReport.
const (
	StatusRan       = "ran"       // executed during this build
	StatusReused    = "reused"    // fingerprint and outputs still valid
	StatusFailed    = "failed"    // executed but did not complete
	StatusSkipped   = "skipped"   // a prerequisite did not complete
	StatusDrifted   = "drifted"   // started, but an input changed while running
	StatusCancelled = "cancelled" // not reached before cancellation started
)

// ActionReport is the provenance record for one action: what the build
// actually used and produced, by content hash, or why it did not run. A
// record for an action that never started (cancelled before its turn, or
// skipped after a prerequisite failure) carries no input fingerprint:
// no input version was ever chosen. A record for an action that started
// but did not succeed (failed or drifted) always carries the exact input
// hashes read at execution start, the fingerprint derived from them, the
// tool identity and the dependency sources, so the attempt stays
// attributable to one concrete input version even after that input later
// changes on disk.
type ActionReport struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	Started     bool              `json:"started"`
	Reason      string            `json:"reason,omitempty"`
	Tool        string            `json:"tool,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
	Fingerprint string            `json:"fingerprint,omitempty"`
	Inputs      map[string]string `json:"inputs,omitempty"`  // path -> content hash at start
	Outputs     map[string]string `json:"outputs,omitempty"` // path -> content hash
	DependsOn   []string          `json:"depends_on,omitempty"`
}

// Explain renders this report entry into a human-readable explanation of
// the attempt it records. It uses only the report itself, never the
// current workspace: a report loaded from a saved view long after the
// build explains exactly the build that was saved, even though the files
// on disk have since changed.
func (a *ActionReport) Explain() []string {
	var lines []string
	switch a.Status {
	case StatusRan:
		lines = append(lines, fmt.Sprintf("action %q ran with tool %q", a.ID, a.Tool))
	case StatusReused:
		lines = append(lines, fmt.Sprintf("action %q reused existing products built by tool %q", a.ID, a.Tool))
	case StatusFailed:
		if a.Started {
			lines = append(lines, fmt.Sprintf("action %q started with tool %q but failed: %s", a.ID, a.Tool, a.Reason))
		} else {
			lines = append(lines, fmt.Sprintf("action %q failed before the tool %q started: %s", a.ID, a.Tool, a.Reason))
		}
	case StatusDrifted:
		lines = append(lines, fmt.Sprintf("action %q started with tool %q but its inputs changed during execution; products were discarded: %s", a.ID, a.Tool, a.Reason))
	case StatusSkipped:
		lines = append(lines, fmt.Sprintf("action %q never started: %s", a.ID, a.Reason))
	case StatusCancelled:
		lines = append(lines, fmt.Sprintf("action %q never started: %s", a.ID, a.Reason))
	default:
		if a.Reason != "" {
			lines = append(lines, fmt.Sprintf("action %q: %s: %s", a.ID, a.Status, a.Reason))
		} else {
			lines = append(lines, fmt.Sprintf("action %q: %s", a.ID, a.Status))
		}
	}
	if len(a.DependsOn) > 0 {
		lines = append(lines, "dependency sources: "+strings.Join(a.DependsOn, ", "))
	}
	if a.Fingerprint != "" {
		lines = append(lines, "input fingerprint at execution start: "+a.Fingerprint)
	}
	keys := make([]string, 0, len(a.Inputs))
	for in := range a.Inputs {
		keys = append(keys, in)
	}
	sort.Strings(keys)
	for _, in := range keys {
		lines = append(lines, fmt.Sprintf("input %q had content hash %s when the action started", in, a.Inputs[in]))
	}
	keys = keys[:0]
	for out := range a.Outputs {
		keys = append(keys, out)
	}
	sort.Strings(keys)
	for _, out := range keys {
		lines = append(lines, fmt.Sprintf("product %q recorded with content hash %s", out, a.Outputs[out]))
	}
	return lines
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

func cloneParams(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneHashes(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// snapshotInputs reads each declared input's current content hash. Files
// that are missing or unreadable are simply omitted: the result is used
// only to document what a failed-before-start attempt could see, never to
// reconstruct a fingerprint.
func (e *Engine) snapshotInputs(a *Action) map[string]string {
	hashes := map[string]string{}
	for _, in := range a.Inputs {
		if h, err := hashFile(filepath.Join(e.root, in)); err == nil {
			hashes[in] = h
		}
	}
	return hashes
}

// isInputDriftErr reports whether a run failure means an input changed
// while the tool was executing.
func isInputDriftErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "changed during build")
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
		ar := &ActionReport{
			ID:        id,
			Tool:      a.Tool,
			Params:    cloneParams(a.Params),
			DependsOn: e.deps(a),
		}
		rep.Actions = append(rep.Actions, ar)

		if ctx.Err() != nil {
			rep.Cancelled = true
			ar.Status = StatusCancelled
			ar.Reason = "build cancelled before this action started; no input version was chosen"
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
			ar.Started = false
			ar.Reason = err.Error()
			// Pre-execution failure: the input version could not be read,
			// so no fingerprint can honestly be recorded. Snapshot whatever
			// is currently readable instead of leaving the attempt blank.
			ar.Inputs = e.snapshotInputs(a)
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
			ar.Started = true
			ar.Fingerprint = st.Fingerprint
			ar.Inputs = cloneHashes(st.Inputs)
			ar.Outputs = st.Outputs
			status[id] = ar.Status
			continue
		}

		rec, inputs, err := e.run(a)
		if err != nil {
			if rec != nil && rec.Started {
				// The tool actually ran. Attribute the unsuccessful attempt
				// to the input version read at execution start, never to
				// whatever the files contain now.
				ar.Fingerprint = rec.Fingerprint
				ar.Inputs = cloneHashes(rec.Inputs)
				if isInputDriftErr(err) {
					ar.Status = StatusDrifted
				} else {
					ar.Status = StatusFailed
				}
			} else {
				// Staging/setup/preflight failure: the tool never ran.
				ar.Status = StatusFailed
				if rec != nil {
					ar.Fingerprint = rec.Fingerprint
					ar.Inputs = cloneHashes(rec.Inputs)
				}
			}
			ar.Started = rec != nil && rec.Started
			ar.Reason = err.Error()
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
		ar.Inputs = cloneHashes(inputs)
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
