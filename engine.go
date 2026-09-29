package depengine

import (
	"context"
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

const DefaultStateDir = ".depengine"

// GenerateFunc is the only kind of work an Engine can run. A generator may use
// project-authored Go libraries, but must not start shell commands or other
// repository build steps.
type GenerateFunc func(ctx context.Context, task *Task, inputs map[string][]byte) (map[string][]byte, error)

type Task struct {
	ID     string
	Action string
	Params map[string]any
	Deps   []string
	Inputs []string
	// Outputs are repository-relative file paths. A task owns every listed path.
	Outputs []string
}

type Engine struct {
	root     string
	stateDir string
	actions  map[string]GenerateFunc
}

func New(root string, actions map[string]GenerateFunc) *Engine {
	return NewWithStateDir(root, filepath.Join(root, DefaultStateDir), actions)
}

func NewWithStateDir(root, stateDir string, actions map[string]GenerateFunc) *Engine {
	copied := make(map[string]GenerateFunc, len(actions))
	for name, action := range actions {
		copied[name] = action
	}
	return &Engine{root: root, stateDir: stateDir, actions: copied}
}

type BuildResult struct {
	Ran     []string
	Skipped []string
	Counts  map[string]int
}

type record struct {
	Fingerprint string            `json:"fingerprint"`
	Outputs     map[string]string `json:"outputs"`
}

type stateFile struct {
	Version int               `json:"version"`
	Tasks   map[string]record `json:"tasks"`
}

// Build runs exactly the tasks whose task definition, parameters, source
// inputs, dependencies, or current output contents require it. It runs each
// successful task once, even when that task is reached through a diamond.
func (e *Engine) Build(ctx context.Context, tasks []*Task) (*BuildResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index, err := validate(tasks, e.actions)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(e.stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	state, err := e.loadState()
	if err != nil {
		return nil, err
	}

	order, err := topoOrder(tasks, index)
	if err != nil {
		return nil, err
	}
	fingerprints := make(map[string]string, len(order))
	result := &BuildResult{Counts: make(map[string]int, len(e.actions))}

	for _, id := range order {
		task := index[id]
		fp, err := e.fingerprint(task, index, fingerprints)
		if err != nil {
			return nil, err
		}
		fingerprints[id] = fp

		if e.isCurrent(task, fp, state.Tasks[id]) {
			result.Skipped = append(result.Skipped, id)
			continue
		}

		if err := ctx.Err(); err != nil {
			return nil, err
		}
		inputData, err := e.readInputs(task, index)
		if err != nil {
			return nil, fmt.Errorf("task %s: read inputs: %w", task.ID, err)
		}
		outputs, err := e.actions[task.Action](ctx, task, inputData)
		if err != nil {
			return nil, fmt.Errorf("task %s: %w", task.ID, err)
		}
		hashes, err := e.publish(task, outputs)
		if err != nil {
			return nil, fmt.Errorf("task %s: publish outputs: %w", task.ID, err)
		}
		state.Tasks[id] = record{Fingerprint: fp, Outputs: hashes}
		if err := e.saveState(state); err != nil {
			return nil, err
		}
		result.Ran = append(result.Ran, id)
		result.Counts[task.Action]++
	}
	return result, nil
}

func validate(tasks []*Task, actions map[string]GenerateFunc) (map[string]*Task, error) {
	index := make(map[string]*Task, len(tasks))
	owners := make(map[string]string)
	for _, task := range tasks {
		if task == nil {
			return nil, errors.New("task list contains nil task")
		}
		if strings.TrimSpace(task.ID) == "" {
			return nil, errors.New("task has empty id")
		}
		if _, exists := index[task.ID]; exists {
			return nil, fmt.Errorf("duplicate task id %q", task.ID)
		}
		if _, ok := actions[task.Action]; !ok {
			return nil, fmt.Errorf("task %s uses unregistered action %q", task.ID, task.Action)
		}
		for _, path := range task.Inputs {
			clean, err := cleanRootPath(path)
			if err != nil {
				return nil, fmt.Errorf("task %s input: %w", task.ID, err)
			}
			if owner, ok := owners[clean]; ok {
				return nil, fmt.Errorf("task %s cannot read output %s owned by task %s", task.ID, clean, owner)
			}
		}
		for _, path := range task.Outputs {
			clean, err := cleanRootPath(path)
			if err != nil {
				return nil, fmt.Errorf("task %s output: %w", task.ID, err)
			}
			if owner, exists := owners[clean]; exists {
				return nil, fmt.Errorf("output %s is owned by both %s and %s", clean, owner, task.ID)
			}
			owners[clean] = task.ID
		}
		index[task.ID] = task
	}
	for _, task := range tasks {
		seen := map[string]bool{}
		for _, dep := range task.Deps {
			if _, ok := index[dep]; !ok {
				return nil, fmt.Errorf("task %s depends on unknown task %q", task.ID, dep)
			}
			if seen[dep] {
				return nil, fmt.Errorf("task %s lists duplicate dependency %q", task.ID, dep)
			}
			seen[dep] = true
		}
	}
	return index, nil
}

func topoOrder(tasks []*Task, index map[string]*Task) ([]string, error) {
	const visiting, done = 1, 2
	state := make(map[string]int, len(tasks))
	var order []string
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case visiting:
			return fmt.Errorf("dependency cycle contains task %q", id)
		case done:
			return nil
		}
		state[id] = visiting
		deps := append([]string(nil), index[id].Deps...)
		sort.Strings(deps)
		for _, dep := range deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = done
		order = append(order, id)
		return nil
	}
	ids := make([]string, 0, len(tasks))
	for id := range index {
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

func (e *Engine) fingerprint(task *Task, index map[string]*Task, deps map[string]string) (string, error) {
	h := sha256.New()
	write := func(value string) { h.Write([]byte(value)); h.Write([]byte{0}) }
	write("task")
	write(task.ID)
	write(task.Action)
	params, err := canonicalJSON(task.Params)
	if err != nil {
		return "", fmt.Errorf("task %s parameters: %w", task.ID, err)
	}
	h.Write(params)
	inputs := append([]string(nil), task.Inputs...)
	sort.Strings(inputs)
	for _, path := range inputs {
		data, err := os.ReadFile(e.rootPath(path))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		write(path)
		h.Write(hashBytes(data))
	}
	depIDs := append([]string(nil), task.Deps...)
	sort.Strings(depIDs)
	for _, dep := range depIDs {
		write(dep)
		write(deps[dep])
	}
	outputs := append([]string(nil), task.Outputs...)
	sort.Strings(outputs)
	for _, output := range outputs {
		write(output)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (e *Engine) isCurrent(task *Task, fingerprint string, rec record) bool {
	if rec.Fingerprint != fingerprint || len(rec.Outputs) != len(task.Outputs) {
		return false
	}
	for _, path := range task.Outputs {
		data, err := os.ReadFile(e.rootPath(path))
		if err != nil {
			return false
		}
		if string(hashBytes(data)) != rec.Outputs[path] {
			return false
		}
	}
	return true
}

func (e *Engine) readInputs(task *Task, index map[string]*Task) (map[string][]byte, error) {
	inputs := make(map[string][]byte)
	for _, path := range task.Inputs {
		data, err := os.ReadFile(e.rootPath(path))
		if err != nil {
			return nil, err
		}
		inputs[path] = data
	}
	for _, depID := range task.Deps {
		for _, path := range index[depID].Outputs {
			data, err := os.ReadFile(e.rootPath(path))
			if err != nil {
				return nil, err
			}
			inputs[path] = data
		}
	}
	return inputs, nil
}

func cleanRootPath(path string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid repository path %q", path)
	}
	return filepath.ToSlash(clean), nil
}

func (e *Engine) rootPath(path string) string {
	return filepath.Join(e.root, filepath.FromSlash(path))
}

func hashBytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return []byte(hex.EncodeToString(sum[:]))
}

func canonicalJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}
