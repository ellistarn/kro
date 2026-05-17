package graphcontroller_test

import (
	"context"
	"fmt"
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
// Observed state (self-reference) integration tests
//
// Before evaluating a node's template expressions, kro GETs the target
// resource from the API server and puts it in scope under the node's ID.
// This enables self-reference — a node reading its own existing state
// (status, conditions, generation) to compute new values.
// ═══════════════════════════════════════════════════════════════════════════════

// TestObservedState_SelfReferenceFirstCreate proves that on first create the
// observed state is an empty map (resource doesn't exist yet), so .orValue()
// provides defaults. After the first apply, the next reconcile GETs the live
// object and the self-reference reflects server-set fields like generation.
func TestObservedState_SelfReferenceFirstCreate(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD with status subresource.
	group := uniqueGroup()
	crdName := fmt.Sprintf("selfrefwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "SelfRefWidget", "selfrefwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "SelfRefWidget"}

	// Graph: one template node that self-references its own generation.
	// On first create, myresource is an empty map → orValue(0) provides default.
	// After the resource exists (generation=1), the next reconcile reads it.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-selfref-first-create",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "myresource",
						"template": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "SelfRefWidget",
							"metadata": map[string]any{
								"name": "selfref-instance",
							},
							"spec": map[string]any{
								"value": "hello",
							},
							"status": map[string]any{
								"generation": "${string(myresource.?metadata.?generation.orValue(0))}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Wait for graph ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-selfref-first-create", Namespace: ns}))

	// Wait for the instance to exist and its status.generation to converge.
	require.NoError(t, waitForField(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "selfref-instance", Namespace: ns},
		[]string{"status", "generation"}, "1", 15*time.Second))

	// Read the instance and assert status.generation >= 1.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "selfref-instance", Namespace: ns}, obj))

	// spec.value should be "hello"
	val, found, _ := unstructured.NestedString(obj.Object, "spec", "value")
	require.True(t, found, "spec.value should exist")
	assert.Equal(t, "hello", val)

	// status.generation should be "1" (server-assigned after first create, now a string via CEL)
	gen, found2, _ := unstructured.NestedString(obj.Object, "status", "generation")
	require.True(t, found2, "status.generation should exist")
	assert.Equal(t, "1", gen,
		"status.generation should be '1' after self-reference reads live object")
	t.Logf("Self-reference first create proved: status.generation=%s", gen)
}

