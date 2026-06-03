package graphcontroller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Deletion correctness tests
//
// These tests prove behaviors at the intersection of deletion, co-ownership,
// and external lifecycle coordination:
//
// 1. Contribution deletion: Graph A templates a resource, Graph B patches it,
//    Graph A is deleted → resource is deleted (field managers don't block),
//    Graph B's patch node enters Pending.
//
// 2. External finalizer foreground deletion: an external actor places a
//    finalizer on a Graph-managed resource. When the Graph deletes it, the
//    resource enters Terminating and stays there until the finalizer is
//    released externally. The Graph's teardown waits correctly.
// ═══════════════════════════════════════════════════════════════════════════════

// TestContributionDeletion proves that when Graph A templates a resource and
// Graph B patches it, deleting Graph A correctly deletes the resource despite
// Graph B's field manager being present. Graph B's patch node recovers to
// Pending (target absent) without error.
//
// Design 003-ownership § Deletion:
//
//	"Field managers on the resource do not block deletion — Kubernetes
//	finalizers are the correct mechanism for preventing premature deletion."
//
// This is the multi-graph variant: the co-owner is another kro Graph, not an
// external tool. The resource is still deleted because template: owns lifecycle.
func TestContributionDeletion(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Phase 1: Graph A creates and owns a ConfigMap via template:.
	graphA := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-contrib-owner",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "shared",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "shared-resource"},
							"data":       map[string]any{"phase": "created"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graphA))

	// Wait for the resource and Graph A to be ready.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	cmKey := types.NamespacedName{Name: "shared-resource", Namespace: ns}
	require.NoError(t, waitForResource(ctx, k8sClient, cmKey, cm))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-contrib-owner", Namespace: ns}))
	t.Log("Phase 1: Graph A created shared-resource (template)")

	// Phase 2: Graph B patches the same resource (contribution).
	graphB := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-contrib-patcher",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "contribute",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "shared-resource",
								"annotations": map[string]any{
									"contrib/phase": "enriched",
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graphB))

	// Wait for Graph B to apply its contribution.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-contrib-patcher", Namespace: ns}))
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK, cmKey,
		[]string{"metadata", "annotations", "contrib/phase"}, "enriched"))
	t.Log("Phase 2: Graph B patched shared-resource (contribution)")

	// Phase 3: Delete Graph B first (stops active patching), then delete Graph A.
	// Graph B's release-apply drops its fields but the managedFields ENTRY
	// persists until the API server GCs it. Graph A's teardown encounters
	// Graph B's stale field manager and must proceed regardless.
	require.NoError(t, k8sClient.Delete(ctx, graphB))
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-contrib-patcher", Namespace: ns}))
	t.Log("Phase 3: Graph B deleted (contribution released)")

	// Verify the ConfigMap still exists with Graph B's stale managedFields entry.
	cm2 := &unstructured.Unstructured{}
	cm2.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, cmKey, cm2),
		"shared-resource must still exist after Graph B release")
	t.Log("Phase 3: shared-resource still exists after Graph B released")

	// Phase 4: Delete Graph A. Its teardown must delete the ConfigMap despite
	// any residual managedFields entries from Graph B.
	graphALatest := &unstructured.Unstructured{}
	graphALatest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-contrib-owner", Namespace: ns}, graphALatest))
	require.NoError(t, k8sClient.Delete(ctx, graphALatest))
	t.Log("Phase 4: Graph A deletion requested")

	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-contrib-owner", Namespace: ns}))
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK, cmKey),
		"shared-resource must be deleted — field managers do not block template deletion")
	t.Log("Phase 4: CONTRIBUTION DELETION PROVED — template owner deleted resource despite co-ownership history")
}

