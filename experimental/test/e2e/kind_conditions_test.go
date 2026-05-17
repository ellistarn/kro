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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Kind status conditions tests
//
// The Kind resource's status conditions are hoisted from its underlying
// controller Graph (kind-<name>). The kindConditions node at the top-level
// kind Graph iterates over controller Graphs and forwards their Compiled
// and Ready conditions onto the corresponding Kind resource.
//
// Additionally, the kindCount node inside each controller Graph writes
// status.items with the count of watched instances.
//
// These tests verify:
//   1. Kind gets Compiled=True and Ready=True conditions when healthy
//   2. Kind gets status.items reflecting instance count
//   3. Conditions use ALL CAPS status values (True/False/Unknown)
//   4. The old status.ready boolean field is gone
// ═══════════════════════════════════════════════════════════════════════════════

var kindGVK = schema.GroupVersionKind{
	Group:   "experimental.kro.run",
	Version: "v1alpha1",
	Kind:    "Kind",
}

// TestKindStatusConditions verifies that a Kind resource receives Compiled
// and Ready conditions hoisted from its controller Graph, plus an items count.
func TestKindStatusConditions(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	// Phase 1: Create a Kind that defines CondWidget.
	t.Log("creating Kind: CondWidget")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "condwidget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "CondWidget",
				"spec": map[string]any{
					"label": "string | default=test",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-cond",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"label": "${schema.spec.label}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	// Phase 2: Wait for the CRD to be established.
	t.Log("waiting for CondWidget CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "condwidgets.test.stdlib.kro.run", stdlibCRDTimeout))

	// Phase 3: Wait for Kind to receive conditions from its controller Graph.
	// The kindConditions node hoists Compiled/Ready from the kind-condwidget Graph.
	kindKey := types.NamespacedName{Name: "condwidget", Namespace: "kro-system"}
	t.Log("waiting for Kind Compiled=True condition...")
	require.NoError(t, waitForConditionStatus(ctx, t, k8sClient, kindGVK, kindKey, "Compiled", "True", stdlibReconcileTimeout),
		"Kind should have Compiled=True hoisted from controller Graph")

	t.Log("waiting for Kind Ready=True condition...")
	require.NoError(t, waitForConditionStatus(ctx, t, k8sClient, kindGVK, kindKey, "Ready", "True", stdlibReconcileTimeout),
		"Kind should have Ready=True hoisted from controller Graph")

	// Phase 4: Verify the old status.ready boolean is NOT present.
	require.NoError(t, k8sClient.Get(ctx, kindKey, kind))
	status, _ := kind.Object["status"].(map[string]any)
	require.NotNil(t, status, "Kind should have status")
	_, hasReadyBool := status["ready"]
	assert.False(t, hasReadyBool, "status.ready boolean should not exist — replaced by conditions")

	// Phase 5: Verify conditions have standard fields.
	conditions, _ := status["conditions"].([]any)
	require.NotEmpty(t, conditions, "Kind should have conditions")

	compiled, found := findCondition(conditions, "Compiled")
	require.True(t, found, "should have Compiled condition")
	assert.Equal(t, "True", compiled["status"])
	assert.NotEmpty(t, compiled["lastTransitionTime"], "Compiled should have lastTransitionTime")

	ready, found := findCondition(conditions, "Ready")
	require.True(t, found, "should have Ready condition")
	assert.Equal(t, "True", ready["status"])
	assert.NotEmpty(t, ready["lastTransitionTime"], "Ready should have lastTransitionTime")

	t.Log("Kind conditions verified: Compiled=True, Ready=True with timestamps")
}

// TestKindStatusItems verifies that status.items reflects the number of
// instances the Kind is managing.
func TestKindStatusItems(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	// Create a Kind that defines ItemCounter.
	t.Log("creating Kind: ItemCounter")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "itemcounter",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "ItemCounter",
				"spec": map[string]any{
					"value": "string | default=x",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-item",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"value": "${schema.spec.value}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	// Wait for CRD.
	require.NoError(t, waitForCRD(ctx, k8sClient, "itemcounters.test.stdlib.kro.run", stdlibCRDTimeout))

	// Wait for Kind to become Ready (controller Graph healthy).
	kindKey := types.NamespacedName{Name: "itemcounter", Namespace: "kro-system"}
	require.NoError(t, waitForConditionStatus(ctx, t, k8sClient, kindGVK, kindKey, "Ready", "True", stdlibReconcileTimeout))

	// Verify items=0 before any instances.
	t.Log("checking items count with no instances...")
	require.NoError(t, k8sClient.Get(ctx, kindKey, kind))
	items, _, _ := unstructured.NestedInt64(kind.Object, "status", "items")
	assert.Equal(t, int64(0), items, "items should be 0 with no instances")

	// Create an instance.
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "ItemCounter",
		"metadata": map[string]any{
			"name":      "ic-one",
			"namespace": "kro-system",
		},
		"spec": map[string]any{"value": "first"},
	}}
	require.NoError(t, k8sClient.Create(ctx, instance))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), instance) })

	// Wait for items to reflect the new instance.
	t.Log("waiting for items=1...")
	require.NoError(t, waitForItems(ctx, k8sClient, kindKey, 1, stdlibReconcileTimeout),
		"items should be 1 after creating an instance")

	// Create a second instance.
	instance2 := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "ItemCounter",
		"metadata": map[string]any{
			"name":      "ic-two",
			"namespace": "kro-system",
		},
		"spec": map[string]any{"value": "second"},
	}}
	require.NoError(t, k8sClient.Create(ctx, instance2))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), instance2) })

	// Wait for items to reflect both.
	t.Log("waiting for items=2...")
	require.NoError(t, waitForItems(ctx, k8sClient, kindKey, 2, stdlibReconcileTimeout),
		"items should be 2 after creating second instance")

	t.Log("Kind status.items correctly tracks instance count")
}

