package graphcontroller_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// ---------------------------------------------------------------------------
// Condition message format tests
//
// These tests enforce the structured message format defined in 001-graph.md §
// Condition Messages:
//   - Ready message: state counts summary + per-node error detail lines
//   - Compiled message: node count on success, error text on failure
//
// Tests use assert.Equal on exact messages where possible to fortify against
// format regressions. For error detail lines where the internal error string
// is an implementation detail, tests assert the structural prefix (node ID +
// state label) and verify line count.
// ---------------------------------------------------------------------------

// TestMessageFormat_ReadyShowsNodeCount proves that when all nodes are ready,
// the Ready message is exactly "<N> ready".
func TestMessageFormat_ReadyShowsNodeCount(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "fmt-ready",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "one",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-ready-one"},
							"data":       map[string]any{"key": "value"},
						},
					},
					map[string]any{
						"id": "two",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-ready-two"},
							"data":       map[string]any{"from": "${one.data.key}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "fmt-ready", Namespace: ns}
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("Ready message: %q", msg)
	assert.Equal(t, "2 ready", msg)
}

// TestMessageFormat_CompiledShowsNodeCount proves that when compilation
// succeeds, the Compiled message is exactly "<N> nodes".
func TestMessageFormat_CompiledShowsNodeCount(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "fmt-compiled",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cfg",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-compiled-cfg"},
							"data":       map[string]any{"key": "value"},
						},
					},
					map[string]any{
						"id": "svc",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-compiled-svc"},
							"data":       map[string]any{"ref": "${cfg.data.key}"},
						},
					},
					map[string]any{
						"id": "dep",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-compiled-dep"},
							"data":       map[string]any{"ref": "${svc.data.ref}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "fmt-compiled", Namespace: ns}
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphCompiledMessage(g)
	t.Logf("Compiled message: %q", msg)
	assert.Equal(t, "3 nodes", msg)
}

// TestMessageFormat_ErrorShowsStateCounts proves that when a node has an
// error, the Ready message has:
//   - Line 1: exact summary with state counts
//   - Line 2+: indented detail lines with "nodeID (state): reason" format
func TestMessageFormat_ErrorShowsStateCounts(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create a source with divisor=0.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "fmt-error-source",
				"namespace": ns,
			},
			"data": map[string]any{"divisor": "0"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: "source" ref + "broken" does division by zero + "downstream" depends on broken.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "fmt-error",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-error-source"},
						},
					},
					map[string]any{
						"id": "broken",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-error-broken"},
							"data":       map[string]any{"result": "${100 / int(source.data.divisor)}"},
						},
					},
					map[string]any{
						"id": "downstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-error-downstream"},
							"data":       map[string]any{"ref": "${broken.data.result}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "fmt-error", Namespace: ns}
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Error"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("Error message:\n%s", msg)

	// Parse into lines.
	lines := strings.Split(msg, "\n")
	require.Equal(t, 3, len(lines),
		"message should have 1 summary + 2 detail lines (error + blocked)")

	// Line 1: exact summary line. "source" is ready, "downstream" is blocked,
	// "broken" is in error.
	assert.Equal(t, "1 ready, 1 blocked, 1 error", lines[0],
		"summary line must show exact state counts")

	// Line 2: detail line for the error node (alphabetically first: "broken").
	// Format: "  nodeID (state): reason"
	assert.True(t, strings.HasPrefix(lines[1], "  broken (error): "),
		"detail line must start with '  broken (error): ', got: %q", lines[1])
	assert.Contains(t, lines[1], "division by zero",
		"detail line must contain the root cause")

	// Line 3: detail line for the blocked node.
	assert.Equal(t, "  downstream (blocked)", lines[2],
		"blocked node must appear as detail line without reason")
}