// TestObservedState_SelfReferenceConditionLifecycle proves that a single
// template node can write and preserve its own status conditions using the
// self-reference pattern — reading its own existing conditions to decide
// whether to update lastTransitionTime.
func TestObservedState_SelfReferenceConditionLifecycle(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD with status subresource.
	group := uniqueGroup()
	crdName := fmt.Sprintf("condlifecycles.%s", group)
	crd := buildCustomCRD(crdName, group, "CondLifecycle", "condlifecycles")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "CondLifecycle"}

	// Create a control ConfigMap that drives the ready signal.
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "lifecycle-control",
				"namespace": ns,
			},
			"data": map[string]any{
				"ready": "false",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	// Graph with:
	// - ref node "control" watching the ConfigMap
	// - template node "myapp" that self-references to write conditions
	//
	// The condition expression:
	// - Reads control.data.ready to determine status
	// - Reads myapp's own existing conditions to preserve lastTransitionTime
	//   when the status hasn't changed
	condExpr := `${[{"type": "Ready", "status": control.data.ready == "true" ? "True" : "False", "observedGeneration": myapp.?metadata.?generation.orValue(0), "lastTransitionTime": myapp.?status.?conditions.orValue([]).exists(c, c.type == "Ready") && myapp.status.conditions.filter(c, c.type == "Ready")[0].status == (control.data.ready == "true" ? "True" : "False") ? myapp.status.conditions.filter(c, c.type == "Ready")[0].lastTransitionTime : string(time.now())}]}`

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-selfref-condition-lifecycle",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "lifecycle-control"},
						},
					},
					map[string]any{
						"id": "myapp",
						"template": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "CondLifecycle",
							"metadata": map[string]any{
								"name": "lifecycle-instance",
							},
							"spec": map[string]any{
								"placeholder": "value",
							},
							"status": map[string]any{
								"conditions": condExpr,
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Wait for graph ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-selfref-condition-lifecycle", Namespace: ns}))

	// Wait for the Ready condition to appear with status=False and observedGeneration set.
	require.NoError(t, waitForConditionWithGeneration(ctx, t, k8sClient, crGVK,
		types.NamespacedName{Name: "lifecycle-instance", Namespace: ns}, "Ready", "False", 15*time.Second))

	// Read the instance and extract the Ready condition.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "lifecycle-instance", Namespace: ns}, obj))

	conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	require.True(t, found, "instance should have status.conditions")
	require.NotEmpty(t, conditions)

	cond, ok := findCondition(conditions, "Ready")
	require.True(t, ok, "Ready condition should exist")
	assert.Equal(t, "False", cond["status"], "initial status should be False")

	// observedGeneration should match instance generation
	instanceGen := obj.GetGeneration()
	condGen := extractConditionGeneration(cond)
	assert.Equal(t, instanceGen, condGen,
		"observedGeneration should match metadata.generation")

	// lastTransitionTime should be valid RFC3339
	t1Str, _ := cond["lastTransitionTime"].(string)
	t1, err := time.Parse(time.RFC3339, t1Str)
	require.NoError(t, err, "lastTransitionTime should be valid RFC3339, got: %s", t1Str)
	t.Logf("Phase 1: status=False, lastTransitionTime=%s", t1Str)

	// Sleep to ensure second-boundary difference for RFC3339 comparison.
	time.Sleep(1100 * time.Millisecond)

	// Update the ConfigMap: ready → "true"
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "lifecycle-control", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "true", "data", "ready")
		}))

	// Wait for the condition to become status="True".
	require.NoError(t, waitForConditionStatus(ctx, t, k8sClient, crGVK,
		types.NamespacedName{Name: "lifecycle-instance", Namespace: ns}, "Ready", "True", 15*time.Second))

	// Read the condition again and assert lastTransitionTime is AFTER T1.
	obj2 := &unstructured.Unstructured{}
	obj2.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "lifecycle-instance", Namespace: ns}, obj2))
	conditions2, _, _ := unstructured.NestedSlice(obj2.Object, "status", "conditions")
	cond2, ok := findCondition(conditions2, "Ready")
	require.True(t, ok)
	assert.Equal(t, "True", cond2["status"])
	t2Str, _ := cond2["lastTransitionTime"].(string)
	t2, err := time.Parse(time.RFC3339, t2Str)
	require.NoError(t, err)
	assert.True(t, t2.After(t1),
		"lastTransitionTime must advance on False→True: T1=%s T2=%s", t1Str, t2Str)
	t.Logf("Phase 2: status=True, lastTransitionTime=%s (advanced from %s)", t2Str, t1Str)

	// Sleep to ensure second-boundary difference.
	time.Sleep(1100 * time.Millisecond)

	// Trigger another reconcile by adding a field — status stays "True".
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "lifecycle-control", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "foo", "data", "msg")
		}))

	// Wait for settle (reconcile has processed the change).
	require.NoError(t, waitForSettle(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "lifecycle-instance", Namespace: ns}))

	// Read condition again — lastTransitionTime should be preserved (status unchanged).
	obj3 := &unstructured.Unstructured{}
	obj3.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "lifecycle-instance", Namespace: ns}, obj3))
	conditions3, _, _ := unstructured.NestedSlice(obj3.Object, "status", "conditions")
	cond3, ok := findCondition(conditions3, "Ready")
	require.True(t, ok)
	assert.Equal(t, "True", cond3["status"])
	t3Str, _ := cond3["lastTransitionTime"].(string)
	assert.Equal(t, t2Str, t3Str,
		"lastTransitionTime must be preserved when status is unchanged (True→True): T2=%s T3=%s", t2Str, t3Str)
	t.Logf("Phase 3: status=True (unchanged), lastTransitionTime=%s (preserved)", t3Str)
}

