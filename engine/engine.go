// Package engine implements a content-addressed incremental build engine.
//
// A build is modeled as a set of actions. Each action declares its tool,
// parameters, input files and output files. Dependencies between actions
// are derived from input/output path relationships, forming a DAG.
//
// Rebuild decisions are based purely on content hashes and parameters,
// never on timestamps. Shared prerequisites execute exactly once. Action
// outputs are staged in a temporary directory and atomically published, so
// a failed action never exposes partial outputs. State is persisted after
// every completed action, enabling restart recovery.
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Action describes one controlled generation step.
type Action struct {
	ID      string            // unique action identifier
	Tool    string            // registered tool name
	Params  map[string]string // tool parameters, part of the fingerprint
	Inputs  []string          // workspace-relative input paths
	Outputs []string          // workspace-relative output paths
}

// Tool is a controlled, registered generation action. The engine only ever
// runs tools from its registry; there is no repository auto-discovery.
// outputs are staged paths; the engine publishes them on success.
type Tool interface {
	Run(params map[string]string, inputs, outputs []string) error
}

// ToolFunc adapts a function to Tool.
type ToolFunc func(params map[string]string, inputs, outputs []string) error

func (f ToolFunc) Run(params map[string]string, inputs, outputs []string) error {
	return f(params, inputs, outputs)
}

type actionState struct {
	Fingerprint string
	Tool        string            // tool that produced the outputs
	Params      map[string]string // parameters the build actually used
	Inputs      map[string]string // input path -> content hash at build time
	Outputs     map[string]string // output path -> content hash
}

type diskState struct {
	Actions map[string]*actionState
}

// Engine executes actions incrementally over a workspace directory.
type Engine struct {
	root      string
	statePath string
	tools     map[string]Tool
	actions   map[string]*Action
	producer  map[string]string // output path -> action ID
	state     diskState

	// Ran records action IDs executed during the last Build, in order.
	Ran []string
}

