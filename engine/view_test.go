package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildengine/engine"
	"buildengine/tools"
)

func TestViewRecordsProvenanceAndIsImmutable(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)

	v, err := e.BuildView(context.Background(), "v1", "merge")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Complete {
		t.Fatalf("view incomplete: %+v", v.Incomplete)
	}
	// Provenance: every action records the exact inputs it consumed and
	// where each input came from.
	gen := v.Actions["gen"]
	if gen.Status != engine.StatusRebuilt {
		t.Fatalf("gen status %q", gen.Status)
	}
	if gen.InputFrom["src.txt"] != "" {
		t.Fatalf("src.txt should be a source file, got producer %q", gen.InputFrom["src.txt"])
	}
	if gen.Inputs["src.txt"] == "" {
		t.Fatal("gen did not record the hash of src.txt")
	}
	merge := v.Actions["merge"]
	if merge.InputFrom["mid/b.txt"] != "left" || merge.InputFrom["mid/c.txt"] != "right" {
		t.Fatalf("merge input producers wrong: %v", merge.InputFrom)
	}
	if len(merge.Outputs) != 1 || merge.Outputs["out/d.txt"] == "" {
		t.Fatalf("merge outputs not recorded: %v", merge.Outputs)
	}

	// Later source changes must not silently replace the saved view.
	write(t, root, "src.txt", "changed-later")
	if _, err := e.BuildView(context.Background(), "v1", "merge"); !errors.Is(err, engine.ErrViewExists) {
		t.Fatalf("want ErrViewExists, got %v", err)
	}
	loaded, err := e.LoadView("v1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Actions["gen"].Inputs["src.txt"] != gen.Inputs["src.txt"] {
		t.Fatal("saved view was rewritten by later source change")
	}
}

func TestViewPartialFailureContinuesIndependentBranches(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	write(t, root, "other.txt", "side")
	e := diamond(t, root)
	boom := errors.New("boom")
	e.RegisterTool("flaky", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		return boom
	}))
	// Independent branch that fails, plus a dependent of the failure.
	if err := e.AddAction(engine.Action{ID: "bad", Tool: "flaky", Inputs: []string{"other.txt"}, Outputs: []string{"bad/x.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAction(engine.Action{ID: "badDown", Tool: "copy", Inputs: []string{"bad/x.txt"}, Outputs: []string{"bad/y.txt"}}); err != nil {
		t.Fatal(err)
	}

	v, err := e.BuildView(context.Background(), "partial", "merge", "badDown")
	if err != nil {
		t.Fatal(err)
	}
	if v.Complete {
		t.Fatal("view should be incomplete")
	}
	// The diamond branch finished; only the failing branch did not.
	if got := v.Actions["merge"].Status; got != engine.StatusRebuilt {
		t.Fatalf("merge status %q", got)
	}
	if got := v.Actions["bad"].Status; got != engine.StatusFailed {
		t.Fatalf("bad status %q", got)
	}
	if got := v.Actions["badDown"].Status; got != engine.StatusSkipped {
		t.Fatalf("badDown status %q", got)
	}
	if len(v.Failed) != 1 || v.Failed[0] != "bad" {
		t.Fatalf("failed list %v", v.Failed)
	}
	if read(t, root, "out/d.txt") != "HELLO\nR:hello" {
		t.Fatal("independent branch did not complete")
	}
}

func TestViewCancelAndResume(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first action completes.
	first := true
	e2, _ := engine.New(root)
	e2.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	e2.RegisterTool("concat", engine.ToolFunc(tools.Concat))
	e2.RegisterTool("upper", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		if first {
			first = false
			cancel()
		}
		return tools.Upper(params, inputs, outputs)
	}))
	for _, a := range []engine.Action{
		{ID: "gen", Tool: "copy", Inputs: []string{"src.txt"}, Outputs: []string{"gen/a.txt"}},
		{ID: "left", Tool: "upper", Inputs: []string{"gen/a.txt"}, Outputs: []string{"mid/b.txt"}},
		{ID: "right", Tool: "copy", Params: map[string]string{"header": "R:"}, Inputs: []string{"gen/a.txt"}, Outputs: []string{"mid/c.txt"}},
		{ID: "merge", Tool: "concat", Inputs: []string{"mid/b.txt", "mid/c.txt"}, Outputs: []string{"out/d.txt"}},
	} {
		if err := e2.AddAction(a); err != nil {
			t.Fatal(err)
		}
	}
	v, err := e2.BuildView(ctx, "attempt-1", "merge")
	if err != nil {
		t.Fatal(err)
	}
	if v.Complete {
		t.Fatal("canceled view should be incomplete")
	}
	var canceled []string
	for id, p := range v.Actions {
		if p.Status == engine.StatusCanceled {
			canceled = append(canceled, id)
		}
	}
	if len(canceled) == 0 {
		t.Fatal("no action marked canceled")
	}

	// Resume in a new view: completed actions are reused, canceled ones run.
	e3 := diamond(t, root)
	v2, err := e3.BuildView(context.Background(), "attempt-2", "merge")
	if err != nil {
		t.Fatal(err)
	}
	if !v2.Complete {
		t.Fatalf("resumed view incomplete: %v", v2.Incomplete)
	}
	if v2.Actions["gen"].Status != engine.StatusReused {
		t.Fatalf("gen should be reused after cancel, got %q", v2.Actions["gen"].Status)
	}
	if read(t, root, "out/d.txt") != "HELLO\nR:hello" {
		t.Fatal("resume produced wrong output")
	}
}