// TestObservedState_DownstreamSeesPostApplyState proves that downstream nodes
// see the post-apply state of upstream nodes (including server-set fields like
// uid and resourceVersion), not a pre-apply or empty-map observed state.
func TestObservedState_DownstreamSeesPostApplyState(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph with two nodes:
	// - upstream: creates a ConfigMap
	// - downstream: references upstream.metadata.uid and upstream.metadata.resourceVersion
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-downstream-postapply",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "upstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "upstream-cm",
							},
							"data": map[string]any{
								"value": "hello",
							},
						},
					},
					map[string]any{
						"id": "downstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "downstream-cm",
							},
							"data": map[string]any{
								"upstreamUid":             "${upstream.metadata.uid}",
								"upstreamResourceVersion": "${upstream.metadata.resourceVersion}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Wait for graph ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-downstream-postapply", Namespace: ns}))

	// Read the downstream ConfigMap.
	downstream := &unstructured.Unstructured{}
	downstream.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "downstream-cm", Namespace: ns}, downstream))

	// Assert upstreamUid is a non-empty UUID string.
	uid, found, _ := unstructured.NestedString(downstream.Object, "data", "upstreamUid")
	require.True(t, found, "data.upstreamUid should exist")
	assert.NotEmpty(t, uid, "downstream should see upstream's server-assigned uid")
	// UIDs are 36 chars (8-4-4-4-12 hex with dashes).
	assert.Len(t, uid, 36, "uid should be a standard UUID format (36 chars)")

	// Assert upstreamResourceVersion is a non-empty string.
	rv, found, _ := unstructured.NestedString(downstream.Object, "data", "upstreamResourceVersion")
	require.True(t, found, "data.upstreamResourceVersion should exist")
	assert.NotEmpty(t, rv, "downstream should see upstream's server-assigned resourceVersion")
	t.Logf("Downstream sees post-apply state: uid=%s, resourceVersion=%s", uid, rv)
}

// TestObservedState_ConditionSugarSelfReference proves that the .condition()
// sugar function works correctly with self-reference — the receiver IS the
// node itself (no separate ref node needed).
func TestObservedState_ConditionSugarSelfReference(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD with status subresource.
	group := uniqueGroup()
	crdName := fmt.Sprintf("sugarwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "SugarWidget", "sugarwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "SugarWidget"}

	// Graph: one template node that uses .condition() on itself.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-condition-sugar-selfref",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "widget",
						"template": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "SugarWidget",
							"metadata": map[string]any{
								"name": "sugar-instance",
							},
							"spec": map[string]any{
								"placeholder": "value",
							},
							"status": map[string]any{
								"conditions": "${[widget.condition('Ready', 'True', 'AllGood', 'Widget is ready')]}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Wait for graph ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-condition-sugar-selfref", Namespace: ns}))

	// Wait for the Ready condition to appear with status=True and observedGeneration set.
	require.NoError(t, waitForConditionWithGeneration(ctx, t, k8sClient, crGVK,
		types.NamespacedName{Name: "sugar-instance", Namespace: ns}, "Ready", "True", 15*time.Second))

	// Read the instance and verify condition fields.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "sugar-instance", Namespace: ns}, obj))

	conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	require.True(t, found, "instance should have status.conditions")
	require.NotEmpty(t, conditions)

	cond, ok := findCondition(conditions, "Ready")
	require.True(t, ok, "Ready condition should exist")
	assert.Equal(t, "Ready", cond["type"])
	assert.Equal(t, "True", cond["status"])
	assert.Equal(t, "AllGood", cond["reason"])
	assert.Equal(t, "Widget is ready", cond["message"])

	// observedGeneration should match instance's metadata.generation.
	instanceGen := obj.GetGeneration()
	condGen := extractConditionGeneration(cond)
	assert.Equal(t, instanceGen, condGen,
		"observedGeneration should match metadata.generation")

	// lastTransitionTime should be valid RFC3339.
	ltt, _ := cond["lastTransitionTime"].(string)
	_, err := time.Parse(time.RFC3339, ltt)
	assert.NoError(t, err, "lastTransitionTime should be valid RFC3339, got: %s", ltt)
	t.Logf(".condition() self-reference proved: type=%s status=%s reason=%s ltt=%s",
		cond["type"], cond["status"], cond["reason"], ltt)
}

