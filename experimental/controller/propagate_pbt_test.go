package graphcontroller

import (
	"context"
	"testing"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	"github.com/ellistarn/kro/experimental/controller/graph"
)

// TestPropagateOracle exercises the sequential propagation algorithm on a
// trivial 3-node definition chain. Definition nodes are pure (no cluster
// access) — they evaluate CEL expressions and publish results to scope.
//
// This test is scaffolding for a future property-based comparison between
// the sequential oracle and an optimized parallel implementation. The
// assertions verify internal consistency: all nodes evaluated, states make
// sense, and scope contains the expected values.
func TestPropagateOracle(t *testing.T) {
	// Build a 3-node definition chain: a -> b -> c
	// Each node is a def that produces a map. Downstream nodes reference
	// their predecessor's output via CEL expressions.
	spec := &graph.GraphSpec{
		Nodes: []graph.Node{
			{
				ID:  "a",
				Def: map[string]any{"value": "hello"},
			},
			{
				ID:  "b",
				Def: map[string]any{"value": "${a.value}"},
			},
			{
				ID:  "c",
				Def: map[string]any{"value": "${b.value}"},
			},
		},
	}
	// Set node types to Definition.
	for i := range spec.Nodes {
		spec.Nodes[i].SetType(graph.NodeTypeDef)
	}

	// Compile CEL expressions.
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatalf("CompileGraphSpec: %v", err)
	}

	// Build the DAG (topological sort, dependency edges).
	dag, err := dagpkg.BuildDAG(spec.Nodes, nil, nil)
	if err != nil {
		t.Fatalf("BuildDAG: %v", err)
	}

	// Create instance state and evaluator.
	state := newInstanceState(compiled, dag)
	eval := newEvaluator(state)
	plan := NewPlanState(dag)

	// Run propagation with a zero-value reconciler (no cluster needed for defs).
	r := &GraphReconciler{}
	rs := &reconcileScope{
		name:      "test-graph",
		namespace: "default",
	}
	result := r.propagate(context.Background(), rs, state, eval, dag, plan)

	// --- Assertions: internal consistency ---

	// 1. All nodes should be in a terminal state (not Pending/Unvisited).
	for _, node := range dag.Nodes {
		nodeState, exists := result.plan.States[node.ID]
		if !exists {
			t.Errorf("node %q missing from plan states", node.ID)
			continue
		}
		if nodeState == NodePending || nodeState == NodeUnvisited {
			t.Errorf("node %q in non-terminal state %v", node.ID, nodeState)
		}
	}

	// 2. All nodes should be Ready (defs with no readyWhen are vacuously ready).
	for _, node := range dag.Nodes {
		if result.plan.States[node.ID] != NodeReady {
			t.Errorf("node %q: want NodeReady, got %v", node.ID, result.plan.States[node.ID])
		}
	}

	// 3. No errors reported.
	if len(result.nodeErrors) > 0 {
		t.Errorf("unexpected node errors: %v", result.nodeErrors)
	}

	// 4. Scope should contain all three nodes with correct propagated values.
	for _, id := range []string{"a", "b", "c"} {
		scopeVal, exists := eval.scope[id]
		if !exists {
			t.Errorf("node %q missing from scope", id)
			continue
		}
		m, ok := scopeVal.(map[string]any)
		if !ok {
			t.Errorf("node %q scope value is %T, want map[string]any", id, scopeVal)
			continue
		}
		if m["value"] != "hello" {
			t.Errorf("node %q: scope[value] = %v, want %q", id, m["value"], "hello")
		}
	}

	// 5. Summary should report all ready, no errors.
	if result.summary.ReadyCount != 3 {
		t.Errorf("summary.ReadyCount = %d, want 3", result.summary.ReadyCount)
	}
	if result.summary.HasError {
		t.Error("summary.HasError should be false")
	}
}