func TestMidBuildDependencyDrift(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "v1")
	e, _ := engine.New(root)
	// A tool whose input changes underneath it mid-run.
	e.RegisterTool("mutate", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		data, err := os.ReadFile(inputs[0])
		if err != nil {
			return err
		}
		if err := os.WriteFile(outputs[0], data, 0o644); err != nil {
			return err
		}
		// Simulate a dependency changing while the build is in flight.
		return os.WriteFile(inputs[0], []byte("v2-drifted"), 0o644)
	}))
	if err := e.AddAction(engine.Action{ID: "work", Tool: "mutate", Inputs: []string{"src.txt"}, Outputs: []string{"out/o.txt"}}); err != nil {
		t.Fatal(err)
	}
	v, err := e.BuildView(context.Background(), "drift", "work")
	if err != nil {
		t.Fatal(err)
	}
	if v.Complete {
		t.Fatal("drifted view must not be complete")
	}
	if got := v.Actions["work"].Status; got != engine.StatusDrifted {
		t.Fatalf("status %q, want drifted", got)
	}
	// The product must not be treated as belonging to the view: state was
	// dropped, so the next build reruns the action.
	e2, _ := engine.New(root)
	e2.RegisterTool("copy", engine.ToolFunc(tools.Copy))
	if err := e2.AddAction(engine.Action{ID: "work", Tool: "copy", Inputs: []string{"src.txt"}, Outputs: []string{"out/o.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e2.Build("work"); err != nil {
		t.Fatal(err)
	}
	if len(e2.Ran) != 1 || e2.Ran[0] != "work" {
		t.Fatalf("drifted action was not rebuilt: %v", e2.Ran)
	}
	if got := read(t, root, "out/o.txt"); got != "v2-drifted" {
		t.Fatalf("rebuilt output %q", got)
	}
}

func TestExplainReuseAndChanges(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}

	// After a restart, every action explains why it is reusable.
	e2 := diamond(t, root)
	for _, id := range []string{"gen", "left", "right", "merge"} {
		ex, err := e2.Explain(id)
		if err != nil {
			t.Fatal(err)
		}
		if !ex.Reusable {
			t.Fatalf("%s not reusable after restart: %v", id, ex.Reasons)
		}
	}

	// A source change is reported as the true cause of the difference.
	write(t, root, "src.txt", "world")
	ex, err := e2.Explain("gen")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Reusable {
		t.Fatal("gen should not be reusable after source change")
	}
	joined := strings.Join(ex.Reasons, "; ")
	if !strings.Contains(joined, "src.txt") || !strings.Contains(joined, "changed") {
		t.Fatalf("explanation does not name the changed input: %v", ex.Reasons)
	}

	// An externally corrupted output is identified too.
	write(t, root, "out/d.txt", "TRASH")
	ex, err = e2.Explain("merge")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Reusable || !strings.Contains(strings.Join(ex.Reasons, "; "), "out/d.txt") {
		t.Fatalf("corruption not explained: %v", ex.Reasons)
	}
}

func TestDiffViewsIdentifiesRealCauses(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "one")
	e := diamond(t, root)
	v1, err := e.BuildView(context.Background(), "before", "merge")
	if err != nil {
		t.Fatal(err)
	}

	write(t, root, "src.txt", "two")
	v2, err := e.BuildView(context.Background(), "after", "merge")
	if err != nil {
		t.Fatal(err)
	}

	diff := engine.DiffViews(v1, v2)
	if len(diff.Actions) == 0 {
		t.Fatal("expected differences between views")
	}
	// gen's own input changed; downstream actions differ because their
	// inputs (gen's products) changed. The diff must name the inputs.
	foundGen := false
	for _, ad := range diff.Actions {
		if ad.ActionID == "gen" {
			foundGen = true
			if len(ad.ChangedInputs) != 1 || ad.ChangedInputs[0] != "src.txt" {
				t.Fatalf("gen changed inputs %v", ad.ChangedInputs)
			}
			if !ad.OutputsDiffer {
				t.Fatal("gen outputs should differ")
			}
		}
		if ad.ActionID == "merge" && !ad.OutputsDiffer {
			t.Fatal("merge outputs should differ")
		}
	}
	if !foundGen {
		t.Fatal("gen missing from diff")
	}
}

