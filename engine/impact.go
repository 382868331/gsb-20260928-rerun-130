package engine

import "sort"

// Impact causes understood by the saved-view impact comparison.
const (
	ImpactDirectInput       = "direct-input"
	ImpactSharedDependency  = "shared-dependency"
	ImpactToolIdentity      = "tool-identity"
	ImpactParameter         = "parameter"
	ImpactMissingConclusion = "missing-conclusion"
)

// FactChange identifies one content-addressed fact present in a view.
type FactChange struct {
	Path  string `json:"path"`
	HashA string `json:"hash_a,omitempty"`
	HashB string `json:"hash_b,omitempty"`
	InA   bool   `json:"in_a"`
	InB   bool   `json:"in_b"`
}

// DependencyChange identifies a changed product consumed from another
// saved action.
type DependencyChange struct {
	Action string     `json:"action"`
	Input  string     `json:"input"`
	Change FactChange `json:"change"`
}

// IdentityChange records that an action was invoked as a different tool or
// with different parameters.
type IdentityChange struct {
	Action           string   `json:"action"`
	ToolA            string   `json:"tool_a,omitempty"`
	ToolB            string   `json:"tool_b,omitempty"`
	ToolChanged      bool     `json:"tool_changed"`
	ParameterChanges []string `json:"parameter_changes,omitempty"`
}

// InconclusiveAction records why a saved action has no successful result.
type InconclusiveAction struct {
	Action  string `json:"action"`
	Side    string `json:"side"`
	Status  string `json:"status"`
	Started bool   `json:"started"`
	Reason  string `json:"reason,omitempty"`
}

// ActionImpact is the attribution for one action present in either view.
type ActionImpact struct {
	ID                 string               `json:"id"`
	StatusA            string               `json:"status_a,omitempty"`
	StatusB            string               `json:"status_b,omitempty"`
	Reusable           bool                 `json:"reusable"`
	DirectInputs       []FactChange         `json:"direct_inputs,omitempty"`
	SharedDependencies []DependencyChange   `json:"shared_dependencies,omitempty"`
	Identity           *IdentityChange      `json:"identity,omitempty"`
	Inconclusive       []InconclusiveAction `json:"inconclusive,omitempty"`
}

// TargetImpact attributes differences within one requested build target's
// saved dependency closure.
type TargetImpact struct {
	Target             string               `json:"target"`
	StatusA            string               `json:"status_a,omitempty"`
	StatusB            string               `json:"status_b,omitempty"`
	ReusableActions    []string             `json:"reusable_actions,omitempty"`
	DirectInputs       []FactChange         `json:"direct_inputs,omitempty"`
	SharedDependencies []DependencyChange   `json:"shared_dependencies,omitempty"`
	ToolChanges        []IdentityChange     `json:"tool_changes,omitempty"`
	Inconclusive       []InconclusiveAction `json:"inconclusive,omitempty"`
	Actions            []*ActionImpact      `json:"actions"`
}

// ImpactComparison is a target-oriented comparison derived only from saved
// view records. It never reads or infers facts from current files.
type ImpactComparison struct {
	A       string          `json:"a"`
	B       string          `json:"b"`
	Targets []*TargetImpact `json:"targets"`
}

func successfulSavedAction(a *ActionReport) bool {
	return a != nil && (a.Status == StatusRan || a.Status == StatusReused)
}

func viewActionMap(v *View) map[string]*ActionReport {
	out := map[string]*ActionReport{}
	if v == nil {
		return out
	}
	for _, a := range v.Actions {
		out[a.ID] = a
	}
	return out
}

func savedClosure(target string, byID map[string]*ActionReport) map[string]*ActionReport {
	need := map[string]*ActionReport{}
	var visit func(id string)
	visit = func(id string) {
		if _, ok := need[id]; ok {
			return
		}
		a := byID[id]
		if a == nil {
			return
		}
		need[id] = a
		for _, dep := range a.DependsOn {
			visit(dep)
		}
	}
	visit(target)
	return need
}

