// experiment.go implements isolated experiment candidates: from one known
// workspace state, two (or more) independent sets of modifications can be
// tried side by side and their products compared.
//
// Each candidate gets a full copy of the workspace, including the persisted
// build state, so both start from the same known cache. After that the
// copies diverge: a candidate never reads another candidate's state or
// products, so no inapplicable cache entries can leak between them.
// Adopting one candidate copies its products and state back into the base
// workspace; the other candidates' directories, including any failure
// evidence recorded there, are left untouched.
package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrCandidateExists is returned when an experiment candidate directory
// already exists, so a new experiment cannot clobber a previous one's
// evidence.
var ErrCandidateExists = errors.New("experiment candidate already exists")

// Experiment manages a set of isolated candidate workspaces derived from
// one base engine's current state.
type Experiment struct {
	base  string
	names []string
}

func experimentsDir(root string) string { return filepath.Join(root, ".experiments") }

// Dir returns the workspace directory of a candidate.
func (exp *Experiment) Dir(name string) string {
	return filepath.Join(experimentsDir(exp.base), name)
}

// Names returns the candidate names in creation order.
func (exp *Experiment) Names() []string { return append([]string(nil), exp.names...) }

// skipped top-level entries when snapshotting or adopting: experiment and
// view bookkeeping never crosses between base and candidates.
func snapshotExcluded(name string) bool {
	return name == ".experiments" || name == ".views" || strings.HasPrefix(name, ".staging-")
}

// NewExperiment snapshots the engine's current workspace (sources, products
// and persisted state) into one isolated directory per candidate name. It
// fails without touching anything if any candidate directory already
// exists, so previous experiment evidence is never silently overwritten.
func (e *Engine) NewExperiment(names ...string) (*Experiment, error) {
	if len(names) == 0 {
		return nil, errors.New("experiment needs at least one candidate")
	}
	seen := map[string]bool{}
	exp := &Experiment{base: e.root}
	for _, n := range names {
		if err := validViewName(n); err != nil {
			return nil, fmt.Errorf("candidate: %w", err)
		}
		if seen[n] {
			return nil, fmt.Errorf("duplicate candidate %q", n)
		}
		seen[n] = true
		if _, err := os.Stat(exp.Dir(n)); err == nil {
			return nil, fmt.Errorf("%w: %q", ErrCandidateExists, n)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	for _, n := range names {
		if err := copyTree(e.root, exp.Dir(n), snapshotExcluded); err != nil {
			return nil, fmt.Errorf("snapshot candidate %q: %w", n, err)
		}
	}
	exp.names = append(exp.names, names...)
	return exp, nil
}

// Candidate opens an engine over the candidate's isolated workspace. The
// caller registers tools and actions on it exactly as on the base engine.
func (exp *Experiment) Candidate(name string) (*Engine, error) {
	if _, err := os.Stat(exp.Dir(name)); err != nil {
		return nil, fmt.Errorf("candidate %q: %w", name, err)
	}
	return New(exp.Dir(name))
}

// Adopt copies the chosen candidate's products and persisted state back
// into the base workspace, overwriting base files with the candidate's
// versions. Other candidates are not touched: their directories keep
// whatever success or failure evidence they contain. The experiment
// directories are kept for inspection; call Cleanup to remove them.
func (exp *Experiment) Adopt(name string) error {
	src := exp.Dir(name)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("candidate %q: %w", name, err)
	}
	return copyTree(src, exp.base, snapshotExcluded)
}

// Cleanup removes all candidate directories. Evidence is destroyed only
// when the caller explicitly asks for it.
func (exp *Experiment) Cleanup() error {
	return os.RemoveAll(experimentsDir(exp.base))
}

// copyTree recursively copies regular files and directories from src to
// dst, skipping top-level entries for which exclude(name) is true.
func copyTree(src, dst string, exclude func(string) bool) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		top := strings.SplitN(rel, string(os.PathSeparator), 2)[0]
		if exclude(top) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// FileDiff describes one path whose content differs between two candidates.
type FileDiff struct {
	Path  string
	AHash string // empty when absent in A
	BHash string // empty when absent in B
}

// Comparison reports how the file trees of two candidates differ.
type Comparison struct {
	Same      []string   // identical content in both
	Different []FileDiff // present in both, different content
	OnlyA     []string
	OnlyB     []string
}

// CompareCandidates hashes every file in both candidate workspaces
// (excluding experiment/view bookkeeping) and reports which products are
// identical, which differ, and which exist on only one side.
func CompareCandidates(a, b *Engine) (*Comparison, error) {
	ha, err := hashTree(a.root)
	if err != nil {
		return nil, err
	}
	hb, err := hashTree(b.root)
	if err != nil {
		return nil, err
	}
	cmp := &Comparison{}
	paths := map[string]bool{}
	for p := range ha {
		paths[p] = true
	}
	for p := range hb {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		fa, oka := ha[p]
		fb, okb := hb[p]
		switch {
		case oka && !okb:
			cmp.OnlyA = append(cmp.OnlyA, p)
		case !oka && okb:
			cmp.OnlyB = append(cmp.OnlyB, p)
		case fa == fb:
			cmp.Same = append(cmp.Same, p)
		default:
			cmp.Different = append(cmp.Different, FileDiff{Path: p, AHash: fa, BHash: fb})
		}
	}
	return cmp, nil
}

func hashTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		top := strings.SplitN(rel, string(os.PathSeparator), 2)[0]
		if snapshotExcluded(top) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		h, err := hashFile(path)
		if err != nil {
			return err
		}
		out[rel] = h
		return nil
	})
	return out, err
}
