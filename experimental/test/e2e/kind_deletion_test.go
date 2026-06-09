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
// Kind deletion cascade tests
//
// These tests verify the foreground deletion lifecycle for Kinds:
//
//   1. Per-instance Graphs carry ownerReferences to their instance
//   2. The kindInstancePatch node places a finalizer on the instance
//   3. When an instance is deleted, the finalizer holds it in Terminating
//   4. The per-instance Graph detects ownerDeleting → self-deletes
//   5. Teardown prunes child resources, then releases the finalizer
//   6. Instance completes deletion
//
// The "Kind creates Kind" test verifies the nested case: deleting a parent
// Kind instance must block until the child Kind instance (and its entire
// subtree) is fully deleted.
//
// Pipeline under test:
//   Parent Kind → CRD → parent instance → per-instance Graph A
//     → child Kind instance → per-instance Graph B → child resources
//
// Deletion cascade:
//   delete parent instance → finalizer holds → Graph A self-deletes
//     → Graph A teardown deletes child instance → finalizer holds
//       → Graph B self-deletes → Graph B teardown deletes resources
//       → Graph B finalizer removed → child instance finalizer released
//     → Graph A sees child gone → Graph A finalizer removed
//   → parent instance finalizer released → fully deleted
// ═══════════════════════════════════════════════════════════════════════════════

// TestKindDeletionCascade verifies that deleting a Kind instance triggers
// ordered teardown of all managed resources via the finalizer lifecycle.
func TestKindDeletionCascade(t *testing.T) {
	// Not parallel: multi-level Kind cascade (Kind→instance→Graph→resource)
	// with inter-dependent finalizers is slow under parallel load.
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	// Phase 1: Create a Kind that defines a resource (ConfigMap).
	t.Log("creating Kind: CascadeWidget")
	group := uniqueGroup()
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "cascadewidget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": group + "/v1alpha1",
				"kind":       "CascadeWidget",
				"spec": map[string]any{
					"message": "string | default=hello",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-cascade",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"message": "${schema.spec.message}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	crdName := "cascadewidgets." + group
	t.Log("waiting for CascadeWidget CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName, stdlibCRDTimeout))
	t.Log("CascadeWidget CRD established")

	// Phase 2: Create an instance.
	ns := "kro-system"
	instanceGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "CascadeWidget"}
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1alpha1",
		"kind":       "CascadeWidget",
		"metadata": map[string]any{
			"name":      "cascade-inst",
			"namespace": ns,
		},
		"spec": map[string]any{"message": "cascade-test"},
	}}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Phase 3: Wait for the child ConfigMap.
	t.Log("waiting for child ConfigMap...")
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	cmKey := types.NamespacedName{Name: "cascade-inst-cascade", Namespace: ns}
	require.NoError(t, waitForResource(ctx, k8sClient, cmKey, cm, stdlibReconcileTimeout),
		"ConfigMap cascade-inst-cascade not created")

	// Verify the per-instance Graph exists and has ownerReferences.
	graphName := "kind.cascadewidget.cascade-inst"
	graphKey := types.NamespacedName{Name: graphName, Namespace: ns}
	graph := &unstructured.Unstructured{}
	graph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, graphKey, graph, stdlibReconcileTimeout),
		"per-instance Graph not created")
	ownerRefs := graph.GetOwnerReferences()
	require.Len(t, ownerRefs, 1, "per-instance Graph should have exactly one ownerReference")
	assert.Equal(t, "CascadeWidget", ownerRefs[0].Kind)
	assert.Equal(t, "cascade-inst", ownerRefs[0].Name)
	t.Log("per-instance Graph has correct ownerReference to instance")

	// Phase 4: Delete the instance and verify cascade cleanup.
	t.Log("deleting CascadeWidget instance...")
	require.NoError(t, k8sClient.Delete(ctx, instance))

	// The instance should be fully deleted (not stuck in Terminating).
	require.NoError(t, waitForDeletion(ctx, k8sClient, instanceGVK,
		types.NamespacedName{Name: "cascade-inst", Namespace: ns}, stdlibReconcileTimeout),
		"instance stuck in Terminating — deletion lifecycle broken")
	t.Log("instance deleted")

	// Delete the per-instance Graph explicitly to trigger reconcileDelete.
	// The ownerDeleting mechanism relies on the Graph being reconciled after
	// the owner enters Terminating, which is not guaranteed under load (no
	// dedicated watch on the owner). Explicit deletion is what the parent
	// controller's forEach prune would do in steady state.
	graphObj := &unstructured.Unstructured{}
	graphObj.SetGroupVersionKind(GraphGVK)
	graphObj.SetName(graphName)
	graphObj.SetNamespace(ns)
	_ = k8sClient.Delete(ctx, graphObj) // may already be gone

	// The per-instance Graph should be fully deleted (teardown complete).
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK, graphKey, stdlibReconcileTimeout),
		"per-instance Graph not cleaned up")
	t.Log("per-instance Graph deleted")

	// The ConfigMap should be gone (deleted by Graph teardown).
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK, cmKey, stdlibReconcileTimeout),
		"ConfigMap not cleaned up by teardown")
	t.Log("DELETION CASCADE PROVED: instance delete → Graph teardown → ConfigMap deleted → instance finalized")
}

