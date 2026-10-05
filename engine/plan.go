package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// PlanInput is one input captured when a plan preview was made or last
// refreshed.
type PlanInput struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// BuildPlan is an execution preview for targets selected from an impact
// comparison. The preview does not run actions.
type BuildPlan struct {
	Targets               []string    `json:"targets"`
	Actions               []string    `json:"actions"`
	SharedDependencies    []string    `json:"shared_dependencies"`
	ReusableActions       []string    `json:"reusable_actions"`
	ActionsToRun          []string    `json:"actions_to_run"`
	PossibleDuplicateWork []string    `json:"possible_duplicate_work"`
	Inputs                []PlanInput `json:"inputs"`
}

// PlanRefresh reports inputs that changed after a preview and contains the
// updated preview. The previous plan is left intact for comparison.
type PlanRefresh struct {
	ChangedInputs []string   `json:"changed_inputs"`
	NewlyReusable []string   `json:"newly_reusable,omitempty"`
	NewlyRequired []string   `json:"newly_required,omitempty"`
	UpdatedPlan   *BuildPlan `json:"updated_plan"`
}

// NewBuildPlan creates a preview for selected impact targets in the current
// engine. Current files are read only to determine what is reusable now;
// saved views are neither modified nor backfilled.
func (e *Engine) NewBuildPlan(impact *ImpactComparison, targets ...string) (*BuildPlan, error) {
	if impact == nil {
		return nil, errors.New("cannot plan from nil impact comparison")
	}
	if len(targets) == 0 {
		return nil, errors.New("select at least one target")
	}
	known := map[string]bool{}
	for _, t := range impact.Targets {
		known[t.Target] = true
	}
	for _, target := range targets {
		if !known[target] {
			return nil, fmt.Errorf("target %q is not part of the impact comparison", target)
		}
	}
	return e.planCurrent(targets)
}

func (e *Engine) planCurrent(targets []string) (*BuildPlan, error) {
	need, err := e.closure(targets)
	if err != nil {
		return nil, err
	}
	order, err := e.topoSort(need)
	if err != nil {
		return nil, err
	}
	plan := &BuildPlan{Targets: append([]string(nil), targets...), Actions: order}
	consumers := map[string]map[string]bool{}
	for _, target := range targets {
		closure := savedActionClosure(e, target)
		for id := range closure {
			if consumers[id] == nil {
				consumers[id] = map[string]bool{}
			}
			consumers[id][target] = true
		}
	}
	inputSet := map[string]bool{}
	willRun := map[string]bool{}
	for _, id := range order {
		a := e.actions[id]
		for _, in := range a.Inputs {
			inputSet[in] = true
		}
		ok, err := e.upToDate(a)
		if err != nil {
			return nil, err
		}
		if ok {
			continue
		}
		willRun[id] = true
	}
	// A prerequisite selected to run will publish new products. Consumers of
	// those products are therefore affected in this preview even if their old
	// product still happens to be present and hashable before confirmation.
	for _, id := range order {
		if willRun[id] {
			continue
		}
		a := e.actions[id]
		for _, dep := range e.deps(a) {
			if need[dep] && willRun[dep] {
				willRun[id] = true
				break
			}
		}
	}
	for _, id := range order {
		if willRun[id] {
			plan.ActionsToRun = append(plan.ActionsToRun, id)
			if len(consumers[id]) > 1 {
				plan.PossibleDuplicateWork = append(plan.PossibleDuplicateWork, id)
			}
		} else {
			plan.ReusableActions = append(plan.ReusableActions, id)
		}
		if len(consumers[id]) > 1 {
			plan.SharedDependencies = append(plan.SharedDependencies, id)
		}
	}
	for in := range inputSet {
		hash := ""
		if h, err := hashFile(filepath.Join(e.root, in)); err == nil {
			hash = h
		}
		plan.Inputs = append(plan.Inputs, PlanInput{Path: in, Hash: hash})
	}
	sort.Slice(plan.Inputs, func(i, j int) bool { return plan.Inputs[i].Path < plan.Inputs[j].Path })
	return plan, nil
}

func savedActionClosure(e *Engine, target string) map[string]bool {
	need, _ := e.closure([]string{target})
	return need
}

// RefreshPlan re-evaluates a preview against current files. It returns a
// new plan; the supplied plan remains unchanged so callers can show what
// changed before confirmation.
func (e *Engine) RefreshPlan(plan *BuildPlan) (*PlanRefresh, error) {
	if plan == nil {
		return nil, errors.New("cannot refresh nil plan")
	}
	updated, err := e.planCurrent(plan.Targets)
	if err != nil {
		return nil, err
	}
	old := map[string]PlanInput{}
	for _, in := range plan.Inputs {
		old[in.Path] = in
	}
	changedSet := map[string]bool{}
	for _, in := range updated.Inputs {
		if prev, ok := old[in.Path]; !ok || prev.Hash != in.Hash {
			changedSet[in.Path] = true
		}
	}
	for _, in := range plan.Inputs {
		if _, err := os.Stat(filepath.Join(e.root, in.Path)); errors.Is(err, os.ErrNotExist) {
			changedSet[in.Path] = true
		}
	}
	oldReusable := map[string]bool{}
	for _, id := range plan.ReusableActions {
		oldReusable[id] = true
	}
	oldRun := map[string]bool{}
	for _, id := range plan.ActionsToRun {
		oldRun[id] = true
	}
	newReusable := map[string]bool{}
	for _, id := range updated.ReusableActions {
		newReusable[id] = true
	}
	refresh := &PlanRefresh{UpdatedPlan: updated}
	for _, id := range updated.Actions {
		switch {
		case newReusable[id] && !oldReusable[id]:
			refresh.NewlyReusable = append(refresh.NewlyReusable, id)
		case !newReusable[id] && (oldReusable[id] || !oldRun[id]):
			refresh.NewlyRequired = append(refresh.NewlyRequired, id)
		}
	}
	refresh.ChangedInputs = mapKeysSorted(changedSet)
	return refresh, nil
}
