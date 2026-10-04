// view.go implements saved build-result views, provenance, mid-build
// dependency drift detection, per-action reuse explanations, and saved
// experiment candidates.
//
// A View is an immutable, named record of one build request: which targets
// were asked for, which actions actually ran, the exact inputs (content
// hashes) each action consumed, what it produced, and which parts failed,
// were skipped, canceled, or invalidated by dependencies changing mid-build.
// Once saved, a view is never overwritten: re-requesting the same name
// returns ErrViewExists, so later source changes can never silently replace
// what a saved view says.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// ErrViewExists is returned when a view name is requested that has already
// been saved. Views are immutable snapshots; choose a new name instead of
// replacing an old record.
var ErrViewExists = errors.New("view already exists")

// Action status values recorded in a View.
const (
	StatusRebuilt  = "rebuilt"  // executed in this build
	StatusReused   = "reused"   // cache hit: fingerprint and outputs intact
	StatusFailed   = "failed"   // tool or publish failed
	StatusSkipped  = "skipped"  // a prerequisite failed/skipped/canceled/drifted
	StatusCanceled = "canceled" // not reached before the build was canceled
	StatusDrifted  = "drifted"  // inputs changed while the action ran
)

// Provenance describes exactly what went into one action's products and
// what became of them, so a user can judge whether a product corresponds
// to the source and dependency state they asked for.
type Provenance struct {
	ActionID    string            `json:"action_id"`
	Tool        string            `json:"tool"`
	Params      map[string]string `json:"params,omitempty"`
	Inputs      map[string]string `json:"inputs"`
	InputFrom   map[string]string `json:"input_from"`
	Fingerprint string            `json:"fingerprint"`
	Outputs     map[string]string `json:"outputs,omitempty"`
	Status      string            `json:"status"`
	Reason      string            `json:"reason"`
	Error       string            `json:"error,omitempty"`
}

// View is a saved, immutable record of one build request.
type View struct {
	Name       string                 `json:"name"`
	Targets    []string               `json:"targets"`
	CreatedAt  time.Time              `json:"created_at"`
	Actions    map[string]*Provenance `json:"actions"`
	Failed     []string               `json:"failed,omitempty"`
	Incomplete []string               `json:"incomplete,omitempty"`
	Complete   bool                   `json:"complete"`
}

func viewsDir(root string) string { return filepath.Join(root, ".views") }

func validViewName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\") || strings.Contains(name, "\x00") {
		return fmt.Errorf("invalid view name %q", name)
	}
	return nil
}

