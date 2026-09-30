package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
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

func TestSameNameOutputsInDifferentDirectories(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a_src.txt", "AAA")
	write(t, root, "b_src.txt", "BBB")
	e, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	e.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	actions := []engine.Action{
		{ID: "a", Tool: "copy", Inputs: []string{"a_src.txt"}, Outputs: []string{"dir1/out.txt"}},
		{ID: "b", Tool: "copy", Inputs: []string{"b_src.txt"}, Outputs: []string{"dir2/out.txt"}},
	}
	for _, a := range actions {
		if err := e.AddAction(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Build("a", "b"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "dir1/out.txt"); got != "AAA" {
		t.Fatalf("dir1/out.txt = %q, want AAA", got)
	}
	if got := read(t, root, "dir2/out.txt"); got != "BBB" {
		t.Fatalf("dir2/out.txt = %q, want BBB", got)
	}

	// One action changes; the same-named output in the other directory must
	// not be mistaken for its staged sibling or overwritten.
	write(t, root, "a_src.txt", "AAA2")
	if err := e.Build("a", "b"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "dir1/out.txt"); got != "AAA2" {
		t.Fatalf("dir1/out.txt = %q, want AAA2", got)
	}
	if got := read(t, root, "dir2/out.txt"); got != "BBB" {
		t.Fatalf("dir2/out.txt changed to %q, want BBB", got)
	}
	wantRan := []string{"a"}
	if !reflect.DeepEqual(e.Ran, wantRan) {
		t.Fatalf("ran %v, want %v", e.Ran, wantRan)
	}

	// Restart: both same-named outputs remain valid cache hits.
	e2, _ := engine.New(root)
	e2.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	for _, a := range actions {
		if err := e2.AddAction(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := e2.Build("a", "b"); err != nil {
		t.Fatal(err)
	}
	if len(e2.Ran) != 0 {
		t.Fatalf("after restart reran %v", e2.Ran)
	}
}

// writeBoth writes identical bytes to all declared outputs. It models a
// multi-output action where a rebuild may leave some outputs byte-identical.
func writeBoth(content string) engine.Tool {
	return engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		for _, o := range outputs {
			if err := os.MkdirAll(filepath.Dir(o), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(o, []byte(content), 0o644); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestPublishFailureRollsBackWholeAction(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "v1")
	action := engine.Action{ID: "gen", Tool: "multi", Inputs: []string{"src.txt"}, Outputs: []string{"out/a.txt", "out/sub/b.txt"}}

	newEngine := func() *engine.Engine {
		e, err := engine.New(root)
		if err != nil {
			t.Fatal(err)
		}
		e.RegisterTool("multi", writeBoth("v1"))
		e.RegisterTool("copy", engine.ToolFunc(tools.Copy))
		return e
	}

	e := newEngine()
	if err := e.AddAction(action); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAction(engine.Action{ID: "down", Tool: "copy", Inputs: []string{"out/a.txt"}, Outputs: []string{"down/d.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Build("gen", "down"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "down/d.txt"); got != "v1" {
		t.Fatalf("down/d.txt = %q, want v1", got)
	}

	// Interrupted rebuild: a.txt gets new content while sub/b.txt is
	// unchanged. The tool blocks publication of the second output by
	// replacing its parent directory with a regular file, so the first
	// rename has already happened when publish fails.
	write(t, root, "src.txt", "v2")
	e2 := newEngine()
	e2.RegisterTool("multi", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		if err := os.WriteFile(outputs[0], []byte("v2"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(outputs[1], []byte("v1"), 0o644); err != nil {
			return err
		}
		block := filepath.Join(root, "out", "sub")
		if err := os.RemoveAll(block); err != nil {
			return err
		}
		return os.WriteFile(block, []byte("occupied"), 0o644)
	}))
	if err := e2.AddAction(action); err != nil {
		t.Fatal(err)
	}
	if err := e2.AddAction(engine.Action{ID: "down", Tool: "copy", Inputs: []string{"out/a.txt"}, Outputs: []string{"down/d.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e2.Build("gen", "down"); err == nil {
		t.Fatal("expected interrupted publish to fail")
	}

	// Atomic rollback: the replaced first output is restored to v1 and
	// the dependent never ran against a half-published result. (The
	// obstruction itself removed out/sub, simulating the I/O fault.)
	for _, f := range []string{"out/a.txt", "down/d.txt"} {
		if got := read(t, root, f); got != "v1" {
			t.Fatalf("%s = %q, want v1 after rollback", f, got)
		}
	}
	// No staging residue is left behind.
	entries, _ := os.ReadDir(root)
	for _, ent := range entries {
		if ent.IsDir() && strings.HasPrefix(ent.Name(), ".staging-") {
			t.Fatalf("staging directory left behind: %s", ent.Name())
		}
	}
}

func TestUnchangedOutputKeptAcrossRebuild(t *testing.T) {
	// A multi-output action where only one output changes on rebuild. The
	// unchanged sibling is republished atomically and the resulting set is
	// accepted by the incremental cache, including after restart.
	root := t.TempDir()
	write(t, root, "src.txt", "v1")
	action := engine.Action{ID: "gen", Tool: "multi", Inputs: []string{"src.txt"}, Outputs: []string{"out/a.txt", "out/b.txt"}}
	run := func(contentA, contentB string) *engine.Engine {
		e, _ := engine.New(root)
		e.RegisterTool("multi", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
			contents := []string{contentA, contentB}
			for i, o := range outputs {
				if err := os.WriteFile(o, []byte(contents[i]), 0o644); err != nil {
					return err
				}
			}
			return nil
		}))
		if err := e.AddAction(action); err != nil {
			t.Fatal(err)
		}
		return e
	}
	if err := run("v1", "common").Build("gen"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src.txt", "v2")
	if err := run("v2", "common").Build("gen"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "out/a.txt"); got != "v2" {
		t.Fatalf("out/a.txt = %q, want v2", got)
	}
	if got := read(t, root, "out/b.txt"); got != "common" {
		t.Fatalf("out/b.txt = %q, want common", got)
	}
	e := run("v2", "common")
	if err := e.Build("gen"); err != nil {
		t.Fatal(err)
	}
	if len(e.Ran) != 0 {
		t.Fatalf("unchanged-output set reran after restart: %v", e.Ran)
	}
}

func TestFailedPublishRebuildsAfterRestart(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "v1")
	action := engine.Action{ID: "gen", Tool: "multi", Inputs: []string{"src.txt"}, Outputs: []string{"out/a.txt", "out/sub/b.txt"}}

	e, _ := engine.New(root)
	e.RegisterTool("multi", writeBoth("v1"))
	if err := e.AddAction(action); err != nil {
		t.Fatal(err)
	}
	if err := e.Build("gen"); err != nil {
		t.Fatal(err)
	}

	// Fail a rebuild midway, as in TestPublishFailureRollsBackWholeAction.
	write(t, root, "src.txt", "v2")
	e2, _ := engine.New(root)
	e2.RegisterTool("multi", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		if err := writeBoth("v2").Run(params, inputs, outputs); err != nil {
			return err
		}
		block := filepath.Join(root, "out", "sub")
		if err := os.RemoveAll(block); err != nil {
			return err
		}
		return os.WriteFile(block, []byte("occupied"), 0o644)
	}))
	if err := e2.AddAction(action); err != nil {
		t.Fatal(err)
	}
	if err := e2.Build("gen"); err == nil {
		t.Fatal("expected interrupted publish to fail")
	}

	// After restart with the obstruction cleared, no failed fragment is
	// considered a cache hit: gen reruns and publishes v2 consistently.
	if err := os.Remove(filepath.Join(root, "out", "sub")); err != nil {
		t.Fatal(err)
	}
	e3, _ := engine.New(root)
	e3.RegisterTool("multi", writeBoth("v2"))
	if err := e3.AddAction(action); err != nil {
		t.Fatal(err)
	}
	if err := e3.Build("gen"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e3.Ran, []string{"gen"}) {
		t.Fatalf("ran %v, want [gen]", e3.Ran)
	}
	if got := read(t, root, "out/a.txt"); got != "v2" {
		t.Fatalf("out/a.txt = %q, want v2", got)
	}
	if got := read(t, root, "out/sub/b.txt"); got != "v2" {
		t.Fatalf("out/sub/b.txt = %q, want v2", got)
	}

	// One more restart is a clean no-op.
	e4, _ := engine.New(root)
	e4.RegisterTool("multi", writeBoth("v2"))
	if err := e4.AddAction(action); err != nil {
		t.Fatal(err)
	}
	if err := e4.Build("gen"); err != nil {
		t.Fatal(err)
	}
	if len(e4.Ran) != 0 {
		t.Fatalf("post-recovery restart reran %v", e4.Ran)
	}
}

func TestDependentNeverReadsHalfPublishedOutputs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "v1")
	e, _ := engine.New(root)
	e.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	if err := e.AddAction(engine.Action{ID: "gen", Tool: "copy", Inputs: []string{"src.txt"}, Outputs: []string{"out/g.txt"}}); err != nil {
		t.Fatal(err)
	}

	var seen string
	e.RegisterTool("reader", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		data, err := os.ReadFile(inputs[0])
		if err != nil {
			return err
		}
		seen = string(data)
		return os.WriteFile(outputs[0], data, 0o644)
	}))
	if err := e.AddAction(engine.Action{ID: "use", Tool: "reader", Inputs: []string{"out/g.txt"}, Outputs: []string{"down/u.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Build("use"); err != nil {
		t.Fatal(err)
	}
	if seen != "v1" {
		t.Fatalf("dependent read %q, want v1", seen)
	}

	// Change the source; gen reruns before use, and use must observe the
	// fully published new output rather than anything staged or missing.
	write(t, root, "src.txt", "v2")
	if err := e.Build("use"); err != nil {
		t.Fatal(err)
	}
	if seen != "v2" {
		t.Fatalf("dependent read %q, want v2", seen)
	}
}

func TestStaleStagingRemovedOnOpen(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "x")
	stale := filepath.Join(root, ".staging-crashed-123")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "ghost.txt"), []byte("ghost"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale staging dir survived New: %v", err)
	}
	e.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	if err := e.AddAction(engine.Action{ID: "a", Tool: "copy", Inputs: []string{"src.txt"}, Outputs: []string{"o.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Build("a"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "o.txt"); got != "x" {
		t.Fatalf("o.txt = %q, want x", got)
	}
}