func TestExperimentCandidatesIsolationCompareAdopt(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "base")
	e := diamond(t, root)
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}

	exp, err := e.NewExperiment("cand-a", "cand-b")
	if err != nil {
		t.Fatal(err)
	}
	// Recreating the same experiment must not clobber existing evidence.
	if _, err := e.NewExperiment("cand-a", "cand-b"); !errors.Is(err, engine.ErrCandidateExists) {
		t.Fatalf("want ErrCandidateExists, got %v", err)
	}

	setup := func(dir string) *engine.Engine {
		ce, err := exp.Candidate(filepath.Base(dir))
		if err != nil {
			t.Fatal(err)
		}
		ce.RegisterTool("copy", engine.ToolFunc(tools.Copy))
		ce.RegisterTool("upper", engine.ToolFunc(tools.Upper))
		ce.RegisterTool("concat", engine.ToolFunc(tools.Concat))
		for _, a := range []engine.Action{
			{ID: "gen", Tool: "copy", Inputs: []string{"src.txt"}, Outputs: []string{"gen/a.txt"}},
			{ID: "left", Tool: "upper", Inputs: []string{"gen/a.txt"}, Outputs: []string{"mid/b.txt"}},
			{ID: "right", Tool: "copy", Params: map[string]string{"header": "R:"}, Inputs: []string{"gen/a.txt"}, Outputs: []string{"mid/c.txt"}},
			{ID: "merge", Tool: "concat", Inputs: []string{"mid/b.txt", "mid/c.txt"}, Outputs: []string{"out/d.txt"}},
		} {
			if err := ce.AddAction(a); err != nil {
				t.Fatal(err)
			}
		}
		return ce
	}

	// Candidate A: one modification set.
	ca := setup(exp.Dir("cand-a"))
	write(t, exp.Dir("cand-a"), "src.txt", "alpha")
	if err := ca.Build("merge"); err != nil {
		t.Fatal(err)
	}
	// Candidate B: an independent modification set whose build fails, so
	// its directory holds failure evidence.
	cb := setup(exp.Dir("cand-b"))
	write(t, exp.Dir("cand-b"), "src.txt", "beta")
	write(t, exp.Dir("cand-b"), "mid/b.txt", "CORRUPTED")
	boom := errors.New("boom")
	cb.RegisterTool("flaky", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error { return boom }))
	if err := cb.AddAction(engine.Action{ID: "extra", Tool: "flaky", Inputs: []string{"src.txt"}, Outputs: []string{"extra/x.txt"}}); err != nil {
		t.Fatal(err)
	}
	bv, err := cb.BuildView(context.Background(), "b-attempt", "merge", "extra")
	if err != nil {
		t.Fatal(err)
	}
	if bv.Complete || bv.Actions["extra"].Status != engine.StatusFailed {
		t.Fatalf("candidate B view: complete=%v extra=%v", bv.Complete, bv.Actions["extra"])
	}

	// Compare products: the merge outputs differ, sources differ.
	cmp, err := engine.CompareCandidates(ca, cb)
	if err != nil {
		t.Fatal(err)
	}
	diffPaths := map[string]bool{}
	for _, d := range cmp.Different {
		diffPaths[d.Path] = true
	}
	if !diffPaths["src.txt"] || !diffPaths["out/d.txt"] {
		t.Fatalf("comparison missing real differences: %+v", cmp.Different)
	}

	// Adopting A brings A's products and state into the base workspace.
	if err := exp.Adopt("cand-a"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "out/d.txt"); got != "ALPHA\nR:alpha" {
		t.Fatalf("adopted output %q", got)
	}
	// The base engine now reuses the adopted state: nothing reruns.
	e2 := diamond(t, root)
	if err := e2.Build("merge"); err != nil {
		t.Fatal(err)
	}
	if len(e2.Ran) != 0 {
		t.Fatalf("adopted state not reused: %v", e2.Ran)
	}

	// Candidate B's failure evidence survived the adoption of A.
	if _, err := os.Stat(filepath.Join(exp.Dir("cand-b"), ".views", "b-attempt.json")); err != nil {
		t.Fatalf("candidate B failure evidence lost: %v", err)
	}
	if got := read(t, exp.Dir("cand-b"), "src.txt"); got != "beta" {
		t.Fatalf("candidate B workspace disturbed: %q", got)
	}
}
