package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"buildengine/engine"
	"buildengine/tools"
)

func saveView(t *testing.T, e *engine.Engine, name string, rep *engine.BuildReport) *engine.View {
	t.Helper()
	v, err := e.SaveView(name, rep)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// diamondWithHeader is the diamond graph with a configurable header
// parameter on the "right" action, so tests can change a tool's
// identity between builds.
func diamondWithHeader(t *testing.T, root, header string) *engine.Engine {
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
		{ID: "right", Tool: "copy", Params: map[string]string{"header": header}, Inputs: []string{"gen/a.txt"}, Outputs: []string{"mid/c.txt"}},
		{ID: "merge", Tool: "concat", Inputs: []string{"mid/b.txt", "mid/c.txt"}, Outputs: []string{"out/d.txt"}},
	}
	for _, a := range actions {
		if err := e.AddAction(a); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func remove(t *testing.T, root, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
}

// A shared dependency change is attributed as such, a direct source edit
// as a direct-input cause, a parameter change as tool identity, and
// untouched actions stay reusable.
func TestImpactClassifiesCauses(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	write(t, root, "extra.txt", "one")
	e := diamondWithHeader(t, root, "R:")
	// An independent action with its own source input.
	if err := e.AddAction(engine.Action{ID: "side", Tool: "copy",
		Inputs: []string{"extra.txt"}, Outputs: []string{"out/side.txt"}}); err != nil {
		t.Fatal(err)
	}
	v1 := saveView(t, e, "v1", reportOf(t, e, "merge", "side"))

	// Change the shared source, the side action's own source, and one
	// tool's parameters (tool identity).
	write(t, root, "src.txt", "world")
	write(t, root, "extra.txt", "two")
	// Re-register actions with a changed parameter for "right".
	e = diamondWithHeader(t, root, "R2:")
	if err := e.AddAction(engine.Action{ID: "side", Tool: "copy",
		Inputs: []string{"extra.txt"}, Outputs: []string{"out/side.txt"}}); err != nil {
		t.Fatal(err)
	}
	v2 := saveView(t, e, "v2", reportOf(t, e, "merge", "side"))

	imp := engine.CompareViewImpact(v1, v2)

	gen := imp.Get("gen")
	if gen == nil || !reflect.DeepEqual(gen.Causes, []string{engine.CauseDirectInput}) ||
		!reflect.DeepEqual(gen.DirectInputs, []string{"src.txt"}) {
		t.Fatalf("gen impact: %+v", gen)
	}
	left := imp.Get("left")
	if left == nil || !reflect.DeepEqual(left.Causes, []string{engine.CauseSharedDependency}) ||
		!reflect.DeepEqual(left.SharedInputs, []string{"gen/a.txt (from gen)"}) {
		t.Fatalf("left impact: %+v", left)
	}
	right := imp.Get("right")
	if right == nil || !contains(right.Causes, engine.CauseSharedDependency) ||
		!contains(right.Causes, engine.CauseToolIdentity) ||
		!reflect.DeepEqual(right.ParamChanges, []string{`header: "R:" -> "R2:"`}) {
		t.Fatalf("right impact: %+v", right)
	}
	side := imp.Get("side")
	if side == nil || !reflect.DeepEqual(side.Causes, []string{engine.CauseDirectInput}) ||
		!reflect.DeepEqual(side.DirectInputs, []string{"extra.txt"}) {
		t.Fatalf("side impact: %+v", side)
	}
	for _, a := range imp.Actions {
		if a.Reusable {
			t.Fatalf("action %s unexpectedly reusable: %+v", a.ID, a)
		}
	}

	// A view compared with itself: everything reusable, no causes.
	self := engine.CompareViewImpact(v1, v1)
	for _, a := range self.Actions {
		if !a.Reusable || len(a.Causes) != 0 || a.NoConclusion {
			t.Fatalf("self comparison should be fully reusable: %+v", a)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// One target fails while another succeeds; the comparison shows the
// failure with its recorded start facts, the success as reusable, and
// cancelled/skipped actions as having no conclusion.
func TestImpactFailureSuccessAndNoConclusion(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	write(t, root, "bad.txt", "boom")
	e := diamond(t, root)
	e.RegisterTool("fail", engine.ToolFunc(func(map[string]string, []string, []string) error {
		return errors.New("always fails")
	}))
	if err := e.AddAction(engine.Action{ID: "bad", Tool: "fail",
		Inputs: []string{"bad.txt"}, Outputs: []string{"out/bad.txt"}}); err != nil {
		t.Fatal(err)
	}
	v1 := saveView(t, e, "ok", reportOf(t, e, "merge"))

	rep := e.BuildReport(context.Background(), "merge", "bad")
	if rep.Err == nil {
		t.Fatal("expected failure")
	}
	if rep.Get("bad").Status != engine.StatusFailed || rep.Get("merge").Status != engine.StatusReused {
		t.Fatalf("unexpected statuses: bad=%s merge=%s",
			rep.Get("bad").Status, rep.Get("merge").Status)
	}
	v2 := saveView(t, e, "with-failure", rep)

	imp := engine.CompareViewImpact(v1, v2)
	bad := imp.Get("bad")
	if bad == nil || bad.OnlyIn != "b" || bad.NoConclusion {
		// bad started and its input facts were recorded: it has a
		// conclusion (a failure), not a missing one.
		t.Fatalf("bad impact: %+v", bad)
	}
	if m := imp.Get("merge"); m == nil || !m.Reusable {
		t.Fatalf("merge should be reusable across the views: %+v", m)
	}

	// Cancelled build: actions never started have no conclusion.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	write(t, root, "src.txt", "changed")
	repC := e.BuildReport(ctx, "merge")
	if !repC.Cancelled {
		t.Fatal("expected cancellation")
	}
	v3 := saveView(t, e, "cancelled", repC)
	impC := engine.CompareViewImpact(v1, v3)
	genC := impC.Get("gen")
	if genC == nil || !genC.NoConclusion {
		t.Fatalf("cancelled gen must have no conclusion: %+v", genC)
	}
	if len(genC.Causes) != 0 {
		// No cause is invented for an action that never started.
		t.Fatalf("cancelled gen must not gain causes: %+v", genC)
	}
}

// Comparison works after saving and reopening, never reads the current
// workspace, and saved views stay byte-identical no matter what changes
// afterwards.
func TestImpactAcrossReopenAndViewsUnchanged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	saveView(t, e, "r1", reportOf(t, e, "merge"))
	before := read(t, root, ".views/r1.json")

	write(t, root, "src.txt", "world")
	saveView(t, e, "r2", reportOf(t, e, "merge"))

	// Reopen the workspace: views load from disk and compare the same.
	e = diamond(t, root)
	r1, err := e.View("r1")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.View("r2")
	if err != nil {
		t.Fatal(err)
	}
	imp := engine.CompareViewImpact(r1, r2)
	if got := imp.Get("gen").DirectInputs; !reflect.DeepEqual(got, []string{"src.txt"}) {
		t.Fatalf("gen direct inputs after reopen: %v", got)
	}
	// Continuous comparison: a third change builds on the reopened views.
	write(t, root, "src.txt", "again")
	v3 := saveView(t, e, "r3", reportOf(t, e, "merge"))
	imp23 := engine.CompareViewImpact(r2, v3)
	if got := imp23.Get("merge").Causes; !reflect.DeepEqual(got, []string{engine.CauseSharedDependency}) {
		t.Fatalf("merge causes r2->r3: %v", got)
	}

	// The comparison is a pure function of the views: deleting every
	// source and product changes nothing about it.
	for _, f := range []string{"src.txt", "gen/a.txt", "mid/b.txt", "mid/c.txt", "out/d.txt"} {
		remove(t, root, f)
	}
	imp2 := engine.CompareViewImpact(r1, r2)
	if !reflect.DeepEqual(imp, imp2) {
		t.Fatal("comparison must not depend on the current workspace")
	}

	// Old views are untouched by everything that happened since.
	if got := read(t, root, ".views/r1.json"); got != before {
		t.Fatal("saved view r1 changed on disk")
	}
	again, err := e.View("r1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(again, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != before {
		t.Fatal("view r1 no longer returns the originally saved record")
	}
}

// A subset of targets forms a plan preview showing shared dependencies
// and avoided duplicate work; an input change before confirmation
// updates the affected scope precisely.
func TestPlanPreviewAndDrift(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	write(t, root, "extra.txt", "one")
	e := diamond(t, root)
	if err := e.AddAction(engine.Action{ID: "side", Tool: "copy",
		Inputs: []string{"extra.txt"}, Outputs: []string{"out/side.txt"}}); err != nil {
		t.Fatal(err)
	}
	reportOf(t, e, "merge", "side")

	// Selecting both targets: gen/left/right are shared prerequisites
	// of merge only, side stands alone; merge's closure is shared work
	// done once.
	p, err := e.PreviewPlan("merge", "side")
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"gen", "left", "right", "merge", "side"}
	if !reflect.DeepEqual(p.Order, wantOrder) {
		t.Fatalf("order %v, want %v", p.Order, wantOrder)
	}
	if len(p.Shared) != 0 {
		t.Fatalf("no shared deps between merge and side: %v", p.Shared)
	}
	if got := p.NeededBy["gen"]; !reflect.DeepEqual(got, []string{"merge"}) {
		t.Fatalf("gen needed by %v", got)
	}

	// Selecting left and right: gen is the shared dependency whose
	// duplicate work a combined plan avoids.
	p2, err := e.PreviewPlan("left", "right")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p2.Shared, []string{"gen"}) {
		t.Fatalf("shared %v, want [gen]", p2.Shared)
	}
	if got := p2.NeededBy["gen"]; !reflect.DeepEqual(got, []string{"left", "right"}) {
		t.Fatalf("gen needed by %v", got)
	}

	// No change yet: the plan is still accurate.
	drift, err := e.PlanDrift(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 0 {
		t.Fatalf("unexpected drift %v", drift)
	}

	// The shared source changes before confirmation: the whole merge
	// chain is affected, the independent side action is not.
	write(t, root, "src.txt", "world")
	drift, err = e.PlanDrift(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(drift, []string{"gen", "left", "right", "merge"}) {
		t.Fatalf("affected scope %v", drift)
	}

	// A change to the side action's own input affects only side.
	p3, err := e.PreviewPlan("merge", "side")
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "extra.txt", "two")
	drift, err = e.PlanDrift(p3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(drift, []string{"side"}) {
		t.Fatalf("affected scope %v, want [side]", drift)
	}
}
