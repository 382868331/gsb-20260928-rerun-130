# Go Dependency Execution Engine

`depengine` builds outputs from an explicit graph of tasks, source inputs,
dependency edges, and owned output paths. It has no shell integration and does
not inspect timestamps.

## Model

- A `Task` names one registered Go action, source `Inputs`, task `Deps`, and
  all files the task is allowed to publish.
- A task fingerprint covers the action name, canonical parameters, source input
  contents, dependency fingerprints, and declared output paths.
- Current outputs are additionally hashed on every build, so externally changed
  or corrupted artifacts force their producing task to run.
- Dependencies are topologically ordered. A shared dependency is evaluated once
  per build even when reached through a diamond.
- Actions receive source and dependency file contents as bytes and return all
  outputs as bytes. They cannot publish directly into the repository.
- All returned files are staged first, then renamed into place together. If
  publication fails, previous outputs are restored and new partial outputs are
  removed.
- Persistent state is written atomically to `.depengine/state.json` after each
  successful task. Restarting the process uses the same content-based decision.

Only actions supplied in the `map[string]GenerateFunc` registry can run.
Unknown actions are rejected, and the engine contains no command execution
path, so adding more generators keeps using the same dependency model without
triggering repository auto-builds.

## Example

```go
engine := depengine.New(repoRoot, map[string]depengine.GenerateFunc{
    "render": render,
})

tasks := []*depengine.Task{
    {
        ID:      "page",
        Action:  "render",
        Params:  map[string]any{"template": "page.html"},
        Inputs:  []string{"templates/page.html", "data/page.json"},
        Outputs: []string{"dist/page.html"},
    },
}

result, err := engine.Build(context.Background(), tasks)
```

## Verification

Run the controlled Go test suite:

```sh
go test ./...
go vet ./...
```

The tests cover diamond sharing, unchanged-task skipping, source and parameter
invalidations, external output corruption, restart recovery, failed multi-output
atomicity, unsafe paths, and unregistered actions.
