// Experiment candidates: fork the workspace into independent sandboxes.
//
// A candidate is a full copy of the current workspace (sources, products
// and build state) under .candidates/<name>/. Each candidate has its own
// state file, so two candidates never share cache entries: reuse inside a
// candidate is only possible when content hashes genuinely match, which is
// exactly when a cache entry is applicable. Adopting a candidate copies
// its recorded products back into the main workspace; the other candidates
// and their saved views (including failure evidence) are left untouched.
package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (e *Engine) candidatesDir() string { return filepath.Join(e.root, ".candidates") }

// ForkCandidate creates an independent experiment candidate starting from
// the engine's current state. The returned engine is rooted at the
// candidate directory and already has the parent's tools and actions
// registered. The name must be unused.
func (e *Engine) ForkCandidate(name string) (*Engine, error) {
	if err := validViewName(name); err != nil {
		return nil, err
	}
	dest := filepath.Join(e.candidatesDir(), name)
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf("candidate %q already exists", name)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	err := filepath.Walk(e.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(e.root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		top := strings.SplitN(rel, string(os.PathSeparator), 2)[0]
		if top == ".candidates" || top == ".views" || top == ".git" ||
			strings.HasPrefix(top, ".staging-") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
	if err != nil {
		os.RemoveAll(dest)
		return nil, err
	}
	ce, err := New(dest)
	if err != nil {
		os.RemoveAll(dest)
		return nil, err
	}
	for n, t := range e.tools {
		ce.tools[n] = t
	}
	for id, a := range e.actions {
		cp := *a
		ce.actions[id] = &cp
		for _, out := range a.Outputs {
			ce.producer[out] = id
		}
	}
	return ce, nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// AdoptCandidate applies the named candidate to this engine's workspace:
// every file recorded in the candidate's state (the modified sources the
// experiment consisted of, and the products built from them) is copied
// in and the candidate's action states are merged, so subsequent builds
// here reuse what the candidate proved. The candidate directory itself
// is not modified or deleted: its saved views and any failure evidence
// remain available for inspection.
func (e *Engine) AdoptCandidate(name string) error {
	if err := validViewName(name); err != nil {
		return err
	}
	dir := filepath.Join(e.candidatesDir(), name)
	data, err := os.ReadFile(filepath.Join(dir, ".buildstate.json"))
	if err != nil {
		return fmt.Errorf("candidate %q: %w", name, err)
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("candidate %q: corrupt state: %w", name, err)
	}
	for id, as := range st.Actions {
		files := map[string]string{}
		for in, h := range as.Inputs {
			files[in] = h
		}
		for out, h := range as.Outputs {
			files[out] = h
		}
		for rel, hash := range files {
			src := filepath.Join(dir, rel)
			h, err := hashFile(src)
			if err != nil {
				// An input that is another action's product is
				// covered by that action's own Outputs entry.
				if _, produced := st.Actions[e.producer[rel]]; produced && e.producer[rel] != "" {
					continue
				}
				return fmt.Errorf("candidate %q action %q: recorded file %q missing: %w", name, id, rel, err)
			}
			if h != hash {
				return fmt.Errorf("candidate %q action %q: file %q was modified after recording; refusing to adopt", name, id, rel)
			}
			dst := filepath.Join(e.root, rel)
			tmp := dst + ".adopt-tmp"
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := copyFile(src, tmp); err != nil {
				return err
			}
			if err := os.Rename(tmp, dst); err != nil {
				return err
			}
		}
		cp := *as
		e.state.Actions[id] = &cp
	}
	return e.saveState()
}
