// compile.go orchestrates compilation — turning a GraphRevision's spec into
// a compiled graph artifact. Always compiles from scratch every reconcile.
// No caching of compilation output across cycles.
package graphcontroller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// compileRevision parses a GraphRevision's spec, compiles it, and returns the
// compiled graph and per-instance state. Always recompiles from scratch —
// no cross-cycle caching.
func (r *GraphReconciler) compileRevision(ctx context.Context, namespace string, revision *unstructured.Unstructured) (*graphpkg.GraphSpec, *instanceState, error) {
	instanceKey := revision.GetNamespace() + "/" + revision.GetName()

	// Parse spec, resolve types, compile, assemble DAG.
	spec, err := extractRevisionSpec(revision)
	if err != nil {
		return nil, nil, err
	}

	// Resolve types. Dynamic GVK nodes compile permissively (no hints needed).
	typeInfo := compiler.ResolveNodeTypes(spec.Nodes, r.SchemaResolver)
	compiled, err := compiler.CompileGraphSpec(spec, typeInfo)
	if err != nil {
		return nil, nil, err
	}
	// Assemble a per-instance DAG from the shared topology and this
	// instance's node specs.
	dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)

	// Create fresh state; carry forward forEach item hashes from the previous
	// instance (the only cross-cycle state, per 005-reconciliation-optimized.md).
	state := newInstanceState(compiled, dag)
	if prev := r.Caches.get(instanceKey); prev != nil {
		state.forEachItemHashes = prev.forEachItemHashes
		state.forEachPreviousScope = prev.forEachPreviousScope
		state.forEachPreviousKeys = prev.forEachPreviousKeys
	}
	r.Caches.set(instanceKey, state)
	return spec, state, nil
}
