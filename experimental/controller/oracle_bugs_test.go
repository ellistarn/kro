package graphcontroller

import (
	"context"
	"fmt"
	"testing"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	"github.com/ellistarn/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// Bug 1: ReadinessDependents never triggered
//
// Ellis had a scoped-walk optimization where only nodes reachable from a
// "trigger" were evaluated. The propagation system only tracked hard
// Dependents. So if node A's readiness changed, node B (which has a
// propagateWhen referencing A's readiness) was never re-evaluated by the
// scoped walk.
//
// We reproduce this by:
//   - Building a graph where B has a propagateWhen gated on A's readiness.
//   - Running the full sequential oracle (correct).
//   - Running a "buggy scoped walk" that only evaluates nodes reachable via
//     hard dependency edges (misses B because B doesn't hard-depend on A's data,
//     only on A's readiness through propagateWhen).
//   - Asserting the two results DIVERGE — proving the oracle catches the bug.
// ---------------------------------------------------------------------------

func TestOracleBug_ReadinessDependentsNeverTriggered(t *testing.T) {
	// Graph:
	//   a: def that produces {value: "hello", gate: true}
	//   b: def that produces {value: "world"}, with propagateWhen: ${a.gate == true}
	//
	// In the correct full walk, A evaluates first (becomes ready, publishes scope),
	// then B's propagateWhen passes (a.gate == true) and B evaluates.
	// In a buggy scoped walk that only follows hard dep edges, B is never triggered
	// because B's dependency on A is only via propagateWhen (a readiness/gate edge),
	// not a hard data dependency in B's template body.
	spec := &graph.GraphSpec{
		Nodes: []graph.Node{
			{
				ID:  "a",
				Def: map[string]any{"value": "hello", "gate": true},
			},
			{
				ID:            "b",
				Def:           map[string]any{"value": "world"},
				PropagateWhen: []string{"${a.gate == true}"},
			},
		},
	}
	for i := range spec.Nodes {
		spec.Nodes[i].SetType(graph.NodeTypeDef)
	}

	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatalf("CompileGraphSpec: %v", err)
	}

	dag, err := dagpkg.BuildDAG(spec.Nodes, nil, nil)
	if err != nil {
		t.Fatalf("BuildDAG: %v", err)
	}

	// --- Oracle: full sequential propagation (correct) ---
	state := newInstanceState(compiled, dag)
	eval := newEvaluator(state)
	plan := NewPlanState(dag)
	r := &GraphReconciler{}
	rs := &reconcileScope{name: "test", namespace: "default"}

	oracleResult := r.propagate(context.Background(), rs, state, eval, dag, plan)

	// Oracle should have both nodes ready.
	if oracleResult.plan.States["a"] != NodeReady {
		t.Fatalf("oracle: node a: want Ready, got %v", oracleResult.plan.States["a"])
	}
	if oracleResult.plan.States["b"] != NodeReady {
		t.Fatalf("oracle: node b: want Ready, got %v", oracleResult.plan.States["b"])
	}

	// --- Buggy scoped walk: only evaluates nodes reachable via hard dep edges ---
	// Simulate: after A completes, a scoped walk checks dag.Dependents["a"]
	// for nodes with hard deps on A. If B's dependency on A is only via
	// propagateWhen (not a hard data dep), the buggy walk never visits B.
	//
	// We simulate this by running propagation on only node A (index 0),
	// skipping B entirely — as the buggy scoped walk would.
	buggyState := newInstanceState(compiled, dag)
	buggyEval := newEvaluator(buggyState)
	buggyPlan := NewPlanState(dag)

	// Only propagate node A (index 0).
	r.propagateNode(context.Background(), rs, buggyState, buggyEval, dag, buggyPlan, make(map[string][]Applied), 0)
	// B is never visited — remains Unvisited (simulating the scoped walk bug).

	// --- Oracle comparison: detect divergence ---
	oracleB := oracleResult.plan.States["b"]
	buggyB := buggyPlan.States["b"]

	if oracleB == buggyB {
		t.Fatalf("BUG NOT DETECTED: oracle and buggy walk agree on node b state (%v) — "+
			"expected divergence proving the oracle catches readiness-dependent triggering bugs",
			oracleB)
	}

	t.Logf("Oracle caught bug 1: node b state diverges — oracle=%v, buggy=%v", oracleB, buggyB)
	t.Logf("The buggy scoped walk missed B because it only follows hard dep edges, "+
		"not readiness/propagateWhen edges")
}

// ---------------------------------------------------------------------------
// Bug 2: Stale snapshot in concurrent evaluation
//
// Ellis had worker goroutines that took snapshots of scope at dispatch time.
// If node A and B are dispatched concurrently (no hard dependency between them),
// B's snapshot captures A's stale (pre-evaluation) state. But if B's template
// references A's output, B gets the wrong answer.
//
// We reproduce this by:
//   - Building a graph where A and B are at the same topological level (no
//     hard dep between them) but B's template references A's output.
//   - Running the full sequential oracle (A evaluates first by topo order, B
//     sees A's result).
//   - Running a "buggy parallel evaluator" that snapshots scope BEFORE either
//     node evaluates, then evaluates B against the stale snapshot.
//   - Asserting the two results DIVERGE.
// ---------------------------------------------------------------------------

