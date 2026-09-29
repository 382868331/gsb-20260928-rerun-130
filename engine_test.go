package depengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type counter struct{ calls map[string]int }

func (c *counter) action(_ context.Context, task *Task, inputs map[string][]byte) (map[string][]byte, error) {
	c.calls[task.ID]++
	if fail, _ := task.Params["fail"].(bool); fail {
		return nil, fmt.Errorf("intentional failure")
	}
	prefix, _ := task.Params["prefix"].(string)
	out := map[string][]byte{}
	for _, path := range task.Outputs {
		var parts []string
		parts = append(parts, prefix, path)
		for _, dep := range task.Deps {
			for _, input := range depTaskOutputs(inputs, dep) {
				parts = append(parts, string(input))
			}
		}
		for _, input := range task.Inputs {
			parts = append(parts, string(inputs[input]))
		}
		out[path] = []byte(strings.Join(parts, "|") + "\n")
	}
	return out, nil
}

func depTaskOutputs(inputs map[string][]byte, dep string) [][]byte {
	paths := map[string][]string{
		"shared": {"out/shared.txt"},
		"left":   {"out/left.txt"},
		"right":  {"out/right.txt"},
		"join":   {"out/join.txt"},
	}[dep]
	var values [][]byte
	for _, path := range paths {
		if value, ok := inputs[path]; ok {
			values = append(values, value)
		}
	}
	return values
}

func diamondTasks() []*Task {
	return []*Task{
		{ID: "shared", Action: "gen", Params: map[string]any{"prefix": "s"}, Inputs: []string{"src/shared.txt"}, Outputs: []string{"out/shared.txt"}},
		{ID: "left", Action: "gen", Params: map[string]any{"prefix": "l"}, Deps: []string{"shared"}, Outputs: []string{"out/left.txt"}},
		{ID: "right", Action: "gen", Params: map[string]any{"prefix": "r"}, Deps: []string{"shared"}, Outputs: []string{"out/right.txt"}},
		{ID: "join", Action: "gen", Params: map[string]any{"prefix": "j"}, Deps: []string{"left", "right"}, Outputs: []string{"out/join.txt"}},
	}
}

func newTestEngine(t *testing.T) (*Engine, *counter) {
	t.Helper()
	root := t.TempDir()
	calls := &counter{calls: map[string]int{}}
	engine := New(root, map[string]GenerateFunc{"gen": calls.action})
	return engine, calls
}