// New opens (or creates) an engine rooted at dir. State is loaded from
// dir/.buildstate.json if present, enabling restart recovery.
func New(root string) (*Engine, error) {
	e := &Engine{
		root:     root,
		tools:    map[string]Tool{},
		actions:  map[string]*Action{},
		producer: map[string]string{},
	}
	e.statePath = filepath.Join(root, ".buildstate.json")
	e.state.Actions = map[string]*actionState{}
	// Sweep staging directories left behind by a crashed or killed build.
	// Such fragments are never recorded state and must never be mistaken
	// for published products.
	if entries, err := os.ReadDir(root); err == nil {
		for _, en := range entries {
			if en.IsDir() && strings.HasPrefix(en.Name(), ".staging-") {
				_ = os.RemoveAll(filepath.Join(root, en.Name()))
			}
		}
	}
	data, err := os.ReadFile(e.statePath)
	if err == nil {
		if err := json.Unmarshal(data, &e.state); err != nil {
			return nil, fmt.Errorf("corrupt state file: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return e, nil
}

// cleanRel validates p as a workspace-relative path that cannot escape
// the engine root. It returns the cleaned form.
func cleanRel(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be relative", p)
	}
	c := filepath.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the workspace", p)
	}
	return c, nil
}

// RegisterTool adds a controlled tool to the registry.
func (e *Engine) RegisterTool(name string, t Tool) { e.tools[name] = t }

// AddAction registers an action and wires its outputs into the dependency
// graph. It returns an error on duplicate IDs or conflicting producers.
func (e *Engine) AddAction(a Action) error {
	if a.ID == "" || a.Tool == "" {
		return errors.New("action requires ID and Tool")
	}
	if _, ok := e.actions[a.ID]; ok {
		return fmt.Errorf("duplicate action ID %q", a.ID)
	}
	if _, ok := e.tools[a.Tool]; !ok {
		return fmt.Errorf("action %q uses unregistered tool %q", a.ID, a.Tool)
	}
	if len(a.Outputs) == 0 {
		return fmt.Errorf("action %q declares no outputs", a.ID)
	}
	seen := map[string]bool{}
	for _, raw := range a.Inputs {
		in, err := cleanRel(raw)
		if err != nil {
			return err
		}
		if seen[in] {
			return fmt.Errorf("action %q lists input %q more than once", a.ID, in)
		}
		seen[in] = true
	}
	seen = map[string]bool{}
	for _, raw := range a.Outputs {
		out, err := cleanRel(raw)
		if err != nil {
			return err
		}
		if seen[out] {
			return fmt.Errorf("action %q lists output %q more than once", a.ID, out)
		}
		seen[out] = true
		if prev, ok := e.producer[out]; ok {
			return fmt.Errorf("output %q produced by both %q and %q", out, prev, a.ID)
		}
	}
	cp := a
	for i := range cp.Inputs {
		cp.Inputs[i], _ = cleanRel(cp.Inputs[i])
	}
	for i := range cp.Outputs {
		cp.Outputs[i], _ = cleanRel(cp.Outputs[i])
	}
	e.actions[a.ID] = &cp
	for _, out := range cp.Outputs {
		e.producer[out] = a.ID
	}
	return nil
}

// deps returns the IDs of actions producing a's inputs.
func (e *Engine) deps(a *Action) []string {
	seen := map[string]bool{}
	var out []string
	for _, in := range a.Inputs {
		if pid, ok := e.producer[in]; ok && pid != a.ID && !seen[pid] {
			seen[pid] = true
			out = append(out, pid)
		}
	}
	sort.Strings(out)
	return out
}

// closure returns all actions needed to build the target IDs.
func (e *Engine) closure(targets []string) (map[string]bool, error) {
	need := map[string]bool{}
	var visit func(id string) error
	visit = func(id string) error {
		if need[id] {
			return nil
		}
		a, ok := e.actions[id]
		if !ok {
			return fmt.Errorf("unknown action %q", id)
		}
		need[id] = true
		for _, d := range e.deps(a) {
			if err := visit(d); err != nil {
				return err
			}
		}
		return nil
	}
	for _, t := range targets {
		if err := visit(t); err != nil {
			return nil, err
		}
	}
	return need, nil
}

// topoSort orders needed actions so dependencies come first. Shared
// prerequisites appear exactly once. Cycles are rejected.
func (e *Engine) topoSort(need map[string]bool) ([]string, error) {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var order []string
	var visit func(id string) error
	visit = func(id string) error {
		switch color[id] {
		case gray:
			return fmt.Errorf("dependency cycle at action %q", id)
		case black:
			return nil
		}
		color[id] = gray
		for _, d := range e.deps(e.actions[id]) {
			if need[d] {
				if err := visit(d); err != nil {
					return err
				}
			}
		}
		color[id] = black
		order = append(order, id)
		return nil
	}
	ids := make([]string, 0, len(need))
	for id := range need {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func hashBytes(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return hashBytes(data), nil
}

// fingerprint computes the content+parameter key for an action.
func (e *Engine) fingerprint(a *Action) (string, error) {
	hashes := map[string]string{}
	for _, in := range a.Inputs {
		h, err := hashFile(filepath.Join(e.root, in))
		if err != nil {
			return "", fmt.Errorf("action %q: input %q: %w", a.ID, in, err)
		}
		hashes[in] = h
	}
	return fingerprintFrom(a, hashes), nil
}

// fingerprintFrom computes the content+parameter key for an action from
// already-known input content hashes.
func fingerprintFrom(a *Action, inputHashes map[string]string) string {
	parts := [][]byte{[]byte(a.Tool)}
	keys := make([]string, 0, len(a.Params))
	for k := range a.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, []byte(k), []byte(a.Params[k]))
	}
	inputs := append([]string(nil), a.Inputs...)
	sort.Strings(inputs)
	for _, in := range inputs {
		h := inputHashes[in]
		parts = append(parts, []byte(in), []byte(h))
	}
	return hashBytes(parts...)
}

// upToDate reports whether the action's recorded fingerprint still matches
// and every output exists on disk with its recorded content hash.
func (e *Engine) upToDate(a *Action) (bool, error) {
	st, ok := e.state.Actions[a.ID]
	if !ok {
		return false, nil
	}
	fp, err := e.fingerprint(a)
	if err != nil {
		return false, err
	}
	if fp != st.Fingerprint {
		return false, nil
	}
	for _, out := range a.Outputs {
		recorded, ok := st.Outputs[out]
		if !ok {
			return false, nil
		}
		actual, err := hashFile(filepath.Join(e.root, out))
		if err != nil || actual != recorded {
			return false, nil // missing or externally corrupted output
		}
	}
	return true, nil
}

// Build brings the named actions (and their transitive prerequisites) up to
// date. Actions whose inputs, parameters and outputs are unchanged are not
// re-executed. On failure, no partial outputs are published and state for
// the failed action is left untouched.
func (e *Engine) Build(targets ...string) error {
	e.Ran = nil
	need, err := e.closure(targets)
	if err != nil {
		return err
	}
	order, err := e.topoSort(need)
	if err != nil {
		return err
	}
	for _, id := range order {
		a := e.actions[id]
		ok, err := e.upToDate(a)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		if _, err := e.run(a); err != nil {
			return err
		}
		e.Ran = append(e.Ran, id)
	}
	return nil
}

// run executes one action with staged outputs and atomic publish. It
// returns the input content hashes the build actually consumed. If any
// input changes while the tool runs, the staged products are discarded,
// no state is recorded, and an error is returned: such a product cannot
// be attributed to a consistent source state and the action must be
// rebuilt.
func (e *Engine) run(a *Action) (map[string]string, error) {
	tool := e.tools[a.Tool]
	// Preflight inputs: an action may only consume published files that
	// actually exist, never a half-published result. Fingerprinting below
	// reads the same files for the cache key.
	preHashes := map[string]string{}
	for _, in := range a.Inputs {
		info, err := os.Stat(filepath.Join(e.root, in))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("action %q: missing input %q", a.ID, in)
			}
			return nil, fmt.Errorf("action %q: input %q: %w", a.ID, in, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("action %q: input %q is a directory", a.ID, in)
		}
		h, err := hashFile(filepath.Join(e.root, in))
		if err != nil {
			return nil, fmt.Errorf("action %q: input %q: %w", a.ID, in, err)
		}
		preHashes[in] = h
	}

	staging, err := os.MkdirTemp(e.root, ".staging-"+a.ID+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	inputs := make([]string, len(a.Inputs))
	for i, in := range a.Inputs {
		inputs[i] = filepath.Join(e.root, in)
	}
	staged := make([]string, len(a.Outputs))
	for i, out := range a.Outputs {
		// Mirror the output's workspace-relative path inside staging so
		// outputs with the same basename in different directories stay
		// distinct instead of collapsing onto one staged file.
		d := filepath.Join(staging, filepath.Dir(out))
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
		staged[i] = filepath.Join(staging, out)
	}
	if err := tool.Run(a.Params, inputs, staged); err != nil {
		return nil, fmt.Errorf("action %q failed: %w", a.ID, err)
	}

	// Verify all staged products before touching the workspace.
	hashes := map[string]string{}
	for i, out := range a.Outputs {
		h, err := hashFile(staged[i])
		if err != nil {
			return nil, fmt.Errorf("action %q did not produce output %q: %w", a.ID, out, err)
		}
		hashes[out] = h
	}

	// Re-hash every input after the tool ran. A dependency that changed
	// mid-build means the staged products were derived from a source state
	// that no longer exists; discard them instead of publishing a product
	// that belongs to no consistent view.
	for _, in := range a.Inputs {
		h, err := hashFile(filepath.Join(e.root, in))
		if err != nil {
			return nil, fmt.Errorf("action %q: input %q changed during build and is now unreadable: %w", a.ID, in, err)
		}
		if h != preHashes[in] {
			return nil, fmt.Errorf("action %q: input %q changed during build; products discarded, rebuild required", a.ID, in)
		}
	}
	fp := fingerprintFrom(a, preHashes)

	// Prepare destination directories before publishing anything.
	madeDirs := map[string]bool{}
	for _, out := range a.Outputs {
		dir := filepath.Dir(filepath.Join(e.root, out))
		rel, relErr := filepath.Rel(e.root, dir)
		if relErr != nil || rel == "." {
			continue
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		for n := 1; n <= len(parts); n++ {
			sub := filepath.Join(parts[:n]...)
			abs := filepath.Join(e.root, sub)
			if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
				continue
			}
			if mkErr := os.Mkdir(abs, 0o755); mkErr != nil {
				return nil, fmt.Errorf("publish %q: %w", out, mkErr)
			}
			madeDirs[sub] = true
		}
	}

	// Whole-product publish: move every output into place, tracking each
	// rename so a failure midway rolls back to the previous consistent
	// version. Old files are moved aside (not deleted) until all renames
	// have succeeded.
	type moved struct {
		out    string
		dest   string
		staged string
		backup string
		hadOld bool
	}
	var moves []moved
	published := 0
	oldState := e.state.Actions[a.ID]
	rollback := func() {
		for j := published - 1; j >= 0; j-- {
			m := moves[j]
			_ = os.RemoveAll(m.dest)
			if m.hadOld {
				_ = os.Rename(m.backup, m.dest)
			}
			_ = os.Rename(m.staged, filepath.Join(staging, m.out))
		}
		// Remove directories this publish created, innermost first.
		dirs := make([]string, 0, len(madeDirs))
		for d := range madeDirs {
			dirs = append(dirs, d)
		}
		sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
		for _, d := range dirs {
			_ = os.Remove(filepath.Join(e.root, d))
		}
	}

	for i, out := range a.Outputs {
		dest := filepath.Join(e.root, out)
		m := moved{out: out, dest: dest, staged: staged[i]}
		if info, statErr := os.Stat(dest); statErr == nil && !info.IsDir() {
			m.hadOld = true
			m.backup = filepath.Join(staging, ".old", fmt.Sprint(i))
			if err := os.MkdirAll(filepath.Dir(m.backup), 0o755); err != nil {
				return nil, fmt.Errorf("publish %q: %w", out, err)
			}
			if err := os.Rename(dest, m.backup); err != nil {
				rollback()
				return nil, fmt.Errorf("publish %q: %w", out, err)
			}
		}
		if err := os.Rename(staged[i], dest); err != nil {
			if m.hadOld {
				_ = os.Rename(m.backup, dest)
			}
			rollback()
			return nil, fmt.Errorf("publish %q: %w", out, err)
		}
		moves = append(moves, m)
		published++
	}

	// All products are published: commit state, then discard old versions.
	params := map[string]string{}
	for k, v := range a.Params {
		params[k] = v
	}
	newState := &actionState{
		Fingerprint: fp,
		Tool:        a.Tool,
		Params:      params,
		Inputs:      preHashes,
		Outputs:     hashes,
	}
	e.state.Actions[a.ID] = newState
	if err := e.saveState(); err != nil {
		rollback()
		if oldState != nil {
			e.state.Actions[a.ID] = oldState
		} else {
			delete(e.state.Actions, a.ID)
		}
		return nil, err
	}
	return preHashes, nil
}

func (e *Engine) saveState() error {
	data, err := json.MarshalIndent(&e.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := e.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, e.statePath)
}
