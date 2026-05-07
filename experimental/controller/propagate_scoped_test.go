package graphcontroller

import (
	"context"
	"testing"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	"github.com/ellistarn/kro/experimental/controller/graph"
)

// buildDefChain builds a definition chain and returns compiled artifacts.
// Each node produces map[string]any{"value": <expr>} where expr references
// the predecessor's .value field (except the root which uses a literal).
func buildDefChain(t *testing.T, ids []string, rootValue string) (*compiler.CompiledGraph, *dagpkg.DAG, *graph.GraphSpec) {
	t.Helper()
	nodes := make([]graph.Node, len(ids))
	for i, id := range ids {
		var value any
		if i == 0 {
			value = rootValue
		} else {
			value = "${" + ids[i-1] + ".value}"
		}
		nodes[i] = graph.Node{
			ID:  id,
			Def: map[string]any{"value": value},
		}
		nodes[i].SetType(graph.NodeTypeDef)
	}

	spec := &graph.GraphSpec{Nodes: nodes}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatalf("CompileGraphSpec: %v", err)
	}
	dag, err := dagpkg.BuildDAG(spec.Nodes, nil, nil)
	if err != nil {
		t.Fatalf("BuildDAG: %v", err)
	}
	return compiled, dag, spec
}

// runSimplePropagation runs the simple propagation and returns the result
// plus the evaluator (for scope inspection).
func runSimplePropagation(t *testing.T, compiled *compiler.CompiledGraph, dag *dagpkg.DAG) (*propagateResult, *evaluator) {
	t.Helper()
	state := newInstanceState(compiled, dag)
	eval := newEvaluator(state)
	plan := NewPlanState(dag)
	r := &GraphReconciler{}
	rs := &reconcileScope{name: "test", namespace: "default"}
	result := r.propagate(context.Background(), rs, state, eval, dag, plan)
	return result, eval
}

// TestScopedPropagation_ColdStart verifies that when all nodes are triggered
// (cold start), scoped propagation produces the same result as simple propagation.
func TestScopedPropagation_ColdStart(t *testing.T) {
	compiled, dag, _ := buildDefChain(t, []string{"a", "b", "c"}, "hello")

	// Run simple propagation as oracle.
	oracleResult, oracleEval := runSimplePropagation(t, compiled, dag)

	// Run scoped propagation with all nodes triggered (cold start).
	state := newInstanceState(compiled, dag)
	eval := newEvaluator(state)
	plan := NewPlanState(dag)

	// Cold start: trigger all nodes.
	triggered := make(map[int]bool, len(dag.Nodes))
	for _, idx := range dag.TopologicalOrder {
		triggered[idx] = true
	}

	r := &GraphReconciler{}
	rs := &reconcileScope{name: "test", namespace: "default"}
	scopedResult := r.propagateScoped(context.Background(), rs, state, eval, dag, plan, triggered)

	// Assert equivalence.
	assertPropagateResultsEqual(t, oracleResult, scopedResult, oracleEval, eval, dag)
}