// TestObservedState_PatchSelfReference proves that a patch node can
// self-reference (read its own target's existing state) without a separate
// ref node.
func TestObservedState_PatchSelfReference(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD with status subresource.
	group := uniqueGroup()
	crdName := fmt.Sprintf("patchselfwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "PatchSelfWidget", "patchselfwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "PatchSelfWidget"}

	// Pre-create an instance (so the patch target exists).
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "PatchSelfWidget",
			"metadata": map[string]any{
				"name":      "patchself-instance",
				"namespace": ns,
			},
			"spec": map[string]any{
				"placeholder": "value",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Graph: one patch node that reads its own target via self-reference.
	// The expression uses the observed generation to decide the phase.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-patch-selfref",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "mystatus",
						"patch": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "PatchSelfWidget",
							"metadata": map[string]any{
								"name": "patchself-instance",
							},
							"status": map[string]any{
								"phase": "${mystatus.?metadata.?generation.orValue(0) > 0 ? \"Running\" : \"Starting\"}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Wait for graph ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-patch-selfref", Namespace: ns}))

	// Wait for the status to be patched.
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(crGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "patchself-instance", Namespace: ns}, obj); err != nil {
			return false, nil
		}
		phase, found, _ := unstructured.NestedString(obj.Object, "status", "phase")
		if !found {
			return false, nil
		}
		return phase == "Running", nil
	})
	require.NoError(t, err, "status.phase should become 'Running'")

	// Final assertion: read and confirm.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "patchself-instance", Namespace: ns}, obj))

	phase, found, _ := unstructured.NestedString(obj.Object, "status", "phase")
	require.True(t, found, "status.phase should exist")
	assert.Equal(t, "Running", phase,
		"pre-created resource has generation >= 1, so self-reference should resolve to 'Running'")
	t.Logf("Patch self-reference proved: status.phase=%s (generation=%d)", phase, obj.GetGeneration())
}

// ---------------------------------------------------------------------------
// Test-local helpers
// ---------------------------------------------------------------------------

// extractNumericField extracts a numeric field from nested path, handling
// both int64 and float64 (JSON round-trip produces float64).
func extractNumericField(t *testing.T, obj *unstructured.Unstructured, path ...string) int64 {
	t.Helper()
	raw, found, _ := unstructured.NestedFieldNoCopy(obj.Object, path...)
	require.True(t, found, "field %v should exist", path)
	switch v := raw.(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case int:
		return int64(v)
	default:
		t.Fatalf("field %v has unexpected type %T (value: %v)", path, raw, raw)
		return 0
	}
}

// extractConditionGeneration extracts observedGeneration from a condition map,
// handling both int64 and float64.
func extractConditionGeneration(cond map[string]any) int64 {
	switch v := cond["observedGeneration"].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return 0
	}
}