func compareParams(a, b *ActionReport) []string {
	keys := map[string]bool{}
	for k := range a.Params {
		keys[k] = true
	}
	for k := range b.Params {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		if a.Params[k] != b.Params[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func impactAction(id string, a, b *ActionReport) *ActionImpact {
	ai := &ActionImpact{ID: id}
	if a != nil {
		ai.StatusA = a.Status
	}
	if b != nil {
		ai.StatusB = b.Status
	}
	if a == nil {
		ai.Inconclusive = append(ai.Inconclusive, InconclusiveAction{
			Action: id, Side: "a", Status: "missing", Reason: "action has no saved conclusion in view a",
		})
	}
	if b == nil {
		ai.Inconclusive = append(ai.Inconclusive, InconclusiveAction{
			Action: id, Side: "b", Status: "missing", Reason: "action has no saved conclusion in view b",
		})
	}
	if a != nil && !successfulSavedAction(a) {
		ai.Inconclusive = append(ai.Inconclusive, InconclusiveAction{
			Action: id, Side: "a", Status: a.Status, Started: a.Started, Reason: a.Reason,
		})
	}
	if b != nil && !successfulSavedAction(b) {
		ai.Inconclusive = append(ai.Inconclusive, InconclusiveAction{
			Action: id, Side: "b", Status: b.Status, Started: b.Started, Reason: b.Reason,
		})
	}
	if a == nil || b == nil {
		return ai
	}
	if a.Tool != b.Tool || len(compareParams(a, b)) > 0 {
		ai.Identity = &IdentityChange{
			Action:           id,
			ToolA:            a.Tool,
			ToolB:            b.Tool,
			ToolChanged:      a.Tool != b.Tool,
			ParameterChanges: compareParams(a, b),
		}
	}
	paths := map[string]bool{}
	for p := range a.Inputs {
		paths[p] = true
	}
	for p := range b.Inputs {
		paths[p] = true
	}
	for p := range paths {
		fc := FactChange{Path: p, InA: true, InB: true}
		if ha, ok := a.Inputs[p]; ok {
			fc.HashA = ha
		} else {
			fc.InA = false
		}
		if hb, ok := b.Inputs[p]; ok {
			fc.HashB = hb
		} else {
			fc.InB = false
		}
		if fc.HashA == fc.HashB && fc.InA == fc.InB {
			continue
		}
		producerA, generatedA := a.InputFrom[p]
		producerB, generatedB := b.InputFrom[p]
		if generatedA || generatedB {
			producer := producerA
			if producer == "" {
				producer = producerB
			}
			ai.SharedDependencies = append(ai.SharedDependencies, DependencyChange{
				Action: producer, Input: p, Change: fc,
			})
		} else {
			ai.DirectInputs = append(ai.DirectInputs, fc)
		}
	}
	ai.Reusable = successfulSavedAction(a) && successfulSavedAction(b) &&
		a.Fingerprint != "" && a.Fingerprint == b.Fingerprint &&
		equalStringMap(a.Outputs, b.Outputs)
	sort.Slice(ai.DirectInputs, func(i, j int) bool { return ai.DirectInputs[i].Path < ai.DirectInputs[j].Path })
	sort.Slice(ai.SharedDependencies, func(i, j int) bool {
		if ai.SharedDependencies[i].Action != ai.SharedDependencies[j].Action {
			return ai.SharedDependencies[i].Action < ai.SharedDependencies[j].Action
		}
		return ai.SharedDependencies[i].Input < ai.SharedDependencies[j].Input
	})
	return ai
}

func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// CompareViewImpacts compares two saved views by requested target and
// attributes action differences to direct inputs, shared dependencies, or
// tool/parameter identity. The function uses only the supplied records.
func CompareViewImpacts(a, b *View) *ImpactComparison {
	if a == nil || b == nil {
		return nil
	}
	actionsA := viewActionMap(a)
	actionsB := viewActionMap(b)
	targetSet := map[string]bool{}
	for _, t := range a.Targets {
		targetSet[t] = true
	}
	for _, t := range b.Targets {
		targetSet[t] = true
	}
	targets := mapKeysSorted(targetSet)
	out := &ImpactComparison{A: a.Name, B: b.Name}
	for _, target := range targets {
		closureA := savedClosure(target, actionsA)
		closureB := savedClosure(target, actionsB)
		idSet := map[string]bool{}
		for id := range closureA {
			idSet[id] = true
		}
		for id := range closureB {
			idSet[id] = true
		}
		ti := &TargetImpact{Target: target}
		if ta := closureA[target]; ta != nil {
			ti.StatusA = ta.Status
		}
		if tb := closureB[target]; tb != nil {
			ti.StatusB = tb.Status
		}
		seenDirect := map[string]bool{}
		seenDependency := map[string]bool{}
		for _, id := range mapKeysSorted(idSet) {
			ai := impactAction(id, closureA[id], closureB[id])
			ti.Actions = append(ti.Actions, ai)
			if ai.Reusable {
				ti.ReusableActions = append(ti.ReusableActions, id)
			}
			for _, fact := range ai.DirectInputs {
				key := fact.Path + "\x00" + fact.HashA + "\x00" + fact.HashB
				if !seenDirect[key] {
					seenDirect[key] = true
					ti.DirectInputs = append(ti.DirectInputs, fact)
				}
			}
			for _, dep := range ai.SharedDependencies {
				key := dep.Action + "\x00" + dep.Input + "\x00" + dep.Change.HashA + "\x00" + dep.Change.HashB
				if !seenDependency[key] {
					seenDependency[key] = true
					ti.SharedDependencies = append(ti.SharedDependencies, dep)
				}
			}
			if ai.Identity != nil {
				ti.ToolChanges = append(ti.ToolChanges, *ai.Identity)
			}
			ti.Inconclusive = append(ti.Inconclusive, ai.Inconclusive...)
		}
		sort.Slice(ti.DirectInputs, func(i, j int) bool { return ti.DirectInputs[i].Path < ti.DirectInputs[j].Path })
		sort.Slice(ti.SharedDependencies, func(i, j int) bool {
			if ti.SharedDependencies[i].Action != ti.SharedDependencies[j].Action {
				return ti.SharedDependencies[i].Action < ti.SharedDependencies[j].Action
			}
			return ti.SharedDependencies[i].Input < ti.SharedDependencies[j].Input
		})
		sort.Slice(ti.ToolChanges, func(i, j int) bool { return ti.ToolChanges[i].Action < ti.ToolChanges[j].Action })
		sort.Slice(ti.Inconclusive, func(i, j int) bool {
			if ti.Inconclusive[i].Action != ti.Inconclusive[j].Action {
				return ti.Inconclusive[i].Action < ti.Inconclusive[j].Action
			}
			return ti.Inconclusive[i].Side < ti.Inconclusive[j].Side
		})
		out.Targets = append(out.Targets, ti)
	}
	return out
}

func mapKeysSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Target returns the impact record for one selected target, if present.
func (c *ImpactComparison) Target(name string) *TargetImpact {
	for _, t := range c.Targets {
		if t.Target == name {
			return t
		}
	}
	return nil
}
