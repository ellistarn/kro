package graphcontroller_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestCoOwnershipProducesConflict proves that when two kro field managers
// (*.internal.kro.run) own overlapping fields on the same resource, the
// Graph enters Conflict state — even though the values agree and the SSA
// apply succeeded.
//
// Design 003-ownership § Co-ownership Detection:
//
//	"If the intersection is non-empty, the node enters Conflict — the same
//	state as a 409."
func TestCoOwnershipProducesConflict(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	externalManager := "other-graph." + ns + ".internal.kro.run"

	externalPayload := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "coown-target",
			"namespace": ns,
		},
		"data": map[string]any{
			"shared-key": "same-value",
		},
	}
	raw, err := json.Marshal(externalPayload)
	require.NoError(t, err)

	extCM := &unstructured.Unstructured{}
	extCM.SetGroupVersionKind(cmGVK)
	extCM.SetName("coown-target")
	extCM.SetNamespace(ns)
	require.NoError(t, k8sClient.Patch(ctx, extCM, client.RawPatch(
		types.ApplyPatchType, raw),
		client.ForceOwnership,
		client.FieldOwner(externalManager),
	))
	t.Logf("Pre-created ConfigMap with field manager %s owning data.shared-key", externalManager)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata":   map[string]any{"name": "test-coown-conflict", "namespace": ns},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "shared",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "coown-target"},
							"data":       map[string]any{"shared-key": "same-value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "test-coown-conflict", Namespace: ns}
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Conflict"),
		"Graph should enter Conflict state due to kro-to-kro field co-ownership")
	t.Log("Graph entered Conflict state — co-ownership detected")

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))

	msg := graphReadyMessage(g)
	t.Logf("Ready condition message: %s", msg)
	assert.Contains(t, msg, "field conflict",
		"Conflict message should reference field conflict")
	assert.False(t, graphReady(g),
		"Graph should NOT be Ready — co-ownership produces Conflict")
}
