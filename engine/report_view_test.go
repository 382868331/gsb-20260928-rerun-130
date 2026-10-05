package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func TestFailedActionKeepsStartFingerprintAndProvenance(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	write(t, root, "bad.txt", "boom")
	e := diamond(t, root)
	e.RegisterTool("fail", engine.ToolFunc(func(map[string]string, []string, []string) error {
		return errors.New("always fails")
	}))
	// bad consumes gen's product and fails; downstream depends on bad.
	if err := e.AddAction(engine.Action{ID: "bad", Tool: "fail",
		Inputs: []string{"gen/a.txt", "bad.txt"}, Outputs: []string{"out/bad.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAction(engine.Action{ID: "after", Tool: "copy",
		Inputs: []string{"out/bad.txt"}, Outputs: []string{"out/after.txt"}}); err != nil {
		t.Fatal(err)
	}
	rep := e.BuildReport(context.Background(), "after")
	if rep.Err == nil {
		t.Fatal("expected failure")
	}
	bad := rep.Get("bad")
	if bad.Status != engine.StatusFailed || !bad.Started {
		t.Fatalf("bad should be failed+started, got %q started=%v", bad.Status, bad.Started)
	}
	if bad.Tool != "fail" || bad.Fingerprint == "" {
		t.Fatalf("failed action lost tool identity or fingerprint: %+v", bad)
	}
	// The inputs the failed attempt actually read, incl. gen's product.
	if bad.Inputs["bad.txt"] == "" || bad.Inputs["gen/a.txt"] == "" {
		t.Fatalf("failed action lost its start-time inputs: %+v", bad.Inputs)
	}
	if len(bad.DependsOn) != 1 || bad.DependsOn[0] != "gen" || bad.InputFrom["gen/a.txt"] != "gen" {
		t.Fatalf("failed action lost dependency provenance: %+v", bad)
	}
	// after never began: skipped, not started, but provenance recorded.
	after := rep.Get("after")
	if after.Status != engine.StatusSkipped || after.Started {
		t.Fatalf("after should be skipped+not started, got %q started=%v", after.Status, after.Started)
	}
	if after.Fingerprint != "" || len(after.Inputs) != 0 {
		t.Fatalf("unstarted action must not invent a fingerprint: %+v", after)
	}
	if len(after.DependsOn) != 1 || after.DependsOn[0] != "bad" {
		t.Fatalf("skipped action lost dependency provenance: %+v", after)
	}

	// The saved view keeps the failed attempt's evidence across reopen,
	// even after the inputs change on disk afterwards.
	if _, err := e.SaveView("failed", rep); err != nil {
		t.Fatal(err)
	}
	write(t, root, "bad.txt", "edited afterwards")
	e2, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	v, err := e2.View("failed")
	if err != nil {
		t.Fatal(err)
	}
	got := v.Actions[0]
	for _, a := range v.Actions {
		if a.ID == "bad" {
			got = a
		}
	}
	if got.Inputs["bad.txt"] != bad.Inputs["bad.txt"] || got.Fingerprint != bad.Fingerprint {
		t.Fatalf("reopened view was rewritten from current files: %+v", got)
	}
}

func TestDriftedActionKeepsPreRunInputs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	e.RegisterTool("mutate", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		if err := os.WriteFile(inputs[0], []byte("sneaky"), 0o644); err != nil {
			return err
		}
		return tools.Copy(params, inputs, outputs)
	}))
	if err := e.AddAction(engine.Action{ID: "gen", Tool: "mutate",
		Inputs: []string{"src.txt"}, Outputs: []string{"out/a.txt"}}); err != nil {
		t.Fatal(err)
	}
	rep := e.BuildReport(context.Background(), "gen")
	gen := rep.Get("gen")
	if gen.Status != engine.StatusFailed || !gen.Started {
		t.Fatalf("drifted action should be failed+started, got %q", gen.Status)
	}
	if !strings.Contains(gen.Reason, "changed during build") {
		t.Fatalf("expected drift reason, got %q", gen.Reason)
	}
	// The recorded input is the version read at start ("hello"), not the
	// mutated content now on disk.
	if gen.Inputs["src.txt"] == "" || gen.Fingerprint == "" {
		t.Fatalf("drifted action lost start-time fingerprint: %+v", gen)
	}
	if _, err := e.SaveView("drift", rep); err != nil {
		t.Fatal(err)
	}
	e2, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	v, err := e2.View("drift")
	if err != nil {
		t.Fatal(err)
	}
	if v.Actions[0].Inputs["src.txt"] != gen.Inputs["src.txt"] {
		t.Fatal("reopened view must keep the pre-run input hash")
	}
	// VerifyView reports the drift against the recorded pre-run version.
	drift, err := e2.VerifyView("drift")
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) == 0 || !strings.Contains(drift[0], "src.txt") {
		t.Fatalf("expected drift against recorded input, got %v", drift)
	}
}