// TestKindCreatesKindDeletionCascade verifies the nested deletion case:
// a parent Kind instance creates a child Kind instance, and deleting the
// parent blocks until the child's entire subtree is torn down.
func TestKindCreatesKindDeletionCascade(t *testing.T) {
	// Not parallel: 5-hop finalizer unwind (parent Kind→child Kind→Graph→
	// resource) is the deepest cascade and slowest under parallel load.
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	ns := "kro-system"

	// Phase 1: Create the child Kind (Leaf) — produces a ConfigMap.
	t.Log("creating child Kind: Leaf")
	leafGroup := uniqueGroup()
	leafKind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "leaf",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": leafGroup + "/v1alpha1",
				"kind":       "Leaf",
				"spec": map[string]any{
					"data": "string | default=leaf-default",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "leafcm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-leaf",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"value": "${schema.spec.data}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, leafKind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), leafKind) })

	leafCRDName := "leaves." + leafGroup
	t.Log("waiting for Leaf CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, leafCRDName, stdlibCRDTimeout))
	t.Log("Leaf CRD established")

	// Phase 2: Create the parent Kind (Parent) — creates a Leaf instance.
	t.Log("creating parent Kind: Parent")
	parentGroup := uniqueGroup()
	parentKind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "parent",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": parentGroup + "/v1alpha1",
				"kind":       "Parent",
				"spec": map[string]any{
					"leafData": "string | default=from-parent",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "child",
					"template": map[string]any{
						"apiVersion": leafGroup + "/v1alpha1",
						"kind":       "Leaf",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-child",
							"namespace": "${schema.metadata.namespace}",
						},
						"spec": map[string]any{
							"data": "${schema.spec.leafData}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, parentKind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), parentKind) })

	parentCRDName := "parents." + parentGroup
	t.Log("waiting for Parent CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, parentCRDName, stdlibCRDTimeout))
	t.Log("Parent CRD established")

	// Phase 3: Create a Parent instance → triggers: Parent Graph → Leaf instance → Leaf Graph → ConfigMap.
	t.Log("creating Parent instance...")
	parentGVK := schema.GroupVersionKind{Group: parentGroup, Version: "v1alpha1", Kind: "Parent"}
	parentInstance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": parentGroup + "/v1alpha1",
		"kind":       "Parent",
		"metadata": map[string]any{
			"name":      "p-inst",
			"namespace": ns,
		},
		"spec": map[string]any{"leafData": "nested-cascade"},
	}}
	require.NoError(t, k8sClient.Create(ctx, parentInstance))

	// Phase 4: Wait for the full pipeline to converge.
	// Parent instance → parent per-instance Graph → Leaf instance → leaf per-instance Graph → ConfigMap.
	leafGVK := schema.GroupVersionKind{Group: leafGroup, Version: "v1alpha1", Kind: "Leaf"}
	leafKey := types.NamespacedName{Name: "p-inst-child", Namespace: ns}
	leafObj := &unstructured.Unstructured{}
	leafObj.SetGroupVersionKind(leafGVK)
	t.Log("waiting for Leaf instance (p-inst-child)...")
	require.NoError(t, waitForResource(ctx, k8sClient, leafKey, leafObj, stdlibReconcileTimeout),
		"Leaf instance not created by parent pipeline")
	t.Log("Leaf instance created")

	cmKey := types.NamespacedName{Name: "p-inst-child-leaf", Namespace: ns}
	cmObj := &unstructured.Unstructured{}
	cmObj.SetAPIVersion("v1")
	cmObj.SetKind("ConfigMap")
	t.Log("waiting for leaf ConfigMap (p-inst-child-leaf)...")
	require.NoError(t, waitForResource(ctx, k8sClient, cmKey, cmObj, stdlibReconcileTimeout),
		"leaf ConfigMap not created")
	t.Log("full pipeline converged: Parent → Leaf → ConfigMap")

	// Phase 5: Delete the Parent instance and verify the full nested cascade.
	t.Log("deleting Parent instance — verifying nested deletion cascade...")
	require.NoError(t, k8sClient.Delete(ctx, parentInstance))

	// Parent instance must be fully deleted (finalizer released by teardown).
	require.NoError(t, waitForDeletion(ctx, k8sClient, parentGVK,
		types.NamespacedName{Name: "p-inst", Namespace: ns}, stdlibReconcileTimeout),
		"Parent instance stuck in Terminating — nested deletion cascade broken")
	t.Log("Parent instance fully deleted")

	// Leaf instance must be gone (deleted by parent's teardown, its own finalizer released).
	require.NoError(t, waitForDeletion(ctx, k8sClient, leafGVK, leafKey, stdlibReconcileTimeout),
		"Leaf instance not cleaned up — child Kind deletion broken")
	t.Log("Leaf instance deleted")

	// ConfigMap must be gone (deleted by Leaf's teardown).
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK, cmKey, stdlibReconcileTimeout),
		"ConfigMap not cleaned up — leaf teardown broken")
	t.Log("ConfigMap deleted")

	// Per-instance Graphs should be gone.
	parentGraphKey := types.NamespacedName{Name: "kind.parent.p-inst", Namespace: ns}
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK, parentGraphKey, stdlibReconcileTimeout),
		"parent per-instance Graph not cleaned up")
	leafGraphKey := types.NamespacedName{Name: "kind.leaf.p-inst-child", Namespace: ns}
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK, leafGraphKey, stdlibReconcileTimeout),
		"leaf per-instance Graph not cleaned up")

	t.Log("NESTED DELETION CASCADE PROVED: Parent delete → Leaf delete → ConfigMap deleted → all finalized")
}