// BuildView builds the targets like Build, but never stops at the first
// failure: independent actions keep going, dependents of failed actions are
// skipped, and everything is recorded in a named View saved under
// .views/<name>.json. The view records exactly which inputs participated
// and which parts did not complete. The build stops early if ctx is
// canceled; remaining actions are marked canceled and the partial view is
// still saved, so a later view can resume from the persisted state.
//
// The returned error is non-nil only for setup problems (bad name, view
// already exists, unknown target, cycle). Per-action failures are reported
// inside the View, not as the return error.
func (e *Engine) BuildView(ctx context.Context, name string, targets ...string) (*View, error) {
	if err := validViewName(name); err != nil {
		return nil, err
	}
	viewPath := filepath.Join(viewsDir(e.root), name+".json")
	if _, err := os.Stat(viewPath); err == nil {
		return nil, fmt.Errorf("%w: %q", ErrViewExists, name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	need, err := e.closure(targets)
	if err != nil {
		return nil, err
	}
	order, err := e.topoSort(need)
	if err != nil {
		return nil, err
	}

	v := &View{
		Name:      name,
		Targets:   slices.Clone(targets),
		CreatedAt: time.Now().UTC(),
		Actions:   map[string]*Provenance{},
	}
	e.Ran = nil

	blocked := func(a *Action) string {
		for _, d := range e.deps(a) {
			if !need[d] {
				continue
			}
			st := v.Actions[d].Status
			if st == StatusFailed || st == StatusSkipped || st == StatusCanceled || st == StatusDrifted {
				return d
			}
		}
		return ""
	}

	canceled := false
	for _, id := range order {
		a := e.actions[id]
		prov := &Provenance{
			ActionID:  id,
			Tool:      a.Tool,
			Params:    maps.Clone(a.Params),
			Inputs:    map[string]string{},
			InputFrom: map[string]string{},
		}
		for _, in := range a.Inputs {
			prov.InputFrom[in] = e.producer[in] // "" when it is a plain source file
		}
		v.Actions[id] = prov

		if canceled {
			prov.Status = StatusCanceled
			prov.Reason = "build canceled before this action ran"
			continue
		}
		if err := ctx.Err(); err != nil {
			canceled = true
			prov.Status = StatusCanceled
			prov.Reason = "build canceled before this action ran"
			continue
		}
		if dep := blocked(a); dep != "" {
			prov.Status = StatusSkipped
			prov.Reason = fmt.Sprintf("dependency %q did not complete (%s)", dep, v.Actions[dep].Status)
			continue
		}

		fpBefore, inputsBefore, fpErr := e.fingerprintDetails(a)
		if fpErr == nil {
			prov.Inputs = inputsBefore
			prov.Fingerprint = fpBefore
		}
		ok, upErr := e.upToDate(a)
		if upErr == nil && ok {
			prov.Status = StatusReused
			prov.Reason = "recorded fingerprint matches and all outputs are intact on disk"
			prov.Outputs = maps.Clone(e.state.Actions[id].Outputs)
			continue
		}

		if err := e.run(a); err != nil {
			prov.Status = StatusFailed
			prov.Reason = "action failed; no partial outputs published"
			prov.Error = err.Error()
			continue
		}
		e.Ran = append(e.Ran, id)

		// Dependency drift: re-hash inputs after the run. If they changed
		// while the tool was executing, the products were made from inputs
		// that no longer exist, so they do not belong to this view.
		fpAfter, inputsAfter, err2 := e.fingerprintDetails(a)
		if err2 != nil || fpAfter != fpBefore {
			prov.Status = StatusDrifted
			prov.Reason = "inputs changed while the action ran; products do not belong to this view and must be rebuilt"
			if err2 == nil {
				prov.Inputs = inputsAfter
			}
			// Drop the state run() just recorded: it keys the products to
			// inputs they were not built from. The next build must rerun.
			delete(e.state.Actions, id)
			_ = e.saveState()
			continue
		}
		prov.Status = StatusRebuilt
		prov.Reason = "executed in this build"
		prov.Outputs = maps.Clone(e.state.Actions[id].Outputs)
	}

	for _, id := range order {
		switch v.Actions[id].Status {
		case StatusFailed:
			v.Failed = append(v.Failed, id)
			v.Incomplete = append(v.Incomplete, id)
		case StatusSkipped, StatusCanceled, StatusDrifted:
			v.Incomplete = append(v.Incomplete, id)
		}
	}
	v.Complete = len(v.Incomplete) == 0

	if err := os.MkdirAll(viewsDir(e.root), 0o755); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	tmp := viewPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, viewPath); err != nil {
		return nil, err
	}
	return v, nil
}

// LoadView reads back a previously saved view. Saved views are never
// modified by later builds or source changes.
func (e *Engine) LoadView(name string) (*View, error) {
	if err := validViewName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(viewsDir(e.root), name+".json"))
	if err != nil {
		return nil, err
	}
	var v View
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("corrupt view %q: %w", name, err)
	}
	return &v, nil
}

// Explanation reports why an action's recorded products are reusable right
// now, or exactly which inputs, parameters, or outputs changed to prevent
// reuse. It answers "why is this artifact still valid?" after a restart.
type Explanation struct {
	ActionID string
	Reusable bool
	Reasons  []string
}

