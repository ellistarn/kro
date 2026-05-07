// propagate_scoped.go implements scoped (optimized) DAG propagation.
//
// Instead of walking all nodes in topological order, scoped propagation
// walks only nodes reachable from a set of triggered nodes. Unaffected
// nodes retain their previous state and scope entry.
//
// Per 005-reconciliation-optimized.md: "The optimized version walks only
// nodes reachable from a change."
package graphcontroller

import (
	"context"

	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
)

// propagateScoped executes a scoped DAG propagation, evaluating only
// nodes in the affected set (triggered nodes plus their transitive
// forward-reachable descendants).
//
// Unaffected nodes carry forward their previous state, scope entry,
// and applied keys from the prior reconcile cycle.
//
// On cold start (no previous state), all nodes must be triggered.
// The affected set equals the full DAG and the result is identical
// to propagate().
func (r *GraphReconciler) propagateScoped(
	ctx context.Context,
	rs *reconcileScope,
	state *instanceState,
	eval *evaluator,
	dag *dagpkg.DAG,
	plan *PlanState,
	triggered map[int]bool,
) *propagateResult {
	result := &propagateResult{
		plan: plan,
	}

	// Compute affected set: triggered nodes + reachability union.
	affected := computeAffectedSet(dag, triggered)

	// Per-node applied keys — flattened into result.keys after the propagation.
	nodeKeys := make(map[string][]Applied, len(dag.Nodes))

	for _, nodeIdx := range dag.TopologicalOrder {
		if !affected[nodeIdx] {
			// Unaffected node: carry forward previous state.
			nodeID := dag.Nodes[nodeIdx].ID
			carryForwardNode(state, eval, plan, nodeKeys, nodeID)
			continue
		}

		nrOut := r.propagateNode(ctx, rs, state, eval, dag, plan, nodeKeys, nodeIdx)
		if nrOut != nil {
			result.nodeErrors = append(result.nodeErrors, nrOut.errMsgs...)
			if nrOut.needsRecompile {
				result.needsRecompile = true
			}
		}
	}

	// --- Post-propagation ---

	// Flatten per-node keys into the applied key set.
	result.nodeKeys = nodeKeys
	for _, keys := range nodeKeys {
		result.keys = append(result.keys, keys...)
	}

	// Derive aggregate state from the DAG plan.
	result.summary = plan.Summary()

	return result
}

// computeAffectedSet returns the set of node indices that must be
// evaluated: the triggered nodes themselves, plus all nodes transitively
// reachable from any triggered node via forward edges.
func computeAffectedSet(dag *dagpkg.DAG, triggered map[int]bool) map[int]bool {
	affected := make(map[int]bool, len(triggered)*2)
	for idx := range triggered {
		affected[idx] = true
		for reachable := range dag.Reachability[idx] {
			affected[reachable] = true
		}
	}
	return affected
}

// carryForwardNode restores a node's previous state, scope, and applied
// keys without re-evaluation. This is valid because an unaffected node's
// inputs have not changed, so re-evaluation would produce the same result.
func carryForwardNode(
	state *instanceState,
	eval *evaluator,
	plan *PlanState,
	nodeKeys map[string][]Applied,
	nodeID string,
) {
	// Restore plan state.
	if prevState, ok := state.previousNodeStates[nodeID]; ok {
		plan.SetState(nodeID, prevState)
	}

	// Restore scope entry.
	if prevScope, ok := state.previousScope[nodeID]; ok {
		eval.scope[nodeID] = prevScope
		// Restore nodeReady if present.
		if eval.nodeReady != nil {
			if prevState, ok := state.previousNodeStates[nodeID]; ok {
				eval.nodeReady[nodeID] = (prevState == NodeReady)
			}
		}
	}

	// Restore applied keys.
	if prevKeys, ok := state.previousNodeKeys[nodeID]; ok {
		nodeKeys[nodeID] = prevKeys
	}
}