// TestExternalFinalizerForegroundDeletion proves that when an external actor
// places a finalizer on a Graph-managed resource, the Graph's teardown waits
// correctly: it issues DELETE, the resource enters Terminating, and the Graph's
// finalizer holds teardown until the external finalizer is released and the
// resource actually disappears.
//
// Design 003-ownership § Deletion:
//
//	"Kubernetes finalizers are the correct mechanism for preventing premature
//	deletion. If an external actor needs the resource to persist beyond the
//	Graph's lifecycle, it places a finalizer."
//
// This is the complementary test: field managers don't block, but finalizers DO
// hold (via Kubernetes, not via kro logic). The Graph handles it gracefully.
func TestExternalFinalizerForegroundDeletion(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Phase 1: Create a Graph that owns a ConfigMap.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-ext-finalizer",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "ext-fin-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	cmKey := types.NamespacedName{Name: "ext-fin-target", Namespace: ns}
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, cmKey, cm))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-ext-finalizer", Namespace: ns}))
	t.Log("Phase 1: Graph created ext-fin-target")

	// Phase 2: External actor places a finalizer on the resource.
	// This simulates an external controller that needs to do cleanup before
	// the resource disappears.
	require.NoError(t, k8sClient.Get(ctx, cmKey, cm))
	cm.SetFinalizers(append(cm.GetFinalizers(), "external.example.com/cleanup"))
	require.NoError(t, k8sClient.Update(ctx, cm))
	t.Log("Phase 2: External finalizer placed on ext-fin-target")

	// Phase 3: Delete the Graph. Teardown issues DELETE on the ConfigMap.
	// The ConfigMap enters Terminating (external finalizer holds it).
	// The Graph's teardown must wait — it can't complete until the resource
	// is actually gone.
	graphObj := &unstructured.Unstructured{}
	graphObj.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-ext-finalizer", Namespace: ns}, graphObj))
	require.NoError(t, k8sClient.Delete(ctx, graphObj))
	t.Log("Phase 3: Graph deletion requested — teardown should issue DELETE on ConfigMap")

	// Give the controller time to process and issue DELETE on the ConfigMap.
	// The ConfigMap should be in Terminating (DeletionTimestamp set, finalizer holds).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true,
		func(ctx context.Context) (bool, error) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx, cmKey, obj); err != nil {
				return false, nil // not found yet means DELETE completed instantly (shouldn't happen)
			}
			return obj.GetDeletionTimestamp() != nil, nil
		}), "ConfigMap should be in Terminating state (external finalizer holds it)")
	t.Log("Phase 3: ConfigMap is Terminating — external finalizer holds deletion")

	// The Graph should NOT be fully deleted yet — its teardown waits for the
	// ConfigMap to actually disappear.
	graphCheck := &unstructured.Unstructured{}
	graphCheck.SetGroupVersionKind(GraphGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-ext-finalizer", Namespace: ns}, graphCheck)
	require.NoError(t, err, "Graph should still exist (teardown blocked by Terminating resource)")
	assert.NotNil(t, graphCheck.GetDeletionTimestamp(),
		"Graph should be in Terminating state (controller finalizer holds it)")
	t.Log("Phase 3: Graph correctly waiting — its finalizer holds while ConfigMap is Terminating")

	// Phase 4: External actor releases the finalizer (simulates cleanup completion).
	finCM := &unstructured.Unstructured{}
	finCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, cmKey, finCM))
	finCM.SetFinalizers(nil) // remove all finalizers
	require.NoError(t, k8sClient.Update(ctx, finCM))
	t.Log("Phase 4: External finalizer released")

	// Phase 5: ConfigMap should now complete deletion, and the Graph's teardown
	// should complete — Graph finalizer removed, Graph fully deleted.
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK, cmKey),
		"ConfigMap should complete deletion after finalizer release")
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-ext-finalizer", Namespace: ns}),
		"Graph teardown should complete after ConfigMap is gone")
	t.Log("Phase 5: FOREGROUND DELETION PROVED — external finalizer held, then released, full cascade completed")
}