// Explain compares the action's recorded state against the current
// workspace and reports, in plain terms, what matches and what changed.
func (e *Engine) Explain(actionID string) (*Explanation, error) {
	a, ok := e.actions[actionID]
	if !ok {
		return nil, fmt.Errorf("unknown action %q", actionID)
	}
	ex := &Explanation{ActionID: actionID}
	st, ok := e.state.Actions[actionID]
	if !ok {
		ex.Reasons = append(ex.Reasons, "no recorded state: never built, failed, or invalidated")
		return ex, nil
	}
	if st.Tool != "" && st.Tool != a.Tool {
		ex.Reasons = append(ex.Reasons, fmt.Sprintf("tool changed: %q -> %q", st.Tool, a.Tool))
	}
	for k, val := range a.Params {
		if old, ok := st.Params[k]; !ok {
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("param %q added: %q", k, val))
		} else if old != val {
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("param %q changed: %q -> %q", k, old, val))
		}
	}
	for k, val := range st.Params {
		if _, ok := a.Params[k]; !ok {
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("param %q removed (was %q)", k, val))
		}
	}
	if st.Inputs != nil {
		for _, in := range a.Inputs {
			recorded, ok := st.Inputs[in]
			if !ok {
				ex.Reasons = append(ex.Reasons, fmt.Sprintf("input %q added", in))
				continue
			}
			actual, err := hashFile(filepath.Join(e.root, in))
			switch {
			case errors.Is(err, os.ErrNotExist):
				ex.Reasons = append(ex.Reasons, fmt.Sprintf("input %q is missing", in))
			case err != nil:
				return nil, err
			case actual != recorded:
				ex.Reasons = append(ex.Reasons, fmt.Sprintf("input %q content changed (hash %.12s -> %.12s)", in, recorded, actual))
			}
		}
	}
	for _, out := range a.Outputs {
		recorded, ok := st.Outputs[out]
		if !ok {
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("output %q has no recorded hash", out))
			continue
		}
		actual, err := hashFile(filepath.Join(e.root, out))
		switch {
		case errors.Is(err, os.ErrNotExist):
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("output %q is missing on disk", out))
		case err != nil:
			return nil, err
		case actual != recorded:
			ex.Reasons = append(ex.Reasons, fmt.Sprintf("output %q was modified on disk", out))
		}
	}
	if len(ex.Reasons) == 0 {
		ex.Reusable = true
		ex.Reasons = append(ex.Reasons, "recorded fingerprint matches and all outputs are intact")
	}
	return ex, nil
}

// ViewDiff reports, per action, which inputs or parameters actually changed
// between two saved views and which products differ as a consequence.
type ViewDiff struct {
	Actions []ActionDiff
}

// ActionDiff describes how one action differs between two views.
type ActionDiff struct {
	ActionID      string
	OnlyInA       bool
	OnlyInB       bool
	StatusA       string
	StatusB       string
	ChangedInputs []string // inputs whose content hash differs
	ChangedParams []string // params whose value differs
	OutputsDiffer bool     // produced different content
}

// DiffViews compares two saved views and reports which input changes truly
// caused the differences between their products.
func DiffViews(a, b *View) *ViewDiff {
	d := &ViewDiff{}
	ids := map[string]bool{}
	for id := range a.Actions {
		ids[id] = true
	}
	for id := range b.Actions {
		ids[id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	for _, id := range sorted {
		pa, pb := a.Actions[id], b.Actions[id]
		ad := ActionDiff{ActionID: id}
		switch {
		case pa == nil:
			ad.OnlyInB = true
			ad.StatusB = pb.Status
		case pb == nil:
			ad.OnlyInA = true
			ad.StatusA = pa.Status
		default:
			ad.StatusA, ad.StatusB = pa.Status, pb.Status
			for in, ha := range pa.Inputs {
				if hb, ok := pb.Inputs[in]; !ok || hb != ha {
					ad.ChangedInputs = append(ad.ChangedInputs, in)
				}
			}
			for in := range pb.Inputs {
				if _, ok := pa.Inputs[in]; !ok {
					ad.ChangedInputs = append(ad.ChangedInputs, in)
				}
			}
			sort.Strings(ad.ChangedInputs)
			for k, va := range pa.Params {
				if vb, ok := pb.Params[k]; !ok || vb != va {
					ad.ChangedParams = append(ad.ChangedParams, k)
				}
			}
			for k := range pb.Params {
				if _, ok := pa.Params[k]; !ok {
					ad.ChangedParams = append(ad.ChangedParams, k)
				}
			}
			sort.Strings(ad.ChangedParams)
			for out, ha := range pa.Outputs {
				if hb, ok := pb.Outputs[out]; !ok || hb != ha {
					ad.OutputsDiffer = true
					break
				}
			}
			if !ad.OutputsDiffer {
				for out := range pb.Outputs {
					if _, ok := pa.Outputs[out]; !ok {
						ad.OutputsDiffer = true
						break
					}
				}
			}
			if len(ad.ChangedInputs) == 0 && len(ad.ChangedParams) == 0 &&
				!ad.OutputsDiffer && ad.StatusA == ad.StatusB {
				continue // identical in both views
			}
		}
		d.Actions = append(d.Actions, ad)
	}
	return d
}
