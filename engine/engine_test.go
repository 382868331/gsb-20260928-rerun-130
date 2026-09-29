package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"buildengine/engine"
	"buildengine/tools"
)

// diamond wires a diamond dependency graph on top of one source file:
//
//	src.txt --copy--> a.txt --+--upper--> b.txt --+
//	                          |                   +--concat--> d.txt
//	                          +--copy--> c.txt ---+
//
// gen is the shared prerequisite of b and c; merge depends on both.
func diamond(t *testing.T, root string) *engine.Engine {
	t.Helper()
	e, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	e.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	e.RegisterTool("upper", engine.ToolFunc(tools.Upper))
	e.RegisterTool("concat", engine.ToolFunc(tools.Concat))
	actions := []engine.Action{
		{ID: "gen", Tool: "copy", Inputs: []string{"src.txt"}, Outputs: []string{"gen/a.txt"}},
		{ID: "left", Tool: "upper", Inputs: []string{"gen/a.txt"}, Outputs: []string{"mid/b.txt"}},
		{ID: "right", Tool: "copy", Params: map[string]string{"header": "R:"}, Inputs: []string{"gen/a.txt"}, Outputs: []string{"mid/c.txt"}},
		{ID: "merge", Tool: "concat", Inputs: []string{"mid/b.txt", "mid/c.txt"}, Outputs: []string{"out/d.txt"}},
	}
	for _, a := range actions {
		if err := e.AddAction(a); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDiamondFullBuild(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}
	// Shared prerequisite gen runs exactly once; every action runs once.
	want := []string{"gen", "left", "merge", "right"}
	got := append([]string(nil), e.Ran...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
	if got := read(t, root, "out/d.txt"); got != "HELLO\nR:hello" {
		t.Fatalf("unexpected merge output %q", got)
	}
}

func TestIncrementalRebuildAfterLocalChange(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}

	// No-op rebuild: nothing reruns.
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}
	if len(e.Ran) != 0 {
		t.Fatalf("no-op build reran %v", e.Ran)
	}

	// Local modification of the source: everything downstream reruns once.
	write(t, root, "src.txt", "world")
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}
	want := []string{"gen", "left", "merge", "right"}
	got := append([]string(nil), e.Ran...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
	if got := read(t, root, "out/d.txt"); got != "WORLD\nR:world" {
		t.Fatalf("unexpected merge output %q", got)
	}
}