// TestMessageFormat_NotReadyShowsCounts proves that when a node's readyWhen
// is unsatisfied, the Ready message shows a summary line and detail line
// identifying which node is not ready.
func TestMessageFormat_NotReadyShowsCounts(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "fmt-notready-src",
				"namespace": ns,
			},
			"data": map[string]any{"ready": "false"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "fmt-notready",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "backend",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-notready-src"},
						},
						"readyWhen": []any{"${backend.data.ready == 'true'}"},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "fmt-notready", Namespace: ns}
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "NotReady"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("NotReady message: %q", msg)

	// Single-node graph with unsatisfied readyWhen: shows summary + detail.
	assert.Equal(t, "1 not ready\n  backend (not ready)", msg)
}

// TestMessageFormat_PendingShowsCounts proves that when a node is gated by
// propagateWhen, the Ready message is exactly "<N> ready, <M> pending".
func TestMessageFormat_PendingShowsCounts(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "fmt-pending",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cfg",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-pending-cfg"},
							"data":       map[string]any{"ready": "false"},
						},
					},
					map[string]any{
						"id":            "gated",
						"propagateWhen": []any{"${cfg.data.ready == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-pending-gated"},
							"data":       map[string]any{"from": "${cfg.data.ready}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "fmt-pending", Namespace: ns}
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Pending"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("Pending message: %q", msg)

	// "cfg" is ready, "gated" is pending. Detail line shows which node is pending.
	assert.Equal(t, "1 ready, 1 pending\n  gated (pending)", msg)
}

// TestMessageFormat_ConflictShowsDetail proves that SSA conflicts produce
// exact summary + detail line with "(conflict)" label.
func TestMessageFormat_ConflictShowsDetail(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create a ConfigMap with an external field manager.
	applyConfigMapAs(t, ns, "fmt-conflict-cm", "external-mgr", map[string]string{
		"key": "external-value",
	})

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "fmt-conflict",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "contested",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-conflict-cm"},
							"data":       map[string]any{"key": "graph-value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "fmt-conflict", Namespace: ns}
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Conflict"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("Conflict message:\n%s", msg)

	// Exact message: summary + 1 detail line.
	lines := strings.Split(msg, "\n")
	require.Equal(t, 2, len(lines),
		"message should have exactly 1 summary + 1 detail line")

	assert.Equal(t, "1 conflict", lines[0],
		"summary line must be exact")
	assert.Equal(t, "  contested (conflict): field conflict", lines[1],
		"detail line must match exact format: '  nodeID (state): reason'")
}

// TestMessageFormat_MultipleErrors proves that multiple error nodes each get
// their own detail line, sorted alphabetically by node ID.
func TestMessageFormat_MultipleErrors(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create source with divisor=0.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "fmt-multi-source",
				"namespace": ns,
			},
			"data": map[string]any{"divisor": "0"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: source ref + two broken nodes (both divide by zero).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "fmt-multi",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-multi-source"},
						},
					},
					map[string]any{
						"id": "zeta",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-multi-zeta"},
							"data":       map[string]any{"x": "${100 / int(source.data.divisor)}"},
						},
					},
					map[string]any{
						"id": "alpha",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fmt-multi-alpha"},
							"data":       map[string]any{"x": "${200 / int(source.data.divisor)}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "fmt-multi", Namespace: ns}
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Error"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("Multi-error message:\n%s", msg)

	lines := strings.Split(msg, "\n")
	require.Equal(t, 3, len(lines),
		"message should have 1 summary + 2 detail lines")

	// Summary.
	assert.Equal(t, "1 ready, 2 error", lines[0],
		"summary line must count both errors")

	// Detail lines: sorted alphabetically (alpha before zeta).
	assert.True(t, strings.HasPrefix(lines[1], "  alpha (error): "),
		"first detail line must be alpha (alphabetical order), got: %q", lines[1])
	assert.True(t, strings.HasPrefix(lines[2], "  zeta (error): "),
		"second detail line must be zeta (alphabetical order), got: %q", lines[2])
}
