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

func reportOf(t *testing.T, e *engine.Engine, targets ...string) *engine.BuildReport {
	t.Helper()
	rep := e.BuildReport(context.Background(), targets...)
	if rep.Err != nil {
		t.Fatalf("build report: %v", rep.Err)
	}
	if !rep.OK() {
		t.Fatalf("report not OK: %+v", rep)
	}
	return rep
}

func TestSavedViewIsImmutableAndVerifiable(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)

	rep := reportOf(t, e, "merge")
	v, err := e.SaveView("v1", rep)
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "v1" || len(v.Actions) != 4 {
		t.Fatalf("unexpected view %+v", v)
	}
	// The view records the actual inputs, incl. shared dependency gen.
	merge := rep.Get("merge")
	if merge.Inputs["mid/b.txt"] == "" || merge.Inputs["mid/c.txt"] == "" {
		t.Fatalf("view did not record merge inputs: %+v", merge.Inputs)
	}

	// Duplicate name rejected: a saved view can never be replaced.
	if _, err := e.SaveView("v1", rep); err == nil {
		t.Fatal("expected error overwriting saved view")
	}

	// Later source changes do not alter the saved view.
	write(t, root, "src.txt", "changed")
	got, err := e.View("v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Actions[0].Inputs["src.txt"] != v.Actions[0].Inputs["src.txt"] {
		t.Fatal("saved view was silently replaced")
	}

	// VerifyView reports the drift without touching the view.
	drift, err := e.VerifyView("v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) == 0 || !strings.Contains(drift[0], "src.txt") {
		t.Fatalf("expected drift mentioning src.txt, got %v", drift)
	}
	if _, err := e.View("v1"); err != nil {
		t.Fatal(err)
	}
}

func TestInputChangedDuringBuildIsNotPublished(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	// A tool that rewrites its own input mid-build: the product would
	// belong to no consistent source state.
	e.RegisterTool("mutate", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		if err := os.WriteFile(inputs[0], []byte("sneaky"), 0o644); err != nil {
			return err
		}
		return tools.Copy(params, inputs, outputs)
	}))
	if err := e.AddAction(engine.Action{ID: "gen", Tool: "mutate", Inputs: []string{"src.txt"}, Outputs: []string{"out/a.txt"}}); err != nil {
		t.Fatal(err)
	}
	err = e.Build("gen")
	if err == nil || !strings.Contains(err.Error(), "changed during build") {
		t.Fatalf("expected changed-during-build error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "out/a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("product of an inconsistent build must not be published")
	}
	// No state recorded: the action is not considered done.
	ex, err := e.Explain("gen")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Reusable {
		t.Fatal("inconsistent build must not be reusable")
	}
}

func TestPartialFailureAndContinueAfterCancel(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	write(t, root, "bad.txt", "boom")
	e := diamond(t, root)
	e.RegisterTool("fail", engine.ToolFunc(func(map[string]string, []string, []string) error {
		return errors.New("always fails")
	}))
	// Independent failing target next to the diamond.
	if err := e.AddAction(engine.Action{ID: "bad", Tool: "fail", Inputs: []string{"bad.txt"}, Outputs: []string{"out/bad.txt"}}); err != nil {
		t.Fatal(err)
	}
	rep := e.BuildReport(context.Background(), "merge", "bad")
	if rep.Err == nil {
		t.Fatal("expected first failure recorded")
	}
	if got := rep.Get("bad").Status; got != engine.StatusFailed {
		t.Fatalf("bad status %q", got)
	}
	// The unaffected target still completed.
	if got := rep.Get("merge").Status; got != engine.StatusRan {
		t.Fatalf("merge status %q", got)
	}
	if got := read(t, root, "out/d.txt"); got != "HELLO\nR:hello" {
		t.Fatalf("unexpected merge output %q", got)
	}

	// Cancellation: nothing new runs, completed state persists.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep = e.BuildReport(ctx, "merge")
	if !rep.Cancelled {
		t.Fatal("expected cancellation")
	}
	// merge was already built, so it is reported reused even when the
	// cancellation check happens first for later actions.
	write(t, root, "src.txt", "again")
	rep = e.BuildReport(ctx, "merge")
	if !rep.Cancelled {
		t.Fatal("expected cancellation")
	}
	if rep.Get("gen").Status != engine.StatusCancelled {
		t.Fatalf("gen status %q", rep.Get("gen").Status)
	}
	// After a restart, the build continues from persisted state.
	e2 := diamond(t, root)
	rep = reportOf(t, e2, "merge")
	if rep.Get("gen").Status != engine.StatusRan {
		t.Fatalf("gen should rerun after source change, got %q", rep.Get("gen").Status)
	}
	if got := read(t, root, "out/d.txt"); got != "AGAIN\nR:again" {
		t.Fatalf("unexpected merge output %q", got)
	}
}