func TestCancelledActionIsMarkedNotStarted(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := e.BuildReport(ctx, "merge")
	if !rep.Cancelled {
		t.Fatal("expected cancellation")
	}
	for _, a := range rep.Actions {
		if a.Status != engine.StatusCancelled {
			t.Fatalf("action %q status %q", a.ID, a.Status)
		}
		if a.Started || a.Fingerprint != "" {
			t.Fatalf("cancelled action %q must be marked not started: %+v", a.ID, a)
		}
		if a.Tool == "" {
			t.Fatalf("cancelled action %q lost tool identity", a.ID)
		}
	}
	merge := rep.Get("merge")
	if len(merge.DependsOn) != 2 {
		t.Fatalf("cancelled action lost dependency provenance: %+v", merge)
	}
}

func TestImpactComparisonAttributesSharedDependencyFailureAndReopen(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	write(t, root, "bad.txt", "boom")
	e := diamond(t, root)
	e.RegisterTool("fail", engine.ToolFunc(func(map[string]string, []string, []string) error {
		return errors.New("always fails")
	}))
	if err := e.AddAction(engine.Action{ID: "bad", Tool: "fail",
		Inputs: []string{"gen/a.txt", "bad.txt"}, Outputs: []string{"out/bad.txt"}}); err != nil {
		t.Fatal(err)
	}

	v1Rep := e.BuildReport(context.Background(), "merge", "bad")
	if v1Rep.Err == nil {
		t.Fatal("expected bad to fail")
	}
	v1, err := e.SaveView("v1", v1Rep)
	if err != nil {
		t.Fatal(err)
	}

	write(t, root, "src.txt", "world")
	v2Rep := e.BuildReport(context.Background(), "merge", "bad")
	if v2Rep.Err == nil {
		t.Fatal("expected bad to remain failed")
	}
	v2, err := e.SaveView("v2", v2Rep)
	if err != nil {
		t.Fatal(err)
	}
	_ = v2

	// Later files must not be used to explain either old build.
	write(t, root, "src.txt", "changed after both views")
	reopened, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	v1Loaded, err := reopened.View("v1")
	if err != nil {
		t.Fatal(err)
	}
	v2Loaded, err := reopened.View("v2")
	if err != nil {
		t.Fatal(err)
	}
	impact := engine.CompareViewImpacts(v1Loaded, v2Loaded)
	if impact == nil || impact.A != "v1" || impact.B != "v2" {
		t.Fatalf("unexpected comparison: %+v", impact)
	}

	merge := impact.Target("merge")
	if merge == nil || merge.StatusA != engine.StatusRan || merge.StatusB != engine.StatusRan {
		t.Fatalf("unexpected successful target: %+v", merge)
	}
	if len(merge.ReusableActions) != 0 {
		t.Fatalf("source change invalidates the diamond: %v", merge.ReusableActions)
	}
	if fact, ok := findDirectInput(merge, "src.txt"); !ok || fact.HashA == fact.HashB {
		t.Fatalf("missing changed direct input: %+v", merge.DirectInputs)
	}
	if dep, ok := findSharedDependency(merge, "gen", "gen/a.txt"); !ok || dep.Change.HashA == dep.Change.HashB {
		t.Fatalf("missing changed shared dependency gen/a.txt: %+v", merge.SharedDependencies)
	}
	if _, ok := findSharedDependency(merge, "left", "mid/b.txt"); !ok {
		t.Fatalf("missing downstream shared dependency: %+v", merge.SharedDependencies)
	}

	bad := impact.Target("bad")
	if bad == nil || bad.StatusA != engine.StatusFailed || bad.StatusB != engine.StatusFailed {
		t.Fatalf("unexpected failing target: %+v", bad)
	}
	var failedEvidence []engine.InconclusiveAction
	for _, action := range bad.Actions {
		if action.ID == "bad" {
			failedEvidence = action.Inconclusive
		}
	}
	if len(failedEvidence) != 2 ||
		failedEvidence[0].Side != "a" || failedEvidence[0].Status != engine.StatusFailed || !failedEvidence[0].Started ||
		failedEvidence[1].Side != "b" || failedEvidence[1].Status != engine.StatusFailed || !failedEvidence[1].Started {
		t.Fatalf("failed attempts lost start/stop evidence: %+v", failedEvidence)
	}
	if _, ok := findSharedDependency(bad, "gen", "gen/a.txt"); !ok {
		t.Fatalf("failed target must still attribute changed shared dependency: %+v", bad.SharedDependencies)
	}
	if got := v1Loaded.Actions[0].Inputs["src.txt"]; got != v1.Actions[0].Inputs["src.txt"] {
		t.Fatal("old view changed after reopen")
	}
}