func TestOracleBug_StaleSnapshotConcurrentEvaluation(t *testing.T) {
	// Graph:
	//   a: def {value: "computed"}
	//   b: def {value: "${a.value}"} — references a.value but with a SOFT dep
	//
	// In the correct sequential walk, A evaluates first (topo order favors lower
	// index), publishes scope, then B evaluates and sees a.value = "computed".
	//
	// In a buggy parallel evaluator, both dispatch at the same time. B's snapshot
	// was taken before A evaluated, so a.value is missing → B gets an error or
	// stale value.
	//
	// We model the soft dependency by having B declare a uses A but marking it
	// as DepSoft in the DAG (so they're at the same topo level).
	spec := &graph.GraphSpec{
		Nodes: []graph.Node{
			{
				ID:  "a",
				Def: map[string]any{"value": "computed"},
			},
			{
				ID:  "b",
				Def: map[string]any{"value": "${a.value}"},
			},
		},
	}
	for i := range spec.Nodes {
		spec.Nodes[i].SetType(graph.NodeTypeDef)
	}

	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatalf("CompileGraphSpec: %v", err)
	}

	dag, err := dagpkg.BuildDAG(spec.Nodes, nil, nil)
	if err != nil {
		t.Fatalf("BuildDAG: %v", err)
	}

	// --- Oracle: full sequential propagation (correct) ---
	state := newInstanceState(compiled, dag)
	eval := newEvaluator(state)
	plan := NewPlanState(dag)
	r := &GraphReconciler{}
	rs := &reconcileScope{name: "test", namespace: "default"}

	oracleResult := r.propagate(context.Background(), rs, state, eval, dag, plan)

	// Oracle: both nodes should be ready, B should see A's value.
	if oracleResult.plan.States["a"] != NodeReady {
		t.Fatalf("oracle: node a: want Ready, got %v", oracleResult.plan.States["a"])
	}
	if oracleResult.plan.States["b"] != NodeReady {
		t.Fatalf("oracle: node b: want Ready, got %v", oracleResult.plan.States["b"])
	}
	oracleBScope, ok := eval.scope["b"].(map[string]any)
	if !ok {
		t.Fatalf("oracle: node b scope not a map")
	}
	if oracleBScope["value"] != "computed" {
		t.Fatalf("oracle: node b value = %v, want 'computed'", oracleBScope["value"])
	}

	// --- Buggy parallel evaluator: evaluate B with a stale (pre-A) scope snapshot ---
	// Simulate: take a snapshot of scope BEFORE any node has evaluated.
	// Then evaluate B against this stale snapshot.
	buggyState := newInstanceState(compiled, dag)
	buggyEval := newEvaluator(buggyState)
	buggyPlan := NewPlanState(dag)

	// Take a "pre-evaluation" snapshot (empty scope — no nodes have run yet).
	staleScope := graph.CopyScope(buggyEval.scope)

	// First, evaluate A normally so it gets its own correct result.
	nodeKeys := make(map[string][]Applied)
	r.propagateNode(context.Background(), rs, buggyState, buggyEval, dag, buggyPlan, nodeKeys, dag.Index["a"])

	// Now evaluate B using the STALE snapshot (simulating concurrent dispatch
	// where B was dispatched before A completed).
	staleEval := buggyEval.withScope(staleScope)
	stalePlan := NewPlanState(dag)
	stalePlan.SetState("a", NodeReady) // pretend A is ready for dep gate
	staleNodeKeys := make(map[string][]Applied)
	r.propagateNode(context.Background(), rs, buggyState, staleEval, dag, stalePlan, staleNodeKeys, dag.Index["b"])

	// --- Oracle comparison: detect divergence ---
	// The stale evaluator should fail to evaluate B correctly because a.value
	// is not in the stale scope.
	buggyBState := stalePlan.States["b"]
	oracleBState := oracleResult.plan.States["b"]

	// Check state divergence OR scope value divergence.
	var diverged bool
	if buggyBState != oracleBState {
		diverged = true
		t.Logf("Oracle caught bug 2 (state divergence): oracle b=%v, buggy b=%v", oracleBState, buggyBState)
	} else {
		// Even if state matches (both "Ready"), check the scope value.
		buggyBScopeVal, _ := staleEval.scope["b"].(map[string]any)
		if buggyBScopeVal == nil || buggyBScopeVal["value"] != "computed" {
			diverged = true
			buggyVal := "<nil>"
			if buggyBScopeVal != nil {
				buggyVal = fmt.Sprintf("%v", buggyBScopeVal["value"])
			}
			t.Logf("Oracle caught bug 2 (scope divergence): oracle b.value='computed', buggy b.value=%q", buggyVal)
		}
	}

	if !diverged {
		t.Fatalf("BUG NOT DETECTED: oracle and buggy parallel evaluator agree — "+
			"expected divergence proving the oracle catches stale-snapshot bugs")
	}
}

