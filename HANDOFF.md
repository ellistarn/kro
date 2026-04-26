# Lazy Dependencies — Handoff

## What this branch does

Replaces the `ReadinessDeps`/`ReadinessDependents` mechanism with a unified
dependency model. Each edge is `DepHard` or `DepLazy`. `.ready()` targets in
branch expressions create lazy deps. Lazy deps differ from hard in two walk
behaviors: dispatch ordering (no gate) and contagious exclusion (no propagation).
Everything else — propagation triggering, cycle detection, hash machinery — is
identical for both kinds.

## Current state

Commit `19344c1`. Unit tests pass. Compat: 34/39 pass (5 failures). E2e: 2
collection-ready tests fail. The failures are all the same root cause: the
field path walker doesn't extract `["__ready"]` from `.ready()` calls yet.

## What's left

### The field path TODO (`fieldpath.go:45`)

The design (001-graph.md § Dependencies) says `.ready()` reads `__ready` from
scope data and should produce `["__ready"]` as a dependency path. The AST
walker currently skips `.ready()` calls. Changing this is ~10 lines in the
walker, but it has two interactions that need careful handling:

1. **`processExpr` dep classification.** `processExpr` marks every dep with
   field paths as `DepHard`. When the walker produces `["__ready"]` for a
   `.ready()` target, `processExpr` would override the lazy classification
   from `checkReadyRef`. Fix: `processExpr` should not set `DepHard` for
   `__ready`-only paths — let `checkReadyRef` handle classification.

2. **`hashNodeInputs` scope lookup.** The hash iterates `DepPaths` and looks
   up each dep in the coordinator scope. A lazy dep that hasn't completed is
   absent from scope. `hashNodeInputs` returns an error → hash check skipped →
   full eval. This is actually correct (forces eval when dep is absent), but
   verify it doesn't cause hash-miss churn on subsequent reconciles where the
   dep IS in scope and the hash stabilizes.

Once `["__ready"]` flows through DepPaths:
- Input-hash naturally includes readiness from lazy deps
- SelfPaths get `["__ready"]` via the push-down in BuildDAG
- Output-hash naturally includes readiness (the `:ready=true/false` suffix
  becomes redundant — remove it)
- The `staleLazyDeps` mechanism (trigger deposit + eval hash invalidation)
  becomes redundant for hash correctness but should be kept for the intra-walk
  timing case (requeue floor)

### Bridge mechanisms to remove after field path fix

- `staleLazyDeps` eval hash invalidation (`walk.go` post-walk cleanup) — the
  trigger deposit stays (requeue floor), but `delete(previousEvalHashes)` goes
- `LenientScope` function in `compiler/cel.go` — dead code, never called

### Tests to verify

Run `make presubmit` from `experimental/`. The 5 compat failures and 2 e2e
failures should resolve once `["__ready"]` is in DepPaths. The specific tests:

- `dependency_readiness_test.go` (2 tests)
- `format_test.go`
- `unknown_fields_test.go` (CRD with preserve unknown fields)
- `instance_conflict_test.go`
- `TestCollectionItemReadyViaIndex`
- `TestCollectionReadyFalseWhenItemNotReady`

All are instances stuck at IN_PROGRESS — `rgdInstanceStatus` evaluates
`.ready()` as false and the eval hash skip prevents re-evaluation when
readiness changes.

## Key files

| File | What changed |
|---|---|
| `graph/types.go` | `DepKind` enum, `Dependencies map[string]DepKind` |
| `graph/expr.go` | `checkReadyRef` creates `DepLazy`, `processExpr` creates `DepHard` |
| `compiler/fieldpath.go` | TODO: extract `["__ready"]` for `.ready()` calls |
| `dag/dag.go` | Both dep kinds in Dependents, only hard in topo sort |
| `walk.go` | Gate re-dispatch, stale lazy dep triggers, dispatch gating |
| `eval.go` | `snapshotFor` includes lazy dep data with empty-map fallback |
| `compiler/cel.go` | `LenientScope` function (dead code, remove) |
| `001-graph.md` | Lazy dep design, behavior table |
| `005-reconciliation.md` | Scope, propagation, frontier steps |