func TestImpactComparisonDetectsToolIdentityChange(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e1 := diamond(t, root)
	v1, err := e1.SaveView("copy-tool", reportOf(t, e1, "gen"))
	if err != nil {
		t.Fatal(err)
	}

	e2, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	e2.RegisterTool("same-copy", engine.ToolFunc(tools.Copy))
	if err := e2.AddAction(engine.Action{ID: "gen", Tool: "same-copy",
		Inputs: []string{"src.txt"}, Outputs: []string{"gen/a.txt"}}); err != nil {
		t.Fatal(err)
	}
	v2, err := e2.SaveView("identity-tool", reportOf(t, e2, "gen"))
	if err != nil {
		t.Fatal(err)
	}

	impact := engine.CompareViewImpacts(v1, v2)
	gen := impact.Target("gen")
	if gen == nil || len(gen.ToolChanges) != 1 {
		t.Fatalf("expected one identity change, got %+v", gen)
	}
	change := gen.ToolChanges[0]
	if !change.ToolChanged || change.ToolA != "copy" || change.ToolB != "same-copy" {
		t.Fatalf("wrong tool identity change: %+v", change)
	}
	if len(gen.DirectInputs) != 0 || len(gen.SharedDependencies) != 0 {
		t.Fatalf("tool change should not be mislabeled as input change: %+v", gen)
	}
	if len(gen.ReusableActions) != 0 {
		t.Fatalf("different tool identity cannot be reusable: %+v", gen)
	}
}

func TestPlanFromImpactsPreviewsSharedWorkAndRefreshesImpact(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	v1, err := e.SaveView("v1", reportOf(t, e, "left", "right"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "src.txt", "world")
	v2, err := e.SaveView("v2", reportOf(t, e, "left", "right"))
	if err != nil {
		t.Fatal(err)
	}
	impact := engine.CompareViewImpacts(v1, v2)

	// Current state is v2, so the full two-target preview first shows gen as
	// a shared dependency and every action reusable.
	plan, err := e.NewBuildPlan(impact, "left", "right")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.SharedDependencies, []string{"gen"}) {
		t.Fatalf("shared dependency = %v", plan.SharedDependencies)
	}
	if len(plan.ActionsToRun) != 0 || len(plan.ReusableActions) != 3 {
		t.Fatalf("unexpected initial plan: run=%v reusable=%v", plan.ActionsToRun, plan.ReusableActions)
	}

	write(t, root, "src.txt", "changed while confirming")
	refresh, err := e.RefreshPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ActionsToRun) != 0 {
		t.Fatalf("old plan was mutated during refresh: %v", plan.ActionsToRun)
	}
	if len(refresh.ChangedInputs) != 1 || refresh.ChangedInputs[0] != "src.txt" {
		t.Fatalf("changed inputs = %v", refresh.ChangedInputs)
	}
	if !reflect.DeepEqual(refresh.NewlyRequired, []string{"gen", "left", "right"}) {
		t.Fatalf("newly required = %v", refresh.NewlyRequired)
	}
	updated := refresh.UpdatedPlan
	if !reflect.DeepEqual(updated.SharedDependencies, []string{"gen"}) ||
		!reflect.DeepEqual(updated.PossibleDuplicateWork, []string{"gen"}) {
		t.Fatalf("updated shared/duplicate work: shared=%v duplicate=%v",
			updated.SharedDependencies, updated.PossibleDuplicateWork)
	}
	if !reflect.DeepEqual(updated.ActionsToRun, []string{"gen", "left", "right"}) {
		t.Fatalf("updated actions to run = %v", updated.ActionsToRun)
	}
}

func findDirectInput(target *engine.TargetImpact, path string) (engine.FactChange, bool) {
	for _, fact := range target.DirectInputs {
		if fact.Path == path {
			return fact, true
		}
	}
	return engine.FactChange{}, false
}

func findSharedDependency(target *engine.TargetImpact, action, input string) (engine.DependencyChange, bool) {
	for _, dep := range target.SharedDependencies {
		if dep.Action == action && dep.Input == input {
			return dep, true
		}
	}
	return engine.DependencyChange{}, false
}
