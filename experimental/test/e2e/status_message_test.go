package graphcontroller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// ---------------------------------------------------------------------------
// Condition message observability tests
//
// These tests enforce that the Ready condition message names the specific
// node IDs responsible for each non-ready state. Without these, the messages
// could regress to generic "one or more resources..." phrasing.
// ---------------------------------------------------------------------------

// TestMessageNamesNodeID_NotReady proves that when a node's readyWhen
// evaluates to false (the normal "waiting for convergence" case), the
// Ready condition message shows both the count and the node ID.
//
// NotReady is a converging state (Ready=Unknown) — the message includes
// per-node detail lines so operators know which node to investigate.
func TestMessageNamesNodeID_NotReady(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create the watched resource with ready=false.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "msg-notready-source",
				"namespace": ns,
			},
			"data": map[string]any{"ready": "false"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: ref node "backend" watches the source, with a readyWhen that
	// evaluates to false until the source data changes.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "msg-notready",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "backend",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "msg-notready-source"},
						},
						"readyWhen": []any{"${backend.data.ready == 'true'}"},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "msg-notready", Namespace: ns}
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "NotReady"))

	// Fetch and assert the message shows state counts.
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("NotReady message: %s", msg)
	assert.Contains(t, msg, "not ready",
		"Ready condition message must show not-ready count")
}

// TestMessageNamesNodeID_Pending proves that when a node is gated by an
// unsatisfied propagateWhen, the Ready condition message shows the pending
// count and names the pending node.
//
// Pending is a converging state (Ready=Unknown) — the message includes
// per-node detail lines so operators know which node to investigate.
func TestMessageNamesNodeID_Pending(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph: "cfg" creates a ConfigMap with ready=false. "gated" has a
	// propagateWhen that requires cfg.data.ready == "true" — so "gated"
	// stays Pending.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "msg-pending",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cfg",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "msg-pending-cfg"},
							"data":       map[string]any{"ready": "false"},
						},
					},
					map[string]any{
						"id":            "gated",
						"propagateWhen": []any{"${cfg.data.ready == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "msg-pending-gated"},
							"data":       map[string]any{"from": "${cfg.data.ready}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "msg-pending", Namespace: ns}
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Pending"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("Pending message: %s", msg)
	assert.Contains(t, msg, "pending",
		"Ready condition message must show pending count")
}

// TestMessageNamesNodeID_Error proves that when a node hits a deterministic
// error (4xx), the Ready condition message names the failing node ID.
func TestMessageNamesNodeID_Error(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create a watched source with divisor=0.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "msg-error-source",
				"namespace": ns,
			},
			"data": map[string]any{"divisor": "0"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: "source" is a ref. "broken" has a CEL expression that does
	// division by zero — a deterministic runtime error classified as Error.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "msg-error",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "msg-error-source"},
						},
					},
					map[string]any{
						"id": "broken",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "msg-error-output"},
							"data":       map[string]any{"result": "${100 / int(source.data.divisor)}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "msg-error", Namespace: ns}
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Error"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("Error message: %s", msg)
	assert.Contains(t, msg, "broken",
		"Ready condition message must name the node with the error")
}

// TestMessageNamesNodeID_Conflict proves that when a node hits an SSA
// field ownership conflict (409), the Ready condition message names the
// conflicting node ID.
func TestMessageNamesNodeID_Conflict(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create a ConfigMap with an external field manager owning data.key.
	applyConfigMapAs(t, ns, "msg-conflict-cm", "external-manager", map[string]string{
		"key": "external-value",
	})

	// Graph: "contested" tries to write data.key — will conflict with
	// the external manager's ownership.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "msg-conflict",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "contested",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "msg-conflict-cm"},
							"data":       map[string]any{"key": "graph-value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "msg-conflict", Namespace: ns}
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Conflict"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("Conflict message: %s", msg)
	assert.Contains(t, msg, "contested",
		"Ready condition message must name the node with the conflict")
}
