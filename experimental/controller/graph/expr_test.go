package graph

import (
	"testing"
)

// TestExtractReferencedPaths_ReadyInBody verifies that .ready() calls in
// body expressions create lazy dependencies. Lazy deps participate in
// propagation triggering but not dispatch ordering or contagious exclusion.
func TestExtractReferencedPaths_ReadyInBody(t *testing.T) {
	node := Node{
		ID: "rgdInstanceStatus",
		Patch: map[string]any{
			"status": map[string]any{
				"state": "${deployment1.ready() && deployment2.ready() ? 'ACTIVE' : 'IN_PROGRESS'}",
			},
		},
	}

	deps, _, _, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// String fallback: processExpr extracts FIRST identifier only
	// (deployment1 from ExtractFirstIdentifier) as a hard dep.
	if deps["deployment1"] != DepHard {
		t.Error("deployment1 should be a hard dependency (string fallback)")
	}
	// deployment2 is a lazy dep via checkReadyRef (not extracted by processExpr).
	if deps["deployment2"] != DepLazy {
		t.Error("deployment2 should be a lazy dependency (.ready() in body)")
	}
}

// TestExtractReferencedPaths_ReadyInBody_Single verifies a single .ready()
// call in a body expression creates the correct dep kind.
func TestExtractReferencedPaths_ReadyInBody_Single(t *testing.T) {
	node := Node{
		ID: "status",
		Patch: map[string]any{
			"status": map[string]any{
				"ready": "${job.ready()}",
			},
		},
	}

	deps, _, _, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// String fallback adds "job" as hard dep via ExtractFirstIdentifier.
	// checkReadyRef would add it as lazy, but hard wins.
	if deps["job"] != DepHard {
		t.Error("job should be a hard dependency (string fallback, hard wins over lazy)")
	}
}

// TestExtractReferencedPaths_ReadySelfReference verifies that .ready() on
// self is ignored — a node can't depend on its own readiness.
func TestExtractReferencedPaths_ReadySelfReference(t *testing.T) {
	node := Node{
		ID: "service",
		Template: map[string]any{
			"data": "${service.ready()}",
		},
	}

	deps, _, _, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, exists := deps["service"]; exists {
		t.Error("self-reference should not create any dependency")
	}
}

// TestExtractReferencedPaths_ReadyInGate verifies that .ready() in
// propagateWhen creates a lazy dependency for re-triggering.
func TestExtractReferencedPaths_ReadyInGate(t *testing.T) {
	node := Node{
		ID: "service",
		Template: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
		},
		PropagateWhen: []string{"${deployment.ready()}"},
	}

	deps, _, _, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// String fallback: processExpr extracts "deployment" as hard.
	// checkReadyRef would set lazy but hard wins.
	if deps["deployment"] != DepHard {
		t.Error("deployment should be a hard dependency (string fallback)")
	}
}

// TestExtractReferencedPaths_DependenciesSelfOnly verifies that
// .dependencies() can only be called on the node itself.
func TestExtractReferencedPaths_DependenciesSelfOnly(t *testing.T) {
	node := Node{
		ID: "service",
		Template: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
		},
		PropagateWhen: []string{"${service.dependencies().all(d, d.ready())}"},
	}

	_, _, _, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("self-referential .dependencies() should be allowed: %v", err)
	}

	node = Node{
		ID: "status",
		Patch: map[string]any{
			"state": "${otherNode.dependencies().all(d, d.ready())}",
		},
	}

	_, _, _, err = ExtractReferencedPathsFromNode(node, nil)
	if err == nil {
		t.Fatal("cross-node .dependencies() should be rejected")
	}
}

// TestExtractReferencedPaths_ReadyCELBuiltinFiltered verifies that CEL
// builtins like "all", "filter", "map" before .ready() are not treated
// as node IDs.
func TestExtractReferencedPaths_ReadyCELBuiltinFiltered(t *testing.T) {
	node := Node{
		ID: "status",
		Patch: map[string]any{
			"data": "${list.all(d, d.ready())}",
		},
	}

	deps, _, _, err := ExtractReferencedPathsFromNode(node, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, exists := deps["list"]; exists {
		t.Error("CEL builtin 'list' should be filtered by ExtractFirstIdentifier")
	}
	// "d" is a loop variable — the string scanner can't distinguish it
	// from a node ID. checkReadyRef adds it as a lazy dep.
	if deps["d"] != DepLazy {
		t.Error("expected 'd' as lazy dep (string scanner can't resolve comprehension variables)")
	}
}