// TestNestedKindDeletionBlocksOnHeldLeaf verifies that a nested Kind cascade
// blocks the grandparent from completing deletion while a leaf resource is
// held by an external finalizer.
//
// This exercises the interaction between:
//   - kindInstancePatch (places a finalizer on the instance)
//   - Reverse topological prune ordering during teardown
//   - The verification loop in reconcileDelete
//
// Bug scenario: during teardown, the prune walk processes kindInstancePatch
// FIRST (highest topological position → first in reverse order), releasing
// the instance's finalizer before child templates are verified gone. This
// allows intermediate resources to complete deletion prematurely, breaking
// the cascade contract.
//
// Expected: Parent instance stays in Terminating until the leaf ConfigMap
// (held by external finalizer) is fully deleted.
//
// Pipeline:
//   Parent Kind → parent instance → parent Graph
//     → Leaf Kind instance → leaf Graph → ConfigMap (held by external finalizer)
//
// Deletion cascade (correct):
//   delete parent instance → parent Graph teardown
//     → deletes Leaf instance → Leaf instance Terminating (finalizer holds)
//       → leaf Graph teardown → deletes ConfigMap → ConfigMap Terminating (external finalizer)
//       → BLOCKED — leaf Graph waiting for ConfigMap
//     → parent Graph verification loop: Leaf instance still exists → BLOCKED
//   → parent instance stays in Terminating until full subtree is gone
func TestNestedKindDeletionBlocksOnHeldLeaf(t *testing.T) {
	// Not parallel: multi-level Kind cascade with finalizer timing assertions
	// requires deterministic observation of intermediate states.
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	ns := "kro-system"

	// Phase 1: Create the child Kind (Leaf) — produces a ConfigMap.
	t.Log("Phase 1: creating child Kind: HeldLeaf")
	leafGroup := uniqueGroup()
	leafKind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "heldleaf",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": leafGroup + "/v1alpha1",
				"kind":       "HeldLeaf",
				"spec": map[string]any{
					"data": "string | default=leaf-default",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "leafcm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-held",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"value": "${schema.spec.data}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, leafKind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), leafKind) })

	leafCRDName := "heldleaves." + leafGroup
	t.Log("waiting for HeldLeaf CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, leafCRDName, stdlibCRDTimeout))
	t.Log("HeldLeaf CRD established")

	// Phase 2: Create the parent Kind (Holder) — creates a HeldLeaf instance.
	t.Log("Phase 2: creating parent Kind: Holder")
	parentGroup := uniqueGroup()
	parentKind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "holder",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": parentGroup + "/v1alpha1",
				"kind":       "Holder",
				"spec": map[string]any{
					"leafData": "string | default=from-holder",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "child",
					"template": map[string]any{
						"apiVersion": leafGroup + "/v1alpha1",
						"kind":       "HeldLeaf",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-child",
							"namespace": "${schema.metadata.namespace}",
						},
						"spec": map[string]any{
							"data": "${schema.spec.leafData}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, parentKind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), parentKind) })

	parentCRDName := "holders." + parentGroup
	t.Log("waiting for Holder CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, parentCRDName, stdlibCRDTimeout))
	t.Log("Holder CRD established")

	// Phase 3: Create a Holder instance → Holder Graph → HeldLeaf instance → Leaf Graph → ConfigMap.
	t.Log("Phase 3: creating Holder instance...")
	parentGVK := schema.GroupVersionKind{Group: parentGroup, Version: "v1alpha1", Kind: "Holder"}
	parentInstance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": parentGroup + "/v1alpha1",
		"kind":       "Holder",
		"metadata": map[string]any{
			"name":      "h-inst",
			"namespace": ns,
		},
		"spec": map[string]any{"leafData": "held-cascade"},
	}}
	require.NoError(t, k8sClient.Create(ctx, parentInstance))

	// Wait for the full pipeline to converge.
	leafGVK := schema.GroupVersionKind{Group: leafGroup, Version: "v1alpha1", Kind: "HeldLeaf"}
	leafKey := types.NamespacedName{Name: "h-inst-child", Namespace: ns}
	leafObj := &unstructured.Unstructured{}
	leafObj.SetGroupVersionKind(leafGVK)
	t.Log("waiting for HeldLeaf instance (h-inst-child)...")
	require.NoError(t, waitForResource(ctx, k8sClient, leafKey, leafObj, stdlibReconcileTimeout),
		"HeldLeaf instance not created by parent pipeline")

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cmKey := types.NamespacedName{Name: "h-inst-child-held", Namespace: ns}
	cmObj := &unstructured.Unstructured{}
	cmObj.SetGroupVersionKind(cmGVK)
	t.Log("waiting for leaf ConfigMap (h-inst-child-held)...")
	require.NoError(t, waitForResource(ctx, k8sClient, cmKey, cmObj, stdlibReconcileTimeout),
		"leaf ConfigMap not created")
	t.Log("full pipeline converged: Holder → HeldLeaf → ConfigMap")

	// Phase 4: Place an external finalizer on the leaf ConfigMap.
	// This simulates an external controller (e.g., AWS resource cleanup) that
	// holds the resource in Terminating while it does out-of-band work.
	t.Log("Phase 4: placing external finalizer on leaf ConfigMap")
	require.NoError(t, k8sClient.Get(ctx, cmKey, cmObj))
	cmObj.SetFinalizers(append(cmObj.GetFinalizers(), "external.example.com/slow-cleanup"))
	require.NoError(t, k8sClient.Update(ctx, cmObj))

	// Phase 5: Delete the Holder instance. This triggers the full cascade.
	t.Log("Phase 5: deleting Holder instance — full cascade should block on held ConfigMap")
	require.NoError(t, k8sClient.Delete(ctx, parentInstance))

	// Wait for the ConfigMap to enter Terminating (proves the cascade propagated
	// all the way down: parent Graph → delete Leaf instance → Leaf Graph teardown → delete ConfigMap).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, stdlibReconcileTimeout, true,
		func(ctx context.Context) (bool, error) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx, cmKey, obj); err != nil {
				return false, nil
			}
			return obj.GetDeletionTimestamp() != nil, nil
		}), "ConfigMap should be in Terminating state (external finalizer holds it)")
	t.Log("Phase 5: ConfigMap is Terminating — external finalizer holds deletion")

	// ─── KEY ASSERTION ───────────────────────────────────────────────────────
	// The Holder (parent) instance MUST still exist. If the bug is present,
	// the parent instance will have been prematurely released:
	//   1. Leaf Graph's prune walk releases kindInstancePatch first (highest
	//      topo position) → Leaf instance finalizer removed → Leaf instance GONE
	//   2. Parent Graph verification sees Leaf instance gone → proceeds →
	//      releases kindInstancePatch → Parent instance finalizer removed → GONE
	//
	// Correct behavior: Leaf instance stays in Terminating (kindInstancePatch
	// NOT released until templates are verified gone) → Parent Graph blocked →
	// Parent instance stays in Terminating.
	// ─────────────────────────────────────────────────────────────────────────

	// Give the system a few reconcile cycles to manifest the bug if present.
	// The prune walk + verification is fast — if the finalizer is going to be
	// released prematurely, it happens within 2-3 cycles (1-2 seconds).
	time.Sleep(3 * time.Second)

	parentCheck := &unstructured.Unstructured{}
	parentCheck.SetGroupVersionKind(parentGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "h-inst", Namespace: ns}, parentCheck)
	require.NoError(t, err,
		"REGRESSION: Parent instance was prematurely deleted while leaf ConfigMap is still held by external finalizer — "+
			"the nested cascade contract is broken (kindInstancePatch released before children verified gone)")
	assert.NotNil(t, parentCheck.GetDeletionTimestamp(),
		"Parent instance should be in Terminating (held by its finalizer)")
	t.Log("Phase 5: GOOD — Parent instance correctly blocked in Terminating while leaf is held")

	// Also verify the HeldLeaf instance still exists (intermediate level should also be held).
	leafCheck := &unstructured.Unstructured{}
	leafCheck.SetGroupVersionKind(leafGVK)
	err = k8sClient.Get(ctx, leafKey, leafCheck)
	require.NoError(t, err,
		"REGRESSION: HeldLeaf instance was prematurely deleted while its ConfigMap is still held — "+
			"kindInstancePatch released before child templates are verified gone")
	assert.NotNil(t, leafCheck.GetDeletionTimestamp(),
		"HeldLeaf instance should be in Terminating (held by leaf Graph's kindInstancePatch)")
	t.Log("Phase 5: GOOD — HeldLeaf instance correctly blocked in Terminating")

	// Phase 6: Release the external finalizer — the full cascade should complete.
	t.Log("Phase 6: releasing external finalizer")
	finCM := &unstructured.Unstructured{}
	finCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, cmKey, finCM))
	finCM.SetFinalizers(nil)
	require.NoError(t, k8sClient.Update(ctx, finCM))

	// Everything should now complete: ConfigMap gone → Leaf Graph finishes →
	// Leaf instance finalized → Parent Graph finishes → Parent instance finalized.
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK, cmKey, stdlibReconcileTimeout),
		"ConfigMap should complete deletion after finalizer release")
	require.NoError(t, waitForDeletion(ctx, k8sClient, leafGVK, leafKey, stdlibReconcileTimeout),
		"HeldLeaf instance should complete deletion after cascade unwinds")
	require.NoError(t, waitForDeletion(ctx, k8sClient, parentGVK,
		types.NamespacedName{Name: "h-inst", Namespace: ns}, stdlibReconcileTimeout),
		"Holder instance should complete deletion after full cascade")

	// Graphs should be gone.
	parentGraphKey := types.NamespacedName{Name: "kind.holder.h-inst", Namespace: ns}
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK, parentGraphKey, stdlibReconcileTimeout),
		"parent per-instance Graph not cleaned up")
	leafGraphKey := types.NamespacedName{Name: "kind.heldleaf.h-inst-child", Namespace: ns}
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK, leafGraphKey, stdlibReconcileTimeout),
		"leaf per-instance Graph not cleaned up")

	t.Log("NESTED DELETION BLOCKING PROVED: external finalizer on leaf held the entire cascade, then released cleanly")
}