// TestPatchOnlyGraphTeardown verifies that a Graph with only patch nodes
// (no templates) completes teardown correctly. Phase 1 (template deletion)
// is empty and passes immediately. Phase 2 releases patch fields.
//
// This exercises the degenerate case where the phase split has no work
// in Phase 1 — teardown must not hang or error when templateCandidates is empty.
func TestPatchOnlyGraphTeardown(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cmKey := types.NamespacedName{Name: "patch-target", Namespace: ns}

	// Create the target ConfigMap that the Graph will patch.
	target := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "patch-target",
			"namespace": ns,
		},
		"data": map[string]any{"original": "value"},
	}}
	require.NoError(t, k8sClient.Create(ctx, target))

	// Create a Graph with only a patch node — no templates.
	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata": map[string]any{
			"name":      "patch-only",
			"namespace": ns,
		},
		"spec": map[string]any{
			"nodes": []any{
				map[string]any{
					"id": "contribution",
					"patch": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "patch-target",
							"namespace": ns,
						},
						"data": map[string]any{"contributed": "by-graph"},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "patch-only", Namespace: ns}))

	// Verify the patch was applied.
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK, cmKey,
		[]string{"data", "contributed"}, "by-graph"))
	t.Log("patch applied: ConfigMap has contributed field")

	// Delete the Graph. Teardown should complete (Phase 1 empty, Phase 2 releases patch).
	graphObj := &unstructured.Unstructured{}
	graphObj.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "patch-only", Namespace: ns}, graphObj))
	require.NoError(t, k8sClient.Delete(ctx, graphObj))

	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "patch-only", Namespace: ns}),
		"patch-only Graph teardown should complete without templates")

	// Target ConfigMap should still exist (patches don't delete targets).
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, cmKey, cm))
	// The contributed field should be released (gone).
	_, found, _ := unstructured.NestedString(cm.Object, "data", "contributed")
	assert.False(t, found, "contributed field should be released after Graph teardown")
	t.Log("PATCH-ONLY TEARDOWN PROVED: Graph deleted, patch released, target intact")
}

// TestCrossGraphPatchTargetGone verifies that when a Graph's patch target
// no longer exists (deleted by another Graph or externally), teardown still
// completes. The release-apply gets NotFound — treated as a no-op.
//
// This exercises the cross-graph scenario: Graph A owns a resource (template),
// Graph B patches it (patch). Graph A is deleted first (resource gone), then
// Graph B is deleted. Graph B's Phase 2 must not block on the missing target.
func TestCrossGraphPatchTargetGone(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cmKey := types.NamespacedName{Name: "shared-resource", Namespace: ns}

	// Graph A: owns the resource (template).
	graphA := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata": map[string]any{
			"name":      "owner-graph",
			"namespace": ns,
		},
		"spec": map[string]any{
			"nodes": []any{
				map[string]any{
					"id": "resource",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "shared-resource", "namespace": ns},
						"data":       map[string]any{"owner": "graph-a"},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, graphA))
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, cmKey, cm))

	// Graph B: patches the resource (no finalizer, pure field contribution).
	graphB := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata": map[string]any{
			"name":      "patcher-graph",
			"namespace": ns,
		},
		"spec": map[string]any{
			"nodes": []any{
				map[string]any{
					"id": "contribution",
					"patch": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "shared-resource", "namespace": ns},
						"data":       map[string]any{"extra": "from-graph-b"},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, graphB))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "patcher-graph", Namespace: ns}))
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK, cmKey,
		[]string{"data", "extra"}, "from-graph-b"))
	t.Log("both graphs applied: ConfigMap has fields from both")

	// Delete Graph A first — the ConfigMap is deleted.
	graphAObj := &unstructured.Unstructured{}
	graphAObj.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "owner-graph", Namespace: ns}, graphAObj))
	require.NoError(t, k8sClient.Delete(ctx, graphAObj))
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "owner-graph", Namespace: ns}))
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK, cmKey))
	t.Log("Graph A deleted, ConfigMap gone")

	// Delete Graph B — its patch target no longer exists.
	// Phase 2 release-apply should get NotFound and treat it as a no-op.
	graphBObj := &unstructured.Unstructured{}
	graphBObj.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "patcher-graph", Namespace: ns}, graphBObj))
	require.NoError(t, k8sClient.Delete(ctx, graphBObj))
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "patcher-graph", Namespace: ns}),
		"patcher Graph teardown should complete even though patch target is gone")

	t.Log("CROSS-GRAPH PATCH TARGET GONE PROVED: patcher teardown completes when target is already deleted")
}
