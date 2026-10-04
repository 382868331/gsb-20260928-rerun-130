// Saved build views: immutable provenance snapshots of build results.
//
// A view records what a build actually consumed and produced (per action,
// by content hash), including the parts that failed. Views are persisted
// under .views/ and are immutable: asking for a view again returns the
// saved record, never a silently refreshed one, so later source changes
// cannot replace what a user was shown. VerifyView reports drift between
// a saved view and the current workspace without touching the view.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// View is a saved, immutable record of one build result.
type View struct {
	Name      string          `json:"name"`
	CreatedAt time.Time       `json:"created_at"`
	Targets   []string        `json:"targets"`
	Actions   []*ActionReport `json:"actions"`
}

func (e *Engine) viewsDir() string { return filepath.Join(e.root, ".views") }

func validViewName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid view name %q", name)
	}
	return nil
}

// SaveView stores report as a named view. The name must be unused: a saved
// view is never overwritten, so re-requesting it later always yields the
// original record regardless of intervening source changes.
func (e *Engine) SaveView(name string, rep *BuildReport) (*View, error) {
	if err := validViewName(name); err != nil {
		return nil, err
	}
	if rep == nil {
		return nil, errors.New("cannot save a nil build report")
	}
	v := &View{
		Name:      name,
		CreatedAt: rep.StartedAt,
		Targets:   rep.Targets,
		Actions:   rep.Actions,
	}
	if err := os.MkdirAll(e.viewsDir(), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(e.viewsDir(), name+".json")
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("view %q already exists; saved views are immutable", name)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return v, nil
}

// View loads a previously saved view. It never rebuilds or refreshes the
// record: the returned view is exactly what was saved.
func (e *Engine) View(name string) (*View, error) {
	if err := validViewName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(e.viewsDir(), name+".json"))
	if err != nil {
		return nil, fmt.Errorf("view %q: %w", name, err)
	}
	var v View
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("view %q: corrupt: %w", name, err)
	}
	return &v, nil
}

// Views lists the names of all saved views, sorted.
func (e *Engine) Views() ([]string, error) {
	entries, err := os.ReadDir(e.viewsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, en := range entries {
		if !en.IsDir() && strings.HasSuffix(en.Name(), ".json") {
			names = append(names, strings.TrimSuffix(en.Name(), ".json"))
		}
	}
	sort.Strings(names)
	return names, nil
}

// VerifyView compares a saved view against the current workspace and
// returns one line per drift found: inputs whose content no longer matches
// what the build consumed, and recorded products that are missing or were
// modified on disk. The saved view itself is left untouched. An empty
// result means the workspace still matches the view exactly.
func (e *Engine) VerifyView(name string) ([]string, error) {
	v, err := e.View(name)
	if err != nil {
		return nil, err
	}
	var drift []string
	for _, a := range v.Actions {
		for in, recorded := range a.Inputs {
			h, err := hashFile(filepath.Join(e.root, in))
			switch {
			case err != nil:
				drift = append(drift, fmt.Sprintf("action %q: input %q no longer exists", a.ID, in))
			case h != recorded:
				drift = append(drift, fmt.Sprintf("action %q: input %q changed since view %q was built", a.ID, in, name))
			}
		}
		for out, recorded := range a.Outputs {
			h, err := hashFile(filepath.Join(e.root, out))
			switch {
			case err != nil:
				drift = append(drift, fmt.Sprintf("action %q: product %q is missing on disk", a.ID, out))
			case h != recorded:
				drift = append(drift, fmt.Sprintf("action %q: product %q on disk differs from the view", a.ID, out))
			}
		}
	}
	sort.Strings(drift)
	return drift, nil
}

// ActionDiff describes how one action differs between two views.
type ActionDiff struct {
	ID            string   `json:"id"`
	Change        string   `json:"change"` // same, inputs, outputs, status, only-in-a, only-in-b
	StatusA       string   `json:"status_a,omitempty"`
	StatusB       string   `json:"status_b,omitempty"`
	InputChanges  []string `json:"input_changes,omitempty"`  // paths whose content hash differs
	OutputChanges []string `json:"output_changes,omitempty"` // paths whose content hash differs
}

// ViewDiff is the result of comparing two saved views action by action.
type ViewDiff struct {
	A       string        `json:"a"`
	B       string        `json:"b"`
	Actions []*ActionDiff `json:"actions"`
}

// CompareViews compares two views (for example one per experiment
// candidate) and reports, per action, which inputs or outputs actually
// differ by content hash. Actions identical in both are marked "same".
func CompareViews(a, b *View) *ViewDiff {
	d := &ViewDiff{A: a.Name, B: b.Name}
	byID := map[string]*ActionReport{}
	for _, ar := range a.Actions {
		byID[ar.ID] = ar
	}
	seen := map[string]bool{}
	for _, br := range b.Actions {
		seen[br.ID] = true
		ar := byID[br.ID]
		if ar == nil {
			d.Actions = append(d.Actions, &ActionDiff{ID: br.ID, Change: "only-in-b", StatusB: br.Status})
			continue
		}
		ad := &ActionDiff{ID: br.ID, StatusA: ar.Status, StatusB: br.Status, Change: "same"}
		for in, hb := range br.Inputs {
			if ha, ok := ar.Inputs[in]; !ok || ha != hb {
				ad.InputChanges = append(ad.InputChanges, in)
			}
		}
		for in := range ar.Inputs {
			if _, ok := br.Inputs[in]; !ok {
				ad.InputChanges = append(ad.InputChanges, in)
			}
		}
		for out, hb := range br.Outputs {
			if ha, ok := ar.Outputs[out]; !ok || ha != hb {
				ad.OutputChanges = append(ad.OutputChanges, out)
			}
		}
		for out := range ar.Outputs {
			if _, ok := br.Outputs[out]; !ok {
				ad.OutputChanges = append(ad.OutputChanges, out)
			}
		}
		sort.Strings(ad.InputChanges)
		sort.Strings(ad.OutputChanges)
		switch {
		case len(ad.InputChanges) > 0:
			ad.Change = "inputs"
		case len(ad.OutputChanges) > 0:
			ad.Change = "outputs"
		case ad.StatusA != ad.StatusB:
			ad.Change = "status"
		}
		d.Actions = append(d.Actions, ad)
	}
	for _, ar := range a.Actions {
		if !seen[ar.ID] {
			d.Actions = append(d.Actions, &ActionDiff{ID: ar.ID, Change: "only-in-a", StatusA: ar.Status})
		}
	}
	sort.Slice(d.Actions, func(i, j int) bool { return d.Actions[i].ID < d.Actions[j].ID })
	return d
}