func TestUnaffectedActionsNotRerun(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	write(t, root, "other.txt", "side")
	e := diamond(t, root)
	// An independent branch off a different source.
	if err := e.AddAction(engine.Action{ID: "side", Tool: "upper", Inputs: []string{"other.txt"}, Outputs: []string{"side/s.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Build("merge", "side"); err != nil {
		t.Fatal(err)
	}

	// Touch only the diamond source: the independent branch must not rerun.
	write(t, root, "src.txt", "again")
	if err := e.Build("merge", "side"); err != nil {
		t.Fatal(err)
	}
	for _, id := range e.Ran {
		if id == "side" {
			t.Fatalf("unaffected action reran: %v", e.Ran)
		}
	}

	// Parameter change on one action reruns only it and its dependents.
	e2 := diamond(t, root)
	if err := e2.AddAction(engine.Action{ID: "side", Tool: "upper", Inputs: []string{"other.txt"}, Outputs: []string{"side/s.txt"}}); err != nil {
		t.Fatal(err)
	}
	// rebuild with modified params for "right"
	e3, _ := engine.New(root)
	e3.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	e3.RegisterTool("upper", engine.ToolFunc(tools.Upper))
	e3.RegisterTool("concat", engine.ToolFunc(tools.Concat))
	for _, a := range []engine.Action{
		{ID: "gen", Tool: "copy", Inputs: []string{"src.txt"}, Outputs: []string{"gen/a.txt"}},
		{ID: "left", Tool: "upper", Inputs: []string{"gen/a.txt"}, Outputs: []string{"mid/b.txt"}},
		{ID: "right", Tool: "copy", Params: map[string]string{"header": "RR:"}, Inputs: []string{"gen/a.txt"}, Outputs: []string{"mid/c.txt"}},
		{ID: "merge", Tool: "concat", Inputs: []string{"mid/b.txt", "mid/c.txt"}, Outputs: []string{"out/d.txt"}},
	} {
		if err := e3.AddAction(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := e3.Build("merge"); err != nil {
		t.Fatal(err)
	}
	want := []string{"merge", "right"}
	got := append([]string(nil), e3.Ran...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("param change ran %v, want %v", got, want)
	}
	_ = e2
}

func TestExternallyCorruptedOutput(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}

	// External process damages an intermediate product.
	write(t, root, "mid/b.txt", "GARBAGE")
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}
	// Only the corrupted action reruns: it restores b.txt to its original
	// content, so merge's inputs and output are still intact and merge is
	// correctly skipped. gen and right are untouched as well.
	want := []string{"left"}
	got := append([]string(nil), e.Ran...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
	if got := read(t, root, "out/d.txt"); got != "HELLO\nR:hello" {
		t.Fatalf("output not restored: %q", got)
	}

	// Corrupting the final product reruns only its producer.
	write(t, root, "out/d.txt", "TRASH")
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.Ran, []string{"merge"}) {
		t.Fatalf("ran %v, want [merge]", e.Ran)
	}
	if got := read(t, root, "out/d.txt"); got != "HELLO\nR:hello" {
		t.Fatalf("final output not restored: %q", got)
	}
}

func TestRestartRecovery(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: a brand-new engine over the same directory.
	e2 := diamond(t, root)
	if err := e2.Build("merge"); err != nil {
		t.Fatal(err)
	}
	if len(e2.Ran) != 0 {
		t.Fatalf("after restart reran %v", e2.Ran)
	}

	// Change after restart triggers a correct incremental rebuild.
	write(t, root, "src.txt", "resume")
	e3 := diamond(t, root)
	if err := e3.Build("merge"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "out/d.txt"); got != "RESUME\nR:resume" {
		t.Fatalf("unexpected output %q", got)
	}
}

func TestFailurePublishesNoPartialOutputs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	e.RegisterTool("flaky", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		// Write one staged output, then fail before producing the other.
		if err := os.WriteFile(outputs[0], []byte("half"), 0o644); err != nil {
			return err
		}
		return boom
	}))
	e.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	if err := e.AddAction(engine.Action{ID: "bad", Tool: "flaky", Inputs: []string{"src.txt"}, Outputs: []string{"out/x.txt", "out/y.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAction(engine.Action{ID: "down", Tool: "copy", Inputs: []string{"out/x.txt"}, Outputs: []string{"out/z.txt"}}); err != nil {
		t.Fatal(err)
	}

	err = e.Build("down")
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	// No half product, no downstream output, no state recorded.
	for _, f := range []string{"out/x.txt", "out/y.txt", "out/z.txt"} {
		if _, statErr := os.Stat(filepath.Join(root, f)); !os.IsNotExist(statErr) {
			t.Fatalf("partial output %q published", f)
		}
	}
	// Recovery: replace the failing tool with a working one, same model.
	e2, _ := engine.New(root)
	e2.RegisterTool("flaky", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		for i, o := range outputs {
			if err := os.WriteFile(o, []byte{byte('a' + i)}, 0o644); err != nil {
				return err
			}
		}
		return nil
	}))
	e2.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	if err := e2.AddAction(engine.Action{ID: "bad", Tool: "flaky", Inputs: []string{"src.txt"}, Outputs: []string{"out/x.txt", "out/y.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e2.AddAction(engine.Action{ID: "down", Tool: "copy", Inputs: []string{"out/x.txt"}, Outputs: []string{"out/z.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e2.Build("down"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "out/z.txt"); got != "a" {
		t.Fatalf("unexpected recovery output %q", got)
	}
}

func TestIncrementalMatchesFullBuild(t *testing.T) {
	// Full clean build.
	full := t.TempDir()
	write(t, full, "src.txt", "v1")
	e := diamond(t, full)
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}
	write(t, full, "src.txt", "v2")
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}

	// Incremental build arriving at the same change through two steps,
	// including a simulated restart.
	inc := t.TempDir()
	write(t, inc, "src.txt", "v1")
	e1 := diamond(t, inc)
	if err := e1.Build("merge"); err != nil {
		t.Fatal(err)
	}
	write(t, inc, "src.txt", "v2")
	e2 := diamond(t, inc) // restart
	if err := e2.Build("merge"); err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{"gen/a.txt", "mid/b.txt", "mid/c.txt", "out/d.txt"} {
		if got, want := read(t, inc, f), read(t, full, f); got != want {
			t.Fatalf("%s: incremental %q != full %q", f, got, want)
		}
	}
}

func TestCycleRejected(t *testing.T) {
	root := t.TempDir()
	e, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	e.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	a := engine.Action{ID: "a", Tool: "copy", Inputs: []string{"b.txt"}, Outputs: []string{"a.txt"}}
	b := engine.Action{ID: "b", Tool: "copy", Inputs: []string{"a.txt"}, Outputs: []string{"b.txt"}}
	if err := e.AddAction(a); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAction(b); err != nil {
		t.Fatal(err)
	}
	if err := e.Build("a"); err == nil {
		t.Fatal("expected cycle error")
	}
}
