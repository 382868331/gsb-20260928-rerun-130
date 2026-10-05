// Impact comparison between saved build views, and build-plan previews.
//
// CompareViewImpact explains, per action, why two saved views differ:
// whether the difference comes from a direct (source) input, from a
// shared dependency's product, or from the tool identity (tool name or
// parameters). It also tells which actions are still reusable as-is and
// which simply have no conclusion because they never started or stopped
// midway. The comparison is a pure function of the two saved views: it
// never reads the current workspace, so facts recorded for an old build
// are never backfilled from files that exist now.
//
// PreviewPlan turns a selection of targets (for example the differing
// ones from a comparison) into a build plan preview: the full closure in
// execution order, the shared dependencies several targets rely on, and
// the duplicate work a combined plan avoids. PlanDrift re-checks the
// workspace before the plan is confirmed and reports the affected scope
// when inputs changed since the preview.
package engine

import (
	"fmt"
	"path/filepath"
	"sort"
)

// Cause categories for a difference between two views.
const (
	CauseDirectInput      = "direct-input"      // a source input (not produced by any action) changed
	CauseSharedDependency = "shared-dependency" // an input produced by another action changed
	CauseToolIdentity     = "tool-identity"     // tool name or parameters changed
)

// ActionImpact explains how one action differs between two views, using
// only the facts each view recorded.
type ActionImpact struct {
	ID           string   `json:"id"`
	StatusA      string   `json:"status_a,omitempty"`
	StatusB      string   `json:"status_b,omitempty"`
	OnlyIn       string   `json:"only_in,omitempty"` // "a" or "b" when present in just one view
	Reusable     bool     `json:"reusable"`          // identical recorded facts; nothing to redo
	NoConclusion bool     `json:"no_conclusion"`     // never started or stopped midway in at least one view
	Causes       []string `json:"causes,omitempty"`
	DirectInputs []string `json:"direct_inputs,omitempty"` // changed source inputs
	SharedInputs []string `json:"shared_inputs,omitempty"` // changed inputs, "path (from action)"
	ToolA        string   `json:"tool_a,omitempty"`
	ToolB        string   `json:"tool_b,omitempty"`
	ParamChanges []string `json:"param_changes,omitempty"`
}

// ImpactReport is the result of an impact comparison between two views.
type ImpactReport struct {
	A       string          `json:"a"`
	B       string          `json:"b"`
	Actions []*ActionImpact `json:"actions"`
}

// concluded reports whether an action report holds a usable outcome:
// it succeeded (ran/reused, facts from state) or it began and its
// start-of-execution facts were captured (failed after starting).
func concluded(ar *ActionReport) bool {
	switch ar.Status {
	case StatusRan, StatusReused:
		return true
	case StatusFailed:
		return ar.Started
	}
	return false
}