// TestObservedState_DynamicNameSelfReference proves that a node whose name
// is a dynamic CEL expression (resolved from an upstream node) can still
// self-reference its own observed state. This tests that seedObservedState
// correctly evaluates the CEL name expression using upstream scope, GETs the
// resource at that dynamic name, and seeds scope so self-reference resolves
// against the live object.
func TestObservedState_DynamicNameSelfReference(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD with status subresource.
	group := uniqueGroup()
	crdName := fmt.Sprintf("dynwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "DynWidget", "dynwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "DynWidget"}

	// Create an upstream ConfigMap that provides the dynamic name.
	upstream := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "upstream-config",
				"namespace": ns,
			},
			"data": map[string]any{
				"instanceName": "dynamic-widget",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, upstream))

	// Graph:
	// - "config" ref node watching the upstream ConfigMap
	// - "widget" template node with:
	//   - dynamic name from config.data.instanceName
	//   - self-reference reading its own generation via the dynamic name
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-dynname-selfref",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "config",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "upstream-config"},
						},
					},
					map[string]any{
						"id": "widget",
						"template": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "DynWidget",
							"metadata": map[string]any{
								"name": "${config.data.instanceName}",
							},
							"spec": map[string]any{
								"value": "hello",
							},
							"status": map[string]any{
								"observedGen": "${string(widget.?metadata.?generation.orValue(0))}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Wait for graph ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-dynname-selfref", Namespace: ns}))

	// Wait for the dynamically-named resource's self-reference to resolve.
	require.NoError(t, waitForField(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "dynamic-widget", Namespace: ns},
		[]string{"status", "observedGen"}, "1", 15*time.Second))

	// Read the instance by its dynamic name and verify self-reference resolved.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "dynamic-widget", Namespace: ns}, obj))

	// spec.value should be "hello"
	val, found, _ := unstructured.NestedString(obj.Object, "spec", "value")
	require.True(t, found, "spec.value should exist")
	assert.Equal(t, "hello", val)

	// status.observedGen should be "1" or greater — proves the self-reference
	// resolved against the live object retrieved via the dynamic name.
	observedGen, found, _ := unstructured.NestedString(obj.Object, "status", "observedGen")
	require.True(t, found, "status.observedGen should exist")
	assert.NotEqual(t, "0", observedGen,
		"status.observedGen should not be '0' — self-reference should see live generation")
	t.Logf("Dynamic name self-reference proved: name=%s, status.observedGen=%s", obj.GetName(), observedGen)
}

// TestObservedState_StateMachineTransition proves that self-reference enables
// state machine patterns: a node reads its own current status to compute the
// next state. The template transitions from "" → "Initializing" → "Running"
// and settles at "Running".
//
// Cycle 1: GET 404 → empty map → phase="" → writes "Initializing"
// Cycle 2: GET returns phase="Initializing" → writes "Running"
// Cycle 3: GET returns phase="Running" → writes "Running" again → SSA no-op → settles
func TestObservedState_StateMachineTransition(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD with status subresource.
	group := uniqueGroup()
	crdName := fmt.Sprintf("statemachines.%s", group)
	crd := buildCustomCRD(crdName, group, "StateMachine", "statemachines")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "StateMachine"}

	// Graph: one template node implementing a one-way state machine.
	// Expression: if current phase is "", write "Initializing"; otherwise write "Running".
	// This settles at "Running" after 2 reconciles.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-statemachine",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "machine",
						"template": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "StateMachine",
							"metadata": map[string]any{
								"name": "machine-instance",
							},
							"spec": map[string]any{
								"value": "hello",
							},
							"status": map[string]any{
								"phase": `${machine.?status.?phase.orValue("") == "" ? "Initializing" : "Running"}`,
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Wait for graph ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-statemachine", Namespace: ns}))

	// Wait for the status.phase to reach "Running" (the terminal state).
	require.NoError(t, waitForField(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "machine-instance", Namespace: ns},
		[]string{"status", "phase"}, "Running"))

	// Final assertion: read and confirm terminal state.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "machine-instance", Namespace: ns}, obj))

	phase, found, _ := unstructured.NestedString(obj.Object, "status", "phase")
	require.True(t, found, "status.phase should exist")
	assert.Equal(t, "Running", phase,
		"state machine should settle at 'Running' after transitioning through '' → 'Initializing' → 'Running'")
	t.Logf("State machine transition proved: status.phase=%s (settled after self-reference transitions)", phase)
}