func TestExplainAcrossRestart(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}
	// Restart: explanations come from persisted state.
	e = diamond(t, root)
	ex, err := e.Explain("merge")
	if err != nil {
		t.Fatal(err)
	}
	if !ex.Reusable {
		t.Fatalf("expected reusable, got %v", ex.Reasons)
	}

	write(t, root, "src.txt", "world")
	ex, _ = e.Explain("gen")
	if len(ex.Reasons) != 1 || !strings.Contains(ex.Reasons[0], "src.txt") {
		t.Fatalf("expected src.txt change reason, got %v", ex.Reasons)
	}
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}
	// Corrupt a product on disk: pinpointed as the reason.
	write(t, root, "mid/b.txt", "tampered")
	ex, _ = e.Explain("left")
	if ex.Reusable || !strings.Contains(strings.Join(ex.Reasons, ";"), "modified on disk") {
		t.Fatalf("expected modified-output reason, got %v", ex.Reasons)
	}
}

func TestCandidatesAreIndependentAndAdoptKeepsEvidence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	if err := e.Build("merge"); err != nil {
		t.Fatal(err)
	}
	base := reportOf(t, e, "merge")
	if _, err := e.SaveView("base", base); err != nil {
		t.Fatal(err)
	}

	candA, err := e.ForkCandidate("exp-a")
	if err != nil {
		t.Fatal(err)
	}
	candB, err := e.ForkCandidate("exp-b")
	if err != nil {
		t.Fatal(err)
	}
	// Two independent modification sets from the same known state.
	write(t, filepath.Join(root, ".candidates", "exp-a"), "src.txt", "alpha")
	write(t, filepath.Join(root, ".candidates", "exp-b"), "src.txt", "beta")

	repA := candA.BuildReport(context.Background(), "merge")
	repB := candB.BuildReport(context.Background(), "merge")
	if !repA.OK() || !repB.OK() {
		t.Fatalf("candidate builds failed: %v / %v", repA.Err, repB.Err)
	}
	// No cache cross-use: each candidate reran its own changed actions.
	if repA.Get("gen").Status != engine.StatusRan || repB.Get("gen").Status != engine.StatusRan {
		t.Fatalf("candidates must rebuild changed actions: %q/%q",
			repA.Get("gen").Status, repB.Get("gen").Status)
	}
	if got := read(t, filepath.Join(root, ".candidates", "exp-a"), "out/d.txt"); got != "ALPHA\nR:alpha" {
		t.Fatalf("candidate A output %q", got)
	}
	if got := read(t, filepath.Join(root, ".candidates", "exp-b"), "out/d.txt"); got != "BETA\nR:beta" {
		t.Fatalf("candidate B output %q", got)
	}

	// Save a view per candidate and compare: the differing input is
	// src.txt, and every product downstream differs.
	vA, err := candA.SaveView("result", repA)
	if err != nil {
		t.Fatal(err)
	}
	vB, err := candB.SaveView("result", repB)
	if err != nil {
		t.Fatal(err)
	}
	diff := engine.CompareViews(vA, vB)
	var genDiff *engine.ActionDiff
	for _, d := range diff.Actions {
		if d.ID == "gen" {
			genDiff = d
		}
	}
	if genDiff == nil || genDiff.Change != "inputs" ||
		len(genDiff.InputChanges) != 1 || genDiff.InputChanges[0] != "src.txt" {
		t.Fatalf("expected src.txt as the differing input, got %+v", genDiff)
	}

	// Adopting A updates the main workspace; B's evidence survives.
	if err := e.AdoptCandidate("exp-a"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "out/d.txt"); got != "ALPHA\nR:alpha" {
		t.Fatalf("adopted output %q", got)
	}
	if rep := reportOf(t, e, "merge"); rep.Get("merge").Status != engine.StatusReused {
		t.Fatalf("adopted state should be reusable, got %q", rep.Get("merge").Status)
	}
	if _, err := candB.View("result"); err != nil {
		t.Fatalf("candidate B view must survive adopting A: %v", err)
	}
	if got := read(t, filepath.Join(root, ".candidates", "exp-b"), "out/d.txt"); got != "BETA\nR:beta" {
		t.Fatalf("candidate B products must survive adopting A, got %q", got)
	}
}

func TestSharedDependencyShownOnce(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	for _, id := range []string{"left", "right"} {
		deps, err := e.Dependencies(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 1 || deps[0] != "gen" {
			t.Fatalf("%s deps %v, want [gen]", id, deps)
		}
	}
	rep := reportOf(t, e, "merge")
	ran := 0
	for _, a := range rep.Actions {
		if a.ID == "gen" && a.Status == engine.StatusRan {
			ran++
		}
	}
	if ran != 1 {
		t.Fatalf("shared prerequisite ran %d times", ran)
	}
}