// CompareViewImpact compares two saved views action by action and
// classifies every difference by cause. It reads only the two views;
// the current workspace is never consulted, so a view's record of an
// old build stands exactly as saved.
func CompareViewImpact(a, b *View) *ImpactReport {
	rep := &ImpactReport{A: a.Name, B: b.Name}
	byA := map[string]*ActionReport{}
	for _, ar := range a.Actions {
		byA[ar.ID] = ar
	}
	byB := map[string]*ActionReport{}
	for _, br := range b.Actions {
		byB[br.ID] = br
	}
	ids := map[string]bool{}
	for id := range byA {
		ids[id] = true
	}
	for id := range byB {
		ids[id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)

	for _, id := range sorted {
		ar, br := byA[id], byB[id]
		imp := &ActionImpact{ID: id}
		switch {
		case ar == nil:
			imp.OnlyIn = "b"
			imp.StatusB = br.Status
			imp.NoConclusion = !concluded(br)
		case br == nil:
			imp.OnlyIn = "a"
			imp.StatusA = ar.Status
			imp.NoConclusion = !concluded(ar)
		default:
			imp.StatusA, imp.StatusB = ar.Status, br.Status
			imp.ToolA, imp.ToolB = ar.Tool, br.Tool
			imp.NoConclusion = !concluded(ar) || !concluded(br)
			if !imp.NoConclusion {
				// Causes are classified only when both sides
				// recorded facts; an action that never started
				// has none, and inventing differences from its
				// missing record would misattribute a cause.
				classifyCauses(ar, br, imp)
			}
			imp.Reusable = !imp.NoConclusion && len(imp.Causes) == 0 &&
				outputsEqual(ar, br)
		}
		rep.Actions = append(rep.Actions, imp)
	}
	return rep
}

// classifyCauses fills imp with the cause categories and concrete
// differences between two action reports, using only recorded facts.
func classifyCauses(ar, br *ActionReport, imp *ActionImpact) {
	causes := map[string]bool{}
	if ar.Tool != br.Tool {
		causes[CauseToolIdentity] = true
	}
	keys := map[string]bool{}
	for k := range ar.Params {
		keys[k] = true
	}
	for k := range br.Params {
		keys[k] = true
	}
	var sortedKeys []string
	for k := range keys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	for _, k := range sortedKeys {
		old, hadOld := ar.Params[k]
		cur, hasCur := br.Params[k]
		switch {
		case !hadOld:
			imp.ParamChanges = append(imp.ParamChanges, fmt.Sprintf("%s added: %q", k, cur))
		case !hasCur:
			imp.ParamChanges = append(imp.ParamChanges, fmt.Sprintf("%s removed (was %q)", k, old))
		case old != cur:
			imp.ParamChanges = append(imp.ParamChanges, fmt.Sprintf("%s: %q -> %q", k, old, cur))
		}
	}
	if len(imp.ParamChanges) > 0 {
		causes[CauseToolIdentity] = true
	}

	// Producer of each input, from whichever view recorded it.
	producer := map[string]string{}
	for in, p := range ar.InputFrom {
		producer[in] = p
	}
	for in, p := range br.InputFrom {
		producer[in] = p
	}
	paths := map[string]bool{}
	for in := range ar.Inputs {
		paths[in] = true
	}
	for in := range br.Inputs {
		paths[in] = true
	}
	var sortedPaths []string
	for in := range paths {
		sortedPaths = append(sortedPaths, in)
	}
	sort.Strings(sortedPaths)
	for _, in := range sortedPaths {
		ha, okA := ar.Inputs[in]
		hb, okB := br.Inputs[in]
		if okA && okB && ha == hb {
			continue
		}
		if p, shared := producer[in]; shared {
			causes[CauseSharedDependency] = true
			imp.SharedInputs = append(imp.SharedInputs, fmt.Sprintf("%s (from %s)", in, p))
		} else {
			causes[CauseDirectInput] = true
			imp.DirectInputs = append(imp.DirectInputs, in)
		}
	}
	for _, c := range []string{CauseDirectInput, CauseSharedDependency, CauseToolIdentity} {
		if causes[c] {
			imp.Causes = append(imp.Causes, c)
		}
	}
}

func outputsEqual(ar, br *ActionReport) bool {
	if len(ar.Outputs) != len(br.Outputs) {
		return false
	}
	for out, ha := range ar.Outputs {
		if hb, ok := br.Outputs[out]; !ok || hb != ha {
			return false
		}
	}
	return true
}

// Get returns the impact for one action, or nil.
func (r *ImpactReport) Get(id string) *ActionImpact {
	for _, a := range r.Actions {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// ForTarget restricts the report to the dependency closure of one
// target, derived from the DependsOn facts saved in the given views. It
// lets a user inspect the impact per selected target without consulting
// the current workspace.
func (r *ImpactReport) ForTarget(target string, views ...*View) *ImpactReport {
	need := map[string]bool{target: true}
	changed := true
	for changed {
		changed = false
		for _, v := range views {
			for _, ar := range v.Actions {
				if !need[ar.ID] {
					continue
				}
				for _, d := range ar.DependsOn {
					if !need[d] {
						need[d] = true
						changed = true
					}
				}
			}
		}
	}
	sub := &ImpactReport{A: r.A, B: r.B}
	for _, a := range r.Actions {
		if need[a.ID] {
			sub.Actions = append(sub.Actions, a)
		}
	}
	return sub
}

// PlanPreview describes a prospective build for a selection of targets
// before it is confirmed: the full execution order, the dependencies
// shared by several targets (work a combined plan does once instead of
// per target), and a snapshot of external input hashes used by PlanDrift
// to update the affected scope when inputs change before confirmation.
type PlanPreview struct {
	Targets  []string            `json:"targets"`
	Order    []string            `json:"order"`     // full closure, dependencies first
	Shared   []string            `json:"shared"`    // actions needed by more than one target
	NeededBy map[string][]string `json:"needed_by"` // action -> selecting targets

	inputs map[string]map[string]string // action -> external input -> hash at preview
}

// PreviewPlan computes the plan that building the selected targets would
// follow, without running anything. Shared prerequisites appear once in
// Order and are listed in Shared with every target that needs them, so
// the duplicate work a combined plan avoids is visible up front.
func (e *Engine) PreviewPlan(targets ...string) (*PlanPreview, error) {
	need, err := e.closure(targets)
	if err != nil {
		return nil, err
	}
	order, err := e.topoSort(need)
	if err != nil {
		return nil, err
	}
	p := &PlanPreview{
		Targets:  append([]string(nil), targets...),
		Order:    order,
		NeededBy: map[string][]string{},
		inputs:   map[string]map[string]string{},
	}
	for _, t := range targets {
		tn, err := e.closure([]string{t})
		if err != nil {
			return nil, err
		}
		for id := range tn {
			p.NeededBy[id] = append(p.NeededBy[id], t)
		}
	}
	for id, ts := range p.NeededBy {
		sort.Strings(ts)
		if len(ts) > 1 {
			p.Shared = append(p.Shared, id)
		}
	}
	sort.Strings(p.Shared)
	// Snapshot external inputs (those not produced inside the plan) so
	// PlanDrift can later tell exactly which planned actions a change
	// affects. Products of planned actions are excluded: the plan itself
	// is expected to regenerate them.
	for _, id := range order {
		a := e.actions[id]
		m := map[string]string{}
		for _, in := range a.Inputs {
			if pid, ok := e.producer[in]; ok && need[pid] {
				continue
			}
			if h, err := hashFile(filepath.Join(e.root, in)); err == nil {
				m[in] = h
			}
		}
		p.inputs[id] = m
	}
	return p, nil
}

// PlanDrift re-hashes the external inputs captured by PreviewPlan and
// returns the affected scope in execution order: every planned action
// whose inputs changed since the preview, plus everything downstream of
// it in the plan. An empty result means the plan is still accurate.
func (e *Engine) PlanDrift(p *PlanPreview) ([]string, error) {
	changed := map[string]bool{}
	for _, id := range p.Order {
		for in, old := range p.inputs[id] {
			h, err := hashFile(filepath.Join(e.root, in))
			if err != nil || h != old {
				changed[id] = true
				break
			}
		}
	}
	affected := map[string]bool{}
	for _, id := range p.Order {
		if changed[id] {
			affected[id] = true
			continue
		}
		for _, d := range e.deps(e.actions[id]) {
			if affected[d] {
				affected[id] = true
				break
			}
		}
	}
	var out []string
	for _, id := range p.Order {
		if affected[id] {
			out = append(out, id)
		}
	}
	return out, nil
}