// TestScopedPropagation_SingleChange verifies that when only one root node
// is triggered, unreachable nodes retain previous state and the overall
// result is equivalent to full re-evaluation (since inputs haven't changed).
func TestScopedPropagation_SingleChange(t *testing.T) {
	// Build a tree: a -> b -> c, and d (independent).
	nodes := []graph.Node{
		{ID: "a", Def: map[string]any{"value": "alpha"}},
		{ID: "b", Def: map[string]any{"value": "${a.value}"}},
		{ID: "c", Def: map[string]any{"value": "${b.value}"}},
		{ID: "d", Def: map[string]any{"value": "delta"}},
	}
	for i := range nodes {
		nodes[i].SetType(graph.NodeTypeDef)
	}
	spec := &graph.GraphSpec{Nodes: nodes}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatalf("CompileGraphSpec: %v", err)
	}
	dag, err := dagpkg.BuildDAG(spec.Nodes, nil, nil)
	if err != nil {
		t.Fatalf("BuildDAG: %v", err)
	}

	// First: run a full propagation to establish "previous" state.
	state := newInstanceState(compiled, dag)
	eval := newEvaluator(state)
	plan := NewPlanState(dag)
	r := &GraphReconciler{}
	rs := &reconcileScope{name: "test", namespace: "default"}
	firstResult := r.propagate(context.Background(), rs, state, eval, dag, plan)

	// Record previous state from first run.
	state.previousNodeStates = make(map[string]NodeState, len(dag.Nodes))
	state.previousScope = make(map[string]any, len(dag.Nodes))
	state.previousNodeKeys = firstResult.nodeKeys
	for id, ns := range firstResult.plan.States {
		state.previousNodeStates[id] = ns
	}
	for _, node := range dag.Nodes {
		if v, ok := eval.scope[node.ID]; ok {
			state.previousScope[node.ID] = v
		}
	}

	// Now run scoped propagation triggering only node "a".
	// Since d is independent, it should be carried forward.
	eval2 := newEvaluator(state)
	plan2 := NewPlanState(dag)
	triggered := map[int]bool{dag.Index["a"]: true}
	scopedResult := r.propagateScoped(context.Background(), rs, state, eval2, dag, plan2, triggered)

	// Also run full re-evaluation as oracle (inputs haven't changed).
	oracleState := newInstanceState(compiled, dag)
	oracleEval := newEvaluator(oracleState)
	oraclePlan := NewPlanState(dag)
	oracleResult := r.propagate(context.Background(), rs, oracleState, oracleEval, dag, oraclePlan)

	// Assert equivalence between scoped and oracle.
	assertPropagateResultsEqual(t, oracleResult, scopedResult, oracleEval, eval2, dag)

	// Verify that "d" was NOT re-evaluated — its scope came from carry-forward.
	// (We can't directly check "was it evaluated" but we can check the value is correct.)
	dScope, ok := eval2.scope["d"]
	if !ok {
		t.Fatal("node d missing from scope")
	}
	dMap, ok := dScope.(map[string]any)
	if !ok {
		t.Fatalf("node d scope is %T, want map[string]any", dScope)
	}
	if dMap["value"] != "delta" {
		t.Errorf("node d scope[value] = %v, want %q", dMap["value"], "delta")
	}

	// Verify that "d" was NOT in the affected set.
	affected := computeAffectedSet(dag, triggered)
	if affected[dag.Index["d"]] {
		t.Error("node d should NOT be in affected set when only a is triggered")
	}
}

