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
	"strconv"
	"strings"
)

// cleanRel validates a workspace-relative path: it must stay inside the
// engine root (no absolute paths, no ".." traversal).
func cleanRel(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return "", fmt.Errorf("invalid workspace path %q", p)
	}
	c := filepath.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid workspace path %q", p)
	}
	return c, nil
}

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
	data, err := os.ReadFile(e.statePath)
	if err == nil {
		if err := json.Unmarshal(data, &e.state); err != nil {
			return nil, fmt.Errorf("corrupt state file: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Remove staging directories left behind by a process that died while
	// publishing. They are never committed products and must not be mistaken
	// for build results on restart.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, ent := range entries {
		if ent.IsDir() && strings.HasPrefix(ent.Name(), ".staging-") {
			_ = os.RemoveAll(filepath.Join(root, ent.Name()))
		}
	}
	return e, nil
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
	for _, in := range a.Inputs {
		if _, err := cleanRel(in); err != nil {
			return fmt.Errorf("action %q: %w", a.ID, err)
		}
	}
	cleaned := make([]string, len(a.Outputs))
	for i, out := range a.Outputs {
		c, err := cleanRel(out)
		if err != nil {
			return fmt.Errorf("action %q: %w", a.ID, err)
		}
		cleaned[i] = c
		if prev, ok := e.producer[out]; ok {
			return fmt.Errorf("output %q produced by both %q and %q", out, prev, a.ID)
		}
	}
	cp := a
	cp.Inputs = make([]string, len(a.Inputs))
	for i, in := range a.Inputs {
		c, _ := cleanRel(in)
		cp.Inputs[i] = c
	}
	cp.Outputs = cleaned
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
		h, err := hashFile(filepath.Join(e.root, in))
		if err != nil {
			return "", fmt.Errorf("action %q: input %q: %w", a.ID, in, err)
		}
		parts = append(parts, []byte(in), []byte(h))
	}
	return hashBytes(parts...), nil
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
		if err := e.run(a); err != nil {
			return err
		}
		e.Ran = append(e.Ran, id)
	}
	return nil
}

// run executes one action with staged outputs and atomic publish.
func (e *Engine) run(a *Action) error {
	tool := e.tools[a.Tool]
	staging, err := os.MkdirTemp(e.root, ".staging-"+a.ID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	inputs := make([]string, len(a.Inputs))
	for i, in := range a.Inputs {
		inputs[i] = filepath.Join(e.root, in)
	}
	staged := make([]string, len(a.Outputs))
	for i, out := range a.Outputs {
		// Mirror the workspace-relative path inside staging so distinct
		// outputs sharing a base name in different directories never
		// collide (e.g. dir1/x.txt and dir2/x.txt).
		sp := filepath.Join(staging, filepath.FromSlash(out))
		if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
			return err
		}
		staged[i] = sp
	}
	if err := tool.Run(a.Params, inputs, staged); err != nil {
		return fmt.Errorf("action %q failed: %w", a.ID, err)
	}

	// Verify all staged outputs exist and hash them before touching the
	// workspace. Publishing then replaces each destination atomically;
	// if any replacement fails the whole set is rolled back so the old
	// outputs remain a single consistent version.
	hashes := map[string]string{}
	for i, out := range a.Outputs {
		h, err := hashFile(staged[i])
		if err != nil {
			return fmt.Errorf("action %q did not produce output %q: %w", a.ID, out, err)
		}
		hashes[out] = h
	}

	// backups[i] is the pre-publish destination (moved aside) when that
	// output previously existed; published[i] marks completed renames.
	backups := make([]string, len(a.Outputs))
	var published []int
	rollback := func() {
		for j := len(published) - 1; j >= 0; j-- {
			i := published[j]
			dest := filepath.Join(e.root, a.Outputs[i])
			if backups[i] != "" {
				_ = os.Rename(backups[i], dest)
			} else {
				_ = os.Remove(dest)
			}
		}
	}

	for i, out := range a.Outputs {
		dest := filepath.Join(e.root, out)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			rollback()
			return fmt.Errorf("publish %q: %w", out, err)
		}
		if _, err := os.Stat(dest); err == nil {
			backupDir := filepath.Join(staging, ".old")
			if err := os.MkdirAll(backupDir, 0o755); err != nil {
				rollback()
				return fmt.Errorf("publish %q: %w", out, err)
			}
			backup := filepath.Join(backupDir, strconv.Itoa(i))
			if err := os.Rename(dest, backup); err != nil {
				rollback()
				return fmt.Errorf("publish %q: back up old output: %w", out, err)
			}
			backups[i] = backup
		} else if !errors.Is(err, os.ErrNotExist) {
			rollback()
			return fmt.Errorf("publish %q: %w", out, err)
		}
		if err := os.Rename(staged[i], dest); err != nil {
			if backups[i] != "" {
				_ = os.Rename(backups[i], dest)
			}
			rollback()
			return fmt.Errorf("publish %q: %w", out, err)
		}
		published = append(published, i)
	}

	fp, err := e.fingerprint(a)
	if err != nil {
		rollback()
		return err
	}
	e.state.Actions[a.ID] = &actionState{Fingerprint: fp, Outputs: hashes}
	return e.saveState()
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