// TestKindConditionsDecoupledFromInstances verifies that the Kind resource
// shows Ready=True even when per-instance Graphs have not converged.
// The Kind's conditions are hoisted from the controller Graph, whose
// readiness is decoupled from instance convergence.
func TestKindConditionsDecoupledFromInstances(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	// Create a Kind whose nodes have an unsatisfiable readyWhen.
	t.Log("creating Kind with unsatisfiable readyWhen")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "conddecouple",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "CondDecouple",
				"spec": map[string]any{
					"value": "string | default=test",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-decouple",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"status": "pending",
						},
					},
					// readyWhen will never be satisfied (data.status != 'healthy').
					"readyWhen": []any{
						"${cm.data.status == 'healthy'}",
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	// Wait for CRD.
	require.NoError(t, waitForCRD(ctx, k8sClient, "conddecouples.test.stdlib.kro.run", stdlibCRDTimeout))

	// Create an instance — its per-instance Graph will never converge.
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "CondDecouple",
		"metadata": map[string]any{
			"name":      "cd-inst",
			"namespace": "kro-system",
		},
		"spec": map[string]any{"value": "test"},
	}}
	require.NoError(t, k8sClient.Create(ctx, instance))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), instance) })

	// Wait for the ConfigMap to confirm the instance was processed.
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "cd-inst-decouple", Namespace: "kro-system"}, cm, stdlibReconcileTimeout))

	// Verify the per-instance Graph is NOT ready.
	instanceGraphName := "kro-system-cd-inst-conddecouple"
	require.NoError(t, waitForGraphReadyStatus(ctx, k8sClient,
		types.NamespacedName{Name: instanceGraphName, Namespace: "kro-system"}, "Unknown", stdlibReconcileTimeout),
		"per-instance Graph should be NotReady")

	// The Kind resource MUST still show Ready=True — its conditions come from
	// the controller Graph (kind-conddecouple), not from per-instance Graphs.
	kindKey := types.NamespacedName{Name: "conddecouple", Namespace: "kro-system"}
	t.Log("verifying Kind is Ready despite instance not converging...")
	require.NoError(t, waitForConditionStatus(ctx, t, k8sClient, kindGVK, kindKey, "Ready", "True", stdlibReconcileTimeout),
		"Kind should be Ready=True even though instance Graph hasn't converged")
	require.NoError(t, waitForConditionStatus(ctx, t, k8sClient, kindGVK, kindKey, "Compiled", "True", stdlibReconcileTimeout),
		"Kind should be Compiled=True")

	t.Log("Kind conditions are decoupled from instance convergence — confirmed")
}

// waitForItems polls until the Kind resource's status.items equals the expected count.
func waitForItems(ctx context.Context, c client.Client, key types.NamespacedName, want int64, timeout ...time.Duration) error {
	t := 30 * time.Second
	if len(timeout) > 0 {
		t = timeout[0]
	}
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, t, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(kindGVK)
		if err := c.Get(ctx, key, obj); err != nil {
			return false, nil
		}
		val, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "status", "items")
		if !found || val == nil {
			return false, nil
		}
		// JSON numbers arrive as float64 or int64 depending on the codec.
		switch v := val.(type) {
		case int64:
			return v == want, nil
		case float64:
			return int64(v) == want, nil
		default:
			return fmt.Sprintf("%v", v) == fmt.Sprintf("%d", want), nil
		}
	})
}