// TestScopedPropagation_OracleEquivalence runs both simple and scoped
// propagation on a larger tree and asserts identical results.
// Pattern: definition tree with branching. Trigger a subset of roots
// and verify the scoped result matches a full re-evaluation.
func TestScopedPropagation_OracleEquivalence(t *testing.T) {
	// Tree structure:
	//   root1 -> mid1 -> leaf1
	//   root2 -> mid2 -> leaf2
	//   mid1 + mid2 -> join
	nodes := []graph.Node{
		{ID: "root1", Def: map[string]any{"value": "r1"}},
		{ID: "root2", Def: map[string]any{"value": "r2"}},
		{ID: "mid1", Def: map[string]any{"value": "${root1.value}"}},
		{ID: "mid2", Def: map[string]any{"value": "${root2.value}"}},
		{ID: "leaf1", Def: map[string]any{"value": "${mid1.value}"}},
		{ID: "leaf2", Def: map[string]any{"value": "${mid2.value}"}},
		{ID: "join", Def: map[string]any{"left": "${mid1.value}", "right": "${mid2.value}"}},
	}
	for i := range nodes {
		nodes[i].SetType(graph.NodeTypeDef)
	}
	spec := &graph.GraphSpec{Nodes: nodes}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatalf("CompileGraphSpec: %v", err)
	}
	dag, err := dagpkg.BuildDAG(spec.Nodes, nil, nil)
	if err != nil {
		t.Fatalf("BuildDAG: %v", err)
	}

	// Run first full propagation to establish previous state.
	state := newInstanceState(compiled, dag)
	eval := newEvaluator(state)
	plan := NewPlanState(dag)
	r := &GraphReconciler{}
	rs := &reconcileScope{name: "test", namespace: "default"}
	firstResult := r.propagate(context.Background(), rs, state, eval, dag, plan)

	// Record previous state.
	state.previousNodeStates = make(map[string]NodeState, len(dag.Nodes))
	state.previousScope = make(map[string]any, len(dag.Nodes))
	state.previousNodeKeys = firstResult.nodeKeys
	for id, ns := range firstResult.plan.States {
		state.previousNodeStates[id] = ns
	}
	for _, node := range dag.Nodes {
		if v, ok := eval.scope[node.ID]; ok {
			state.previousScope[node.ID] = v
		}
	}

	// Trigger only root1. Affected set should be: root1, mid1, leaf1, join.
	// Unaffected: root2, mid2, leaf2.
	triggered := map[int]bool{dag.Index["root1"]: true}
	affected := computeAffectedSet(dag, triggered)

	// Verify affected set is correct.
	expectedAffected := map[string]bool{"root1": true, "mid1": true, "leaf1": true, "join": true}
	for _, node := range dag.Nodes {
		idx := dag.Index[node.ID]
		if expectedAffected[node.ID] && !affected[idx] {
			t.Errorf("node %q should be affected but is not", node.ID)
		}
		if !expectedAffected[node.ID] && affected[idx] {
			t.Errorf("node %q should NOT be affected but is", node.ID)
		}
	}

	// Run scoped propagation.
	eval2 := newEvaluator(state)
	plan2 := NewPlanState(dag)
	scopedResult := r.propagateScoped(context.Background(), rs, state, eval2, dag, plan2, triggered)

	// Run oracle (full re-evaluation with same inputs).
	oracleState := newInstanceState(compiled, dag)
	oracleEval := newEvaluator(oracleState)
	oraclePlan := NewPlanState(dag)
	oracleResult := r.propagate(context.Background(), rs, oracleState, oracleEval, dag, oraclePlan)

	// Assert equivalence.
	assertPropagateResultsEqual(t, oracleResult, scopedResult, oracleEval, eval2, dag)
}

// assertPropagateResultsEqual checks that two propagate results are equivalent.
func assertPropagateResultsEqual(t *testing.T, expected, actual *propagateResult, expectedEval, actualEval *evaluator, dag *dagpkg.DAG) {
	t.Helper()

	// Plan states must match.
	for _, node := range dag.Nodes {
		es := expected.plan.States[node.ID]
		as := actual.plan.States[node.ID]
		if es != as {
			t.Errorf("node %q: plan state mismatch: expected %v, got %v", node.ID, es, as)
		}
	}

	// Scope values must match.
	for _, node := range dag.Nodes {
		ev := expectedEval.scope[node.ID]
		av := actualEval.scope[node.ID]
		if ev == nil && av == nil {
			continue
		}
		if ev == nil || av == nil {
			t.Errorf("node %q: scope presence mismatch (expected=%v, actual=%v)", node.ID, ev != nil, av != nil)
			continue
		}
		// Compare map values for definition nodes.
		eMap, eOk := ev.(map[string]any)
		aMap, aOk := av.(map[string]any)
		if eOk && aOk {
			for k, eV := range eMap {
				if aV, ok := aMap[k]; !ok || aV != eV {
					t.Errorf("node %q: scope[%q] mismatch: expected %v, got %v", node.ID, k, eV, aV)
				}
			}
		}
	}

	// Summary must match.
	if expected.summary != actual.summary {
		t.Errorf("summary mismatch: expected %+v, got %+v", expected.summary, actual.summary)
	}

	// Error count must match.
	if len(expected.nodeErrors) != len(actual.nodeErrors) {
		t.Errorf("nodeErrors count mismatch: expected %d, got %d", len(expected.nodeErrors), len(actual.nodeErrors))
	}
}