func writeFile(t *testing.T, engine *Engine, path, content string) {
	t.Helper()
	full := engine.rootPath(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, engine *Engine, path string) string {
	t.Helper()
	data, err := os.ReadFile(engine.rootPath(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertRan(t *testing.T, result *BuildResult, want ...string) {
	t.Helper()
	if strings.Join(result.Ran, ",") != strings.Join(want, ",") {
		t.Fatalf("ran %v, want %v; skipped %v", result.Ran, want, result.Skipped)
	}
}

func TestDiamondIncrementalRestartAndEquivalence(t *testing.T) {
	engine, calls := newTestEngine(t)
	writeFile(t, engine, "src/shared.txt", "shared-v1\n")

	result, err := engine.Build(context.Background(), diamondTasks())
	if err != nil {
		t.Fatal(err)
	}
	assertRan(t, result, "shared", "left", "right", "join")
	if calls.calls["shared"] != 1 {
		t.Fatalf("shared prerequisite ran %d times", calls.calls["shared"])
	}
	fullJoin := readFile(t, engine, "out/join.txt")
	fullLeft := readFile(t, engine, "out/left.txt")
	fullRight := readFile(t, engine, "out/right.txt")

	result, err = engine.Build(context.Background(), diamondTasks())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ran) != 0 {
		t.Fatalf("unchanged build ran %v", result.Ran)
	}

	// A new engine instance represents restarting the process. Only persisted
	// content and fingerprints decide what is current.
	restarted := New(engine.root, map[string]GenerateFunc{"gen": calls.action})
	result, err = restarted.Build(context.Background(), diamondTasks())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ran) != 0 {
		t.Fatalf("restarted build ran %v", result.Ran)
	}

	writeFile(t, engine, "src/shared.txt", "shared-v2\n")
	result, err = engine.Build(context.Background(), diamondTasks())
	if err != nil {
		t.Fatal(err)
	}
	assertRan(t, result, "shared", "left", "right", "join")
	if got := readFile(t, engine, "out/join.txt"); got == fullJoin {
		t.Fatal("aggregate did not incorporate changed prerequisite content")
	}

	// Restore the original input and rebuild. The final artifacts must equal a
	// build from the original graph, not a timestamp-sensitive cache result.
	writeFile(t, engine, "src/shared.txt", "shared-v1\n")
	if _, err := engine.Build(context.Background(), diamondTasks()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, engine, "out/left.txt"); got != fullLeft {
		t.Fatalf("left mismatch:\n%q\n%q", got, fullLeft)
	}
	if got := readFile(t, engine, "out/right.txt"); got != fullRight {
		t.Fatalf("right mismatch:\n%q\n%q", got, fullRight)
	}
	if got := readFile(t, engine, "out/join.txt"); got != fullJoin {
		t.Fatalf("join mismatch:\n%q\n%q", got, fullJoin)
	}
}

func TestUnaffectedTaskNotRerunWhenCorruptedOutputRepaired(t *testing.T) {
	engine, calls := newTestEngine(t)
	writeFile(t, engine, "src/shared.txt", "shared\n")
	if _, err := engine.Build(context.Background(), diamondTasks()); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(engine.rootPath("out/right.txt"), []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Build(context.Background(), diamondTasks())
	if err != nil {
		t.Fatal(err)
	}
	assertRan(t, result, "right")
	if calls.calls["join"] != 1 {
		t.Fatalf("downstream join ran %d times", calls.calls["join"])
	}
}

func TestParameterChangeRebuildsAffectedBranch(t *testing.T) {
	engine, _ := newTestEngine(t)
	writeFile(t, engine, "src/shared.txt", "shared\n")
	tasks := diamondTasks()
	if _, err := engine.Build(context.Background(), tasks); err != nil {
		t.Fatal(err)
	}

	for _, task := range tasks {
		if task.ID == "left" {
			task.Params["prefix"] = "left-v2"
		}
	}
	result, err := engine.Build(context.Background(), tasks)
	if err != nil {
		t.Fatal(err)
	}
	assertRan(t, result, "left", "join")
	if !strings.Contains(readFile(t, engine, "out/left.txt"), "left-v2") {
		t.Fatal("changed parameter did not affect output content")
	}
}

func TestFailurePublishesNoPartialOutputs(t *testing.T) {
	engine, _ := newTestEngine(t)
	writeFile(t, engine, "src/shared.txt", "shared\n")
	tasks := append(diamondTasks(), &Task{
		ID: "bad", Action: "gen", Deps: []string{"join"},
		Params:  map[string]any{"fail": true},
		Outputs: []string{"out/bad-one.txt", "out/nested/bad-two.txt"},
	})
	_, err := engine.Build(context.Background(), tasks)
	if err == nil || !strings.Contains(err.Error(), "intentional failure") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(engine.rootPath("out/bad-one.txt")); !os.IsNotExist(err) {
		t.Fatalf("first failed output exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(engine.rootPath("out/nested/bad-two.txt")); !os.IsNotExist(err) {
		t.Fatalf("second failed output exists or stat failed unexpectedly: %v", err)
	}

	// The failed task is absent from state, so a retry without the failure flag
	// still starts from clean declared outputs.
	for _, task := range tasks {
		if task.ID == "bad" {
			delete(task.Params, "fail")
		}
	}
	result, err := engine.Build(context.Background(), tasks)
	if err != nil {
		t.Fatal(err)
	}
	assertRan(t, result, "bad")
}

func TestRejectsUnregisteredActionAndUnsafePath(t *testing.T) {
	engine, _ := newTestEngine(t)
	if _, err := engine.Build(context.Background(), []*Task{{ID: "x", Action: "shell", Outputs: []string{"x"}}}); err == nil {
		t.Fatal("expected unregistered action error")
	}
	writeFile(t, engine, "src/shared.txt", "shared\n")
	tasks := diamondTasks()
	tasks[0].Outputs = []string{"../outside.txt"}
	if _, err := engine.Build(context.Background(), tasks); err == nil {
		t.Fatal("expected unsafe output path error")
	}
}
