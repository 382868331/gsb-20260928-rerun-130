package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildengine/engine"
	"buildengine/tools"
)

// testHash mirrors the engine's content hash for fixed test inputs.
func testHash(content string) string {
	h := sha256.New()
	h.Write([]byte(content))
	h.Write([]byte{0})
	return hex.EncodeToString(h.Sum(nil))
}

// A failed-but-started action keeps the input version it actually read,
// its tool identity and its dependency sources; a never-started action
// (cancelled or blocked) keeps none of that and is clearly marked so.
func TestFailedStartedActionKeepsInputVersion(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	e.RegisterTool("fail", engine.ToolFunc(func(map[string]string, []string, []string) error {
		return errors.New("always fails")
	}))
	if err := e.AddAction(engine.Action{
		ID: "bad", Tool: "fail",
		Inputs:  []string{"src.txt"},
		Outputs: []string{"out/bad.txt"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAction(engine.Action{
		ID: "after", Tool: "copy",
		Inputs:  []string{"out/bad.txt"},
		Outputs: []string{"out/after.txt"},
	}); err != nil {
		t.Fatal(err)
	}

	rep := e.BuildReport(context.Background(), "bad", "after")
	if rep.Err == nil {
		t.Fatal("expected a failed build")
	}
	bad := rep.Get("bad")
	if bad.Status != engine.StatusFailed || !bad.Started {
		t.Fatalf("bad = %q started=%v, want failed+started", bad.Status, bad.Started)
	}
	if bad.Tool != "fail" {
		t.Fatalf("tool identity lost: %q", bad.Tool)
	}
	wantHash := testHash("hello")
	if got := bad.Inputs["src.txt"]; got != wantHash {
		t.Fatalf("failed action recorded input %q, want %q", got, wantHash)
	}
	if bad.Fingerprint == "" {
		t.Fatal("failed action lost its execution-start fingerprint")
	}

	// The blocked downstream action never started: no input version was
	// chosen, so it must not carry a fingerprint or an input hash.
	after := rep.Get("after")
	if after.Status != engine.StatusSkipped || after.Started {
		t.Fatalf("after = %q started=%v, want skipped+not-started", after.Status, after.Started)
	}
	if after.Fingerprint != "" || len(after.Inputs) != 0 {
		t.Fatalf("never-started action must not record a chosen input version: %+v", after)
	}
	if len(after.DependsOn) != 1 || after.DependsOn[0] != "bad" {
		t.Fatalf("dependency source not recorded: %v", after.DependsOn)
	}
	if after.Tool != "copy" {
		t.Fatalf("tool identity of skipped action lost: %q", after.Tool)
	}

	// Change the source after the failed attempt: the saved evidence still
	// points at the version the tool actually read, not the current file.
	v, err := e.SaveView("evidence", rep)
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "src.txt", "totally different now")
	reopened, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.View("evidence")
	if err != nil {
		t.Fatal(err)
	}
	savedBad := got.Get("bad")
	if savedBad.Inputs["src.txt"] != wantHash {
		t.Fatalf("saved view backfilled from current files: got %q, want %q",
			savedBad.Inputs["src.txt"], wantHash)
	}
	if savedBad.Fingerprint != bad.Fingerprint {
		t.Fatal("fingerprint not preserved across save/reopen")
	}
	lines := savedBad.Explain()
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, wantHash) || !strings.Contains(joined, "fail") {
		t.Fatalf("explanation lost input version or tool: %v", lines)
	}
	if !strings.Contains(joined, "started") {
		t.Fatalf("explanation should state the action started: %v", lines)
	}
	fromEngine, err := reopened.ExplainView("evidence", "bad")
	if err != nil || strings.Join(fromEngine, "\n") != joined {
		t.Fatalf("ExplainView mismatch: %v / %v", fromEngine, err)
	}
	if v.Get("nope") != nil {
		t.Fatal("View.Get should miss unknown actions")
	}
}

// Input drift is its own status: the action started, the pre-drift input
// version is preserved, and the view remains explainable later.
func TestDriftedActionKeepsPreDriftInputs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "before")
	e, err := engine.New(root)
	if err != nil {
		t.Fatal(err)
	}
	e.RegisterTool("mutate", engine.ToolFunc(func(params map[string]string, inputs, outputs []string) error {
		if err := os.WriteFile(inputs[0], []byte("after"), 0o644); err != nil {
			return err
		}
		return tools.Copy(params, inputs, outputs)
	}))
	if err := e.AddAction(engine.Action{
		ID: "gen", Tool: "mutate",
		Inputs:  []string{"src.txt"},
		Outputs: []string{"out/a.txt"},
	}); err != nil {
		t.Fatal(err)
	}
	rep := e.BuildReport(context.Background(), "gen")
	gen := rep.Get("gen")
	if gen.Status != engine.StatusDrifted || !gen.Started {
		t.Fatalf("gen = %q started=%v, want drifted+started", gen.Status, gen.Started)
	}
	wantHash := testHash("before")
	if gen.Inputs["src.txt"] != wantHash {
		t.Fatalf("drift recorded %q, want pre-drift hash %q", gen.Inputs["src.txt"], wantHash)
	}
	if gen.Fingerprint == "" {
		t.Fatal("drifted action lost its fingerprint")
	}
	if _, err := os.Stat(filepath.Join(root, "out/a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("drifted products must not be published")
	}

	// Save after drift and overwrite the source again: the view keeps the
	// version the drifted attempt started from.
	if _, err := e.SaveView("drift", rep); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src.txt", "third version")
	reopened, _ := engine.New(root)
	v, err := reopened.View("drift")
	if err != nil {
		t.Fatal(err)
	}
	if v.Get("gen").Inputs["src.txt"] != wantHash {
		t.Fatalf("drift view was rewritten from current files: %+v", v.Get("gen").Inputs)
	}
	joined := strings.Join(v.Get("gen").Explain(), "\n")
	if !strings.Contains(joined, "changed during execution") {
		t.Fatalf("drift explanation missing: %v", joined)
	}
}