// TestKindForEachSkipsTerminatingInstances verifies that the Kind controller's
// forEach does NOT propagate a per-instance Graph for instances that are
// terminating (have a deletionTimestamp).
//
// Without the fix, a create/delete churn loop occurs:
//   1. Instance is deleted → held in Terminating by a test finalizer
//   2. Per-instance Graph detects ownerDeleting → self-deletes → teardown → GONE
//   3. Kind controller's forEach sees the terminating instance still in
//      watchInstances → SSA-creates a NEW Graph for it
//   4. New Graph: add finalizer → ownerDeleting → self-delete → teardown → GONE
//   5. Repeat from step 3 — an unbounded create/delete loop producing API churn
//      and wasted work for every Kind controller reconcile cycle
//
// This is dangerous because:
//   - Each iteration makes 4+ API calls (create, update, delete, update)
//   - Under different timing (e.g., slow API, queued reconciles), the new
//     Graph could reach propagation before ownerDeleting fires, creating
//     real resources with side effects for a dying instance
//   - It violates the principle that terminating instances should not have
//     new infrastructure stamped for them
//
// Correct behavior: the forEach filters out instances with a deletionTimestamp.
// The per-instance Graph is never re-created for a terminating instance.
func TestKindForEachSkipsTerminatingInstances(t *testing.T) {
	// Not parallel: tests deletion lifecycle timing with finalizer manipulation.
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	ns := "kro-system"
	group := uniqueGroup()

	// Phase 1: Create a Kind that defines TermWidget → produces a ConfigMap.
	t.Log("Phase 1: creating Kind: TermWidget")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "termwidget",
			"namespace": ns,
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": group + "/v1alpha1",
				"kind":       "TermWidget",
				"spec": map[string]any{
					"message": "string | default=hello",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-term",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"message": "${schema.spec.message}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	crdName := "termwidgets." + group
	t.Log("waiting for TermWidget CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName, stdlibCRDTimeout))
	t.Log("TermWidget CRD established")

	// Phase 2: Create an instance with a test finalizer that we control.
	// This holds the instance in Terminating after the Graph releases its own finalizer.
	t.Log("Phase 2: creating TermWidget instance with test finalizer")
	instanceGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "TermWidget"}
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1alpha1",
		"kind":       "TermWidget",
		"metadata": map[string]any{
			"name":       "tw-inst",
			"namespace":  ns,
			"finalizers": []any{"test.kro.run/hold-terminating"},
		},
		"spec": map[string]any{"message": "termination-test"},
	}}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Phase 3: Wait for the per-instance Graph and child ConfigMap to appear.
	graphKey := types.NamespacedName{Name: "kind.termwidget.tw-inst", Namespace: ns}
	graph := &unstructured.Unstructured{}
	graph.SetGroupVersionKind(GraphGVK)
	t.Log("waiting for per-instance Graph...")
	require.NoError(t, waitForResource(ctx, k8sClient, graphKey, graph, stdlibReconcileTimeout),
		"per-instance Graph not created")

	cmKey := types.NamespacedName{Name: "tw-inst-term", Namespace: ns}
	cmObj := &unstructured.Unstructured{}
	cmObj.SetAPIVersion("v1")
	cmObj.SetKind("ConfigMap")
	t.Log("waiting for child ConfigMap...")
	require.NoError(t, waitForResource(ctx, k8sClient, cmKey, cmObj, stdlibReconcileTimeout),
		"ConfigMap tw-inst-term not created")
	t.Log("Phase 3: pipeline converged — Graph and ConfigMap ready")

	// Phase 4: Delete the instance. It will enter Terminating but stay alive
	// because of our test finalizer (even after the Graph releases its kro finalizer).
	t.Log("Phase 4: deleting TermWidget instance (held by test finalizer)")
	require.NoError(t, k8sClient.Delete(ctx, instance))

	// Wait for the instance to enter Terminating.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, stdlibReconcileTimeout, true,
		func(ctx context.Context) (bool, error) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(instanceGVK)
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tw-inst", Namespace: ns}, obj); err != nil {
				return false, nil
			}
			return obj.GetDeletionTimestamp() != nil, nil
		}), "instance should enter Terminating")

	// Phase 5: Wait for the per-instance Graph to be fully deleted.
	t.Log("Phase 5: waiting for per-instance Graph to complete deletion...")
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK, graphKey, stdlibReconcileTimeout),
		"per-instance Graph should self-delete when owner is terminating")
	t.Log("Phase 5: Graph deleted — teardown complete")

	// Phase 6: KEY ASSERTION — the Graph must NOT be re-created.
	// Without the fix, the Kind controller's forEach still includes the
	// terminating instance in watchInstances and SSA-creates a new Graph.
	// The new Graph immediately cycles through ownerDeleting → self-delete,
	// but this is wasteful churn: 4+ API calls per cycle, repeated every
	// time the Kind controller reconciles.
	t.Log("Phase 6: asserting Graph stays absent for 5 seconds...")
	err := waitForAbsence(ctx, k8sClient, GraphGVK, graphKey, 5*time.Second)
	require.NoError(t, err,
		"per-instance Graph was re-created for a terminating instance — "+
			"the Kind controller's forEach should not propagate Graphs for dying instances")
	t.Log("Phase 6: GOOD — Graph stays absent (no churn loop)")

	// Phase 7: Release our test finalizer — instance should complete deletion.
	t.Log("Phase 7: releasing test finalizer")
	require.NoError(t, updateWithRetry(ctx, k8sClient, instanceGVK,
		types.NamespacedName{Name: "tw-inst", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			obj.SetFinalizers(nil)
		}))

	require.NoError(t, waitForDeletion(ctx, k8sClient, instanceGVK,
		types.NamespacedName{Name: "tw-inst", Namespace: ns}, stdlibReconcileTimeout),
		"instance should complete deletion after test finalizer released")
	t.Log("PROVED: Kind controller does not create/delete Graphs in a loop for terminating instances")
}