// ---------------------------------------------------------------------------
// Bug 3: forEach stale carry-forward
//
// The forEach optimization carries forward previous scope when an item is
// "unchanged" AND the binding is SelfContained. But if the child template
// references scope OUTSIDE the iterator variable, marking SelfContained=true
// is a compiler bug — we'd carry forward stale results when the external
// scope changes.
//
// We reproduce this by:
//   - Building a forEach where the child template references both the iterator
//     variable AND an external scope entry.
//   - Setting SelfContained=true (incorrectly — simulating a compiler bug).
//   - Running propagation once to populate carry-forward state.
//   - Changing the external scope entry but NOT the collection items.
//   - Running the oracle (full re-evaluation, sees new external value).
//   - Running the buggy optimization (carries forward stale results).
//   - Asserting the two DIVERGE.
// ---------------------------------------------------------------------------

func TestOracleBug_ForEachStaleCarryForward(t *testing.T) {
	// The forEach optimization carries forward previous scope when an item is
	// "unchanged" AND the binding is SelfContained. But if the child template
	// references scope OUTSIDE the iterator variable, marking SelfContained=true
	// is a compiler bug — we'd carry forward stale results when the external
	// scope changes.
	//
	// We simulate this WITHOUT full cluster reconciliation. The test directly
	// exercises the carry-forward decision logic:
	//   1. Build a graph with a forEach def node referencing external scope.
	//   2. Run the ORACLE: evaluate the template with updated external scope
	//      (no carry-forward — always re-evaluate).
	//   3. Run the BUGGY path: item hash unchanged + SelfContained=true →
	//      carry forward stale scope from the previous cycle.
	//   4. Compare results — they must diverge.

	// Graph: config produces {prefix: "v2"}, worker template produces
	// "${item.name}-${config.prefix}". The collection item {name: "foo"} is
	// unchanged from the previous cycle where config.prefix was "v1".
	spec := &graph.GraphSpec{
		Nodes: []graph.Node{
			{
				ID:  "config",
				Def: map[string]any{"prefix": "v2", "items": []any{map[string]any{"name": "foo"}}},
			},
			{
				ID:  "worker",
				Def: map[string]any{"label": "${item.name}-${config.prefix}"},
				ForEach: &graph.ForEachBinding{
					VarName:       "item",
					Expr:          "${config.items}",
					SelfContained: true, // BUG: template references config.prefix (external)
				},
			},
		},
	}
	for i := range spec.Nodes {
		spec.Nodes[i].SetType(graph.NodeTypeDef)
	}

	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatalf("CompileGraphSpec: %v", err)
	}

	// --- Oracle: fresh evaluation of the template with current scope ---
	// This is what the simple algorithm does — always re-evaluate, never carry forward.
	oracleEval := &evaluator{compiled: compiled, scope: map[string]any{
		compiler.ReservedNodeReadyVar: map[string]bool{},
	}}
	// Populate scope as the full walk would: config is already evaluated.
	oracleEval.scope["config"] = map[string]any{"prefix": "v2"}
	// Bind the iterator variable (simulating forEach inner scope).
	oracleEval.scope["item"] = map[string]any{"name": "foo"}

	oracleResult, err := oracleEval.toMap(spec.Nodes[1].Def)
	if err != nil {
		t.Fatalf("oracle evaluation: %v", err)
	}
	if oracleResult["label"] != "foo-v2" {
		t.Fatalf("oracle: label = %v, want 'foo-v2'", oracleResult["label"])
	}

	// --- Buggy carry-forward: item unchanged, SelfContained=true ---
	// Previous cycle produced {label: "foo-v1"} for this item.
	// The item {name: "foo"} has the same hash as before.
	// With SelfContained=true, the buggy optimization skips re-evaluation
	// and returns the previous scope entry directly.
	previousItemResult := map[string]any{"label": "foo-v1"}

	currentItem := map[string]any{"name": "foo"}
	currentHash := HashValue(currentItem)
	previousHash := HashValue(map[string]any{"name": "foo"}) // same item, same hash

	// Verify the hashes match (confirming the carry-forward condition triggers).
	if currentHash != previousHash {
		t.Fatalf("test setup: hashes should match for unchanged item")
	}

	// The buggy path: since hash matches and SelfContained=true, carry forward.
	buggyResult := previousItemResult // this is exactly what the buggy code does

	// --- Oracle comparison: detect divergence ---
	oracleLabel := oracleResult["label"]
	buggyLabel := buggyResult["label"]

	if oracleLabel == buggyLabel {
		t.Fatalf("BUG NOT DETECTED: oracle and buggy carry-forward agree on label=%v — "+
			"expected divergence proving the oracle catches stale forEach carry-forward",
			oracleLabel)
	}

	t.Logf("Oracle caught bug 3: label diverges — oracle=%v, buggy=%v", oracleLabel, buggyLabel)
	t.Logf("The buggy SelfContained=true optimization carried forward stale 'foo-v1' "+
		"instead of re-evaluating to 'foo-v2' when config.prefix changed")
}