// Cancelled actions are unambiguously "not started" and carry no input
// fingerprint, while actions already completed in the same report do.
func TestCancelledActionsHaveNoChosenInputVersion(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	if err := e.Build("gen"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src.txt", "changed") // force gen to rerun

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := e.BuildReport(ctx, "merge")
	if !rep.Cancelled {
		t.Fatal("expected cancellation")
	}
	for _, id := range []string{"gen", "left", "right", "merge"} {
		a := rep.Get(id)
		if a.Status != engine.StatusCancelled {
			t.Fatalf("%s = %q, want cancelled", id, a.Status)
		}
		if a.Started {
			t.Fatalf("cancelled %s must be marked not started", id)
		}
		if a.Fingerprint != "" || len(a.Inputs) != 0 {
			t.Fatalf("cancelled %s must not carry a chosen input version: %+v", id, a)
		}
		if a.Tool == "" {
			t.Fatalf("cancelled %s lost tool identity", id)
		}
	}
	if deps := rep.Get("merge").DependsOn; len(deps) != 2 {
		t.Fatalf("merge dependency sources lost: %v", deps)
	}

	// The cancelled view stays explainable after reopen and further edits.
	if _, err := e.SaveView("aborted", rep); err != nil {
		t.Fatal(err)
	}
	write(t, root, "src.txt", "again and again")
	reopened, _ := engine.New(root)
	v, err := reopened.View("aborted")
	if err != nil {
		t.Fatal(err)
	}
	gen := v.Get("gen")
	if gen.Started || gen.Fingerprint != "" || len(gen.Inputs) != 0 {
		t.Fatalf("reopened cancelled record gained provenance: %+v", gen)
	}
	if !strings.Contains(strings.Join(gen.Explain(), " "), "never started") {
		t.Fatalf("cancelled record must explain it never started: %v", gen.Explain())
	}
}

// Successful and reused attempts record tool, dependency sources and the
// consumed input version too, and reuse is distinguishable from a run.
func TestRanAndReusedRecordsAreAttributable(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src.txt", "hello")
	e := diamond(t, root)
	rep := e.BuildReport(context.Background(), "merge")
	merge := rep.Get("merge")
	if merge.Status != engine.StatusRan || !merge.Started {
		t.Fatalf("merge = %q started=%v", merge.Status, merge.Started)
	}
	if merge.Tool != "concat" || merge.Fingerprint == "" {
		t.Fatalf("ran record lost tool/fingerprint: %+v", merge)
	}
	if len(merge.DependsOn) != 2 {
		t.Fatalf("ran record lost dependency sources: %v", merge.DependsOn)
	}
	rep2 := e.BuildReport(context.Background(), "merge")
	reused := rep2.Get("merge")
	if reused.Status != engine.StatusReused || !reused.Started {
		t.Fatalf("merge = %q started=%v, want reused+started", reused.Status, reused.Started)
	}
	if reused.Fingerprint != merge.Fingerprint {
		t.Fatalf("reused fingerprint %q != ran %q", reused.Fingerprint, merge.Fingerprint)
	}
}
