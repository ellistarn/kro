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
)

// ═══════════════════════════════════════════════════════════════════════════════
// Standard library integration tests
//
// These tests exercise the three stdlib types: Kind, Decorator, and Singleton.
// CRDs are installed by envtest from crds/, and stdlib resources are
// applied during test setup. By the time tests run, the type tower is
// materialized:
//
//   kind.yaml (Graph) → Kind CRD
//   decorator.yaml (Kind) → Decorator CRD
//   singleton.yaml (Kind) → Singleton CRD
//
// Each test creates instances of these types and verifies end-to-end
// behavior through multiple layers of Graph reconciliation.
// ═══════════════════════════════════════════════════════════════════════════════

// stdlibCRDTimeout is how long to wait for a CRD that the controller creates.
// The controller must reconcile stdlib Graphs before derived CRDs appear.
const stdlibCRDTimeout = 60 * time.Second

// stdlibReconcileTimeout is how long to wait for multi-layer reconciliation
// chains (Kind → CRD → instance → child resource). More layers = more
// reconcile loops = more time.
const stdlibReconcileTimeout = 120 * time.Second

// ═══════════════════════════════════════════════════════════════════════════════
// Kind
//
// Define a new Kubernetes type. The Kind controller creates a CRD per Kind,
// watches instances of that CRD, and stamps a per-instance Graph whose
// nodes produce child resources.
//
// Pipeline: Kind → CRD → instance → per-instance Graph → child resources
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibKind(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	// Phase 1: Create a Kind that defines TestWidget.
	// Kind + instances live in kro-system (Kind controller is namespace-scoped there).
	t.Log("creating Kind: TestWidget")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "testwidget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "TestWidget",
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
							"name":      "${schema.metadata.name}-config",
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

	// Phase 2: Wait for the TestWidget CRD.
	t.Log("waiting for TestWidget CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "testwidgets.test.stdlib.kro.run", stdlibCRDTimeout))
	t.Log("TestWidget CRD established")

	// Phase 3: Create a TestWidget instance.
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "TestWidget",
		"metadata": map[string]any{
			"name":      "my-widget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"message": "hello from kind test",
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, widget))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), widget) })

	// Phase 4: Verify the per-instance ConfigMap appears with correct data.
	// Chain: Kind → CRD → instance → per-instance Graph → ConfigMap.
	t.Log("waiting for per-instance ConfigMap...")
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	cmKey := types.NamespacedName{Name: "my-widget-config", Namespace: "kro-system"}

	require.NoError(t, waitForResource(ctx, k8sClient, cmKey, cm, stdlibReconcileTimeout),
		"ConfigMap my-widget-config not created within timeout")

	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "hello from kind test", data["message"],
		"ConfigMap should carry the widget's message")
	t.Log("Kind pipeline works: Kind → CRD → instance → ConfigMap")
}

// TestStdlibKindStatusWriteback verifies that spec.schema.status CEL
// expressions are evaluated and patched back onto the instance's status
// subresource. The Kind controller synthesizes a status patch node in
// each per-instance Graph.
func TestStdlibKindStatusWriteback(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	// Phase 1: Create a Kind whose schema.status has a CEL expression
	// that reads from a child resource.
	t.Log("creating Kind: StatusWidget")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "statuswidget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "StatusWidget",
				"spec": map[string]any{
					"message": "string | default=hello",
				},
				"status": map[string]any{
					"configMapName": "${cm.metadata.name}",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-status-cm",
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

	// Phase 2: Wait for the StatusWidget CRD.
	t.Log("waiting for StatusWidget CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "statuswidgets.test.stdlib.kro.run", stdlibCRDTimeout))
	t.Log("StatusWidget CRD established")

	// Phase 3: Create a StatusWidget instance.
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "StatusWidget",
		"metadata": map[string]any{
			"name":      "my-status-widget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"message": "status test",
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, widget))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), widget) })

	// Phase 4: Verify that the status expression is evaluated and written
	// back to the instance. The expression ${cm.metadata.name} should resolve
	// to "my-status-widget-status-cm".
	t.Log("waiting for status writeback on StatusWidget instance...")
	widgetGVK := schema.GroupVersionKind{
		Group:   "test.stdlib.kro.run",
		Version: "v1alpha1",
		Kind:    "StatusWidget",
	}
	widgetKey := types.NamespacedName{Name: "my-status-widget", Namespace: "kro-system"}
	require.NoError(t, waitForField(ctx, k8sClient, widgetGVK, widgetKey,
		[]string{"status", "configMapName"}, "my-status-widget-status-cm", stdlibReconcileTimeout),
		"status.configMapName not written back to StatusWidget instance within timeout")
	t.Log("Kind status writeback works: schema.status CEL → instance .status")
}

// TestKindBareTypeStatusCausesError reproduces the bug where bare type
// declarations in Kind schema status fields (e.g., `ready: boolean`) cause
// the kindInstancePatch to write literal type descriptor strings to the API
// server. The SSA apply rejects the type mismatch, producing a permanent
// SystemError on the per-instance Graph.
//
// Root cause: kindInstancePatch embeds k.spec.schema.status directly as the
// patch body. Bare types pass through evaluation unchanged (no ${...} to
// evaluate) and are written as literal strings to typed fields.
func TestKindBareTypeStatusCausesError(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	// Create a Kind with bare type declarations in status (the bug trigger).
	t.Log("creating Kind with bare type status: BrokenWidget")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "brokenwidget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "BrokenWidget",
				"spec": map[string]any{
					"color": "string | default=red",
				},
				"status": map[string]any{
					"ready":   "boolean", // bare type — NOT an expression
					"message": "string",  // bare type — NOT an expression
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-data",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"color": "${schema.spec.color}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	// Wait for the BrokenWidget CRD to be established.
	require.NoError(t, waitForCRD(ctx, k8sClient, "brokenwidgets.test.stdlib.kro.run", stdlibCRDTimeout))
	t.Log("BrokenWidget CRD established")

	// Create a BrokenWidget instance.
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "BrokenWidget",
		"metadata": map[string]any{
			"name":      "test-broken",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"color": "blue",
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, widget))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), widget) })

	// The per-instance Graph should enter SystemError because the
	// kindInstancePatch tries to write "boolean" (string) to a boolean field.
	graphKey := types.NamespacedName{Name: "kind.brokenwidget.test-broken", Namespace: "kro-system"}
	t.Log("waiting for per-instance Graph to report SystemError...")
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "SystemError", stdlibReconcileTimeout),
		"per-instance Graph should have Ready=False reason=SystemError due to bare type status")
	t.Log("Confirmed: bare type status declarations cause SystemError on kindInstancePatch")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Decorator
//
// Watch instances of an existing kind and create a sub-Graph per item.
// Each sub-Graph gets an auto-prepended "item" Watch node. User nodes
// reference ${item.*} to derive child resources from the watched object.
//
// Pipeline: Decorator → controller Graph → items (Watch) → sub-Graph per item → child resources
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibDecorator(t *testing.T) {
	// Intentionally serial: this Decorator's items Watch uses an
	// empty selector and no namespace, so it picks up every ConfigMap
	// cluster-wide — including those created by TestStdlibKind when
	// running concurrently. Parallel execution is safe but stamps extra
	// "-decorated" ConfigMaps that inflate runtime and muddy the test's
	// intent. Keep serial until the stdlib Decorator template honors
	// spec.watch.selector.
	require.NoError(t, waitForCRD(ctx, k8sClient, "decorators.experimental.kro.run", stdlibCRDTimeout))

	// Phase 1: Create a Decorator that watches ConfigMaps and creates
	// a companion ConfigMap per item with derived data.
	//
	// The Decorator lives in kro-system. Its Watch omits
	// metadata.namespace, so it watches ConfigMaps across all namespaces
	// (k8s list/watch semantics). The test places the source ConfigMap
	// in kro-system for convenience — any namespace would work.
	t.Log("creating Decorator: configmap-annotator")
	decorator := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Decorator",
		"metadata": map[string]any{
			"name":      "configmap-annotator",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"watch": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"selector":   map[string]any{"stdlib-test": "decorator"},
			},
			"nodes": []any{
				map[string]any{
					"id": "companion",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${item.metadata.name}-decorated",
							"namespace": "${item.metadata.namespace}",
						},
						"data": map[string]any{
							"source": "${item.metadata.name}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, decorator))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), decorator) })

	// Phase 2: Create a ConfigMap in kro-system with the matching label.
	// The Decorator's Watch is cluster-wide; kro-system is just where
	// this test places the source.
	t.Log("creating watched ConfigMap in kro-system")
	source := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "my-config",
			"namespace": "kro-system",
			"labels":    map[string]any{"stdlib-test": "decorator"},
		},
		"data": map[string]any{"key": "value"},
	}}
	require.NoError(t, k8sClient.Create(ctx, source))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), source) })

	// Phase 3: Verify the companion ConfigMap appears in kro-system.
	// Chain: Decorator → controller Graph → Watch picks up my-config →
	// sub-Graph → companion ConfigMap.
	t.Log("waiting for companion ConfigMap...")
	companion := &unstructured.Unstructured{}
	companion.SetAPIVersion("v1")
	companion.SetKind("ConfigMap")
	companionKey := types.NamespacedName{Name: "my-config-decorated", Namespace: "kro-system"}

	require.NoError(t, waitForResource(ctx, k8sClient, companionKey, companion, stdlibReconcileTimeout),
		"companion ConfigMap my-config-decorated not created within timeout")

	data, _, _ := unstructured.NestedStringMap(companion.Object, "data")
	assert.Equal(t, "my-config", data["source"],
		"companion should reference the source ConfigMap's name")
	t.Log("Decorator pipeline works: Decorator → Watch → sub-Graph → companion ConfigMap")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Singleton
//
// Declare a resource that should exist exactly once. When multiple
// Singletons target the same resource (same GVK + namespace + name),
// the highest priority wins. Ties broken by earliest creationTimestamp.
//
// Implemented as a single long-lived Graph (fan-in pattern). The Graph
// watches all Singleton CRs, computes winners per unique target identity,
// and applies one resource per identity via forEach template with force-apply.
//
// The target is owned by the singleton controller Graph, not by any
// per-instance sub-Graph. When a Singleton CR is deleted, the Graph
// re-evaluates: if peers remain, the target persists with the new
// winner's template (force-applied in place, same UID).
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibSingleton(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "singletons.experimental.kro.run", stdlibCRDTimeout))

	ns := createNamespace(t)

	// Phase 1: Create two Singletons targeting the same ConfigMap with
	// different priorities. The higher priority should win.
	t.Log("creating Singleton: team-a (priority 10)")
	singletonA := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Singleton",
		"metadata": map[string]any{
			"name":      "team-a",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"priority": int64(10),
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "contested", "namespace": ns},
				"data":       map[string]any{"owner": "team-a"},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, singletonA))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), singletonA) })

	t.Log("creating Singleton: team-b (priority 100)")
	singletonB := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Singleton",
		"metadata": map[string]any{
			"name":      "team-b",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"priority": int64(100),
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "contested", "namespace": ns},
				"data":       map[string]any{"owner": "team-b"},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, singletonB))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), singletonB) })

	// Phase 2: Verify the ConfigMap appears with team-b's content (higher priority).
	t.Log("waiting for contested ConfigMap (expecting team-b wins)...")
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	cmKey := types.NamespacedName{Name: "contested", Namespace: ns}

	require.NoError(t, waitForField(ctx, k8sClient,
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		cmKey, []string{"data", "owner"}, "team-b", stdlibReconcileTimeout),
		"ConfigMap not created with team-b content within timeout")
	t.Log("team-b (priority 100) wins over team-a (priority 10)")

	// Phase 3: Verify status writeback on both Singletons. The Kind controller
	// evaluates the status expressions in kindInstancePatch and writes back
	// active (boolean) and claim (string) to each instance.
	//
	// This is the regression test for the bare-type-status bug: previously
	// the singleton declared `active: boolean` and `claim: string` as bare
	// type declarations. The kindInstancePatch wrote the literal strings
	// "boolean" and "string" as status values, causing SSA rejection:
	//   .status.active: expected boolean, got &{boolean}
	// The fix changes these to CEL expressions that compute the values.
	singletonGVK := schema.GroupVersionKind{
		Group:   "experimental.kro.run",
		Version: "v1alpha1",
		Kind:    "Singleton",
	}
	t.Log("waiting for status writeback on Singleton team-b (holder)...")
	require.NoError(t, waitForField(ctx, k8sClient, singletonGVK,
		types.NamespacedName{Name: "team-b", Namespace: "kro-system"},
		[]string{"status", "claim"}, "v1/namespaces/"+ns+"/ConfigMap/contested", stdlibReconcileTimeout),
		"status.claim not written back to Singleton team-b")
	t.Log("Singleton status writeback works: team-b has claim")

	// Verify team-b is active (holder), team-a is not.
	// waitForField only checks strings; for booleans, check inline.
	bObj := &unstructured.Unstructured{}
	bObj.SetGroupVersionKind(singletonGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "team-b", Namespace: "kro-system"}, bObj))
	active, found, _ := unstructured.NestedBool(bObj.Object, "status", "active")
	assert.True(t, found, "status.active should be present on team-b")
	assert.True(t, active, "team-b should be active (it holds the claim)")

	aObj := &unstructured.Unstructured{}
	aObj.SetGroupVersionKind(singletonGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "team-a", Namespace: "kro-system"}, aObj))
	activeA, foundA, _ := unstructured.NestedBool(aObj.Object, "status", "active")
	assert.True(t, foundA, "status.active should be present on team-a")
	assert.False(t, activeA, "team-a should NOT be active (lower priority)")
	t.Log("Singleton status expressions work: active=true on holder, false on loser")

	// Failover is tested separately in TestStdlibSingletonFailover.
}

// TestStdlibSingletonFailover proves the force-takeover refcounting mechanism:
// when the current holder is deleted, the next-in-line adopts the resource
// via force-apply (SSA with ForceOwnership). The resource is never deleted.
//
// Sequence:
//  1. team-b (priority 100) holds the resource
//  2. team-b is deleted
//  3. team-a (priority 10) re-evaluates as holder, force-applies → adopts resource
//  4. Resource data changes from team-b's content to team-a's content
//  5. Resource was never deleted (resourceVersion continuity, no recreate gap)
//
// The mechanism: team-a's per-instance Graph watches all Singletons. When
// team-b disappears, team-a's holder computation changes, its includeWhen
// opens, and it force-applies the target. Force-apply stamps team-a's
// identity labels, evicting team-b's. If team-b's teardown runs afterward,
// deletePreflight sees labels don't match → deleteNotOwned → skip.
func TestStdlibSingletonFailover(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "singletons.experimental.kro.run", stdlibCRDTimeout))

	ns := createNamespace(t)

	// Phase 1: Create team-a (low priority) first, then team-b (high priority).
	t.Log("creating Singleton: team-a (priority 10)")
	singletonA := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Singleton",
		"metadata": map[string]any{
			"name":      "failover-a",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"priority": int64(10),
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "failover-target", "namespace": ns},
				"data":       map[string]any{"owner": "team-a"},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, singletonA))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), singletonA) })

	t.Log("creating Singleton: team-b (priority 100)")
	singletonB := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Singleton",
		"metadata": map[string]any{
			"name":      "failover-b",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"priority": int64(100),
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "failover-target", "namespace": ns},
				"data":       map[string]any{"owner": "team-b"},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, singletonB))

	// Phase 2: Wait for team-b to win and create the resource.
	cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	cmKey := types.NamespacedName{Name: "failover-target", Namespace: ns}

	t.Log("waiting for ConfigMap with team-b as owner...")
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK, cmKey,
		[]string{"data", "owner"}, "team-b", stdlibReconcileTimeout),
		"ConfigMap should be created with team-b content (higher priority)")

	// Record the resourceVersion before deletion — we'll compare after failover
	// to prove the resource was mutated in place (not deleted and recreated).
	cmObj := &unstructured.Unstructured{}
	cmObj.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, cmKey, cmObj))
	rvBefore := cmObj.GetResourceVersion()
	uidBefore := cmObj.GetUID()
	t.Logf("ConfigMap before failover: rv=%s uid=%s", rvBefore, uidBefore)

	// Phase 3: Delete team-b (the holder). This triggers:
	// - team-a's watch sees team-b disappear → recomputes holder → team-a wins
	// - team-a's includeWhen opens → force-applies the target → adopts in place
	// - team-b's per-instance Graph teardown (if forEach prune fires) sees
	//   labels changed → deletePreflight returns deleteNotOwned → skip
	t.Log("deleting Singleton team-b (holder)...")
	require.NoError(t, k8sClient.Delete(ctx, singletonB))

	// Phase 4: Wait for team-a to adopt — the data.owner field should flip.
	t.Log("waiting for team-a to adopt the ConfigMap via force-apply...")
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK, cmKey,
		[]string{"data", "owner"}, "team-a", stdlibReconcileTimeout),
		"team-a should adopt the ConfigMap after team-b is deleted (force-apply takeover)")

	// Phase 5: Verify the resource was adopted in place (same UID = no delete/recreate).
	// NOTE: This assertion documents the DESIRED behavior. Currently, a race
	// exists: team-b's per-instance Graph teardown may delete the resource
	// before team-a's force-apply can adopt it. When that happens, the UID
	// changes (resource was recreated). The force-apply mechanism is correct
	// (team-a gets the resource either way), but the zero-downtime guarantee
	// (no delete/recreate gap) requires a deletion guard — the dying holder's
	// teardown must skip deletion when other claimants exist.
	require.NoError(t, k8sClient.Get(ctx, cmKey, cmObj))
	uidAfter := cmObj.GetUID()
	t.Logf("ConfigMap after failover: rv=%s uid=%s", cmObj.GetResourceVersion(), uidAfter)

	assert.Equal(t, uidBefore, uidAfter,
		"UID must be unchanged — resource was adopted in place, not deleted and recreated. "+
			"If this fails, the race condition was lost: teardown deleted before force-apply adopted.")
	t.Log("force-apply takeover confirmed: same UID, data changed to team-a")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Kind wrapped in Graph (deferred expressions)
//
// When a Kind is created by an outer Graph using ${${...}} expressions, the
// compiler must recognize the Kind template's child scope (implicit "schema"
// + spec.nodes IDs) and validate deferred expressions against it.
//
// Regression test for: deferred expression validation rejected Kind templates
// with "undeclared reference to 'schema'" because ExtractChildScopeFromBody
// only recognized kind: Graph, not kind: Kind.
//
// Pipeline: outer Graph → Kind → CRD → instance → per-instance Graph → child resources
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibKindWrappedInGraph(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	// Phase 1: Create an outer Graph that templates a Kind with ${${...}} expressions.
	t.Log("creating outer Graph that stamps a Kind with deferred expressions")
	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata": map[string]any{
			"name":      "kind-in-graph",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"nodes": []any{
				map[string]any{
					"id": "mykind",
					"template": map[string]any{
						"apiVersion": "experimental.kro.run/v1alpha1",
						"kind":       "Kind",
						"metadata": map[string]any{
							"name":      "graphwidget",
							"namespace": "kro-system",
						},
						"spec": map[string]any{
							"schema": map[string]any{
								"apiVersion": "test.kindingrph.kro.run/v1alpha1",
								"kind":       "GraphWidget",
								"spec": map[string]any{
									"message": "string | default=from-graph",
								},
								"status": map[string]any{
									"configMapName": "${${cm.metadata.name}}",
								},
							},
							"nodes": []any{
								map[string]any{
									"id": "cm",
									"template": map[string]any{
										"apiVersion": "v1",
										"kind":       "ConfigMap",
										"metadata": map[string]any{
											"name":      "${${schema.metadata.name}}-fromgraph",
											"namespace": "${${schema.metadata.namespace}}",
										},
										"data": map[string]any{
											"message": "${${schema.spec.message}}",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), graph) })

	// Phase 2: Verify the outer Graph compiles and reaches Ready.
	t.Log("waiting for outer Graph to be Ready...")
	graphKey := types.NamespacedName{Name: "kind-in-graph", Namespace: "kro-system"}
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey, stdlibReconcileTimeout),
		"outer Graph did not reach Ready — deferred expression compilation may have failed")
	t.Log("outer Graph is Ready")

	// Phase 3: Wait for the GraphWidget CRD (created by the Kind controller).
	t.Log("waiting for GraphWidget CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "graphwidgets.test.kindingrph.kro.run", stdlibCRDTimeout))
	t.Log("GraphWidget CRD established")

	// Phase 4: Create an instance of GraphWidget.
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.kindingrph.kro.run/v1alpha1",
		"kind":       "GraphWidget",
		"metadata": map[string]any{
			"name":      "gw-instance",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"message": "hello from kind-in-graph",
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, widget))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), widget) })

	// Phase 5: Verify the child ConfigMap appears with correct data.
	// Chain: Graph → Kind → CRD → instance → per-instance Graph → ConfigMap.
	t.Log("waiting for per-instance ConfigMap...")
	cmObj := &unstructured.Unstructured{}
	cmObj.SetAPIVersion("v1")
	cmObj.SetKind("ConfigMap")
	cmKey := types.NamespacedName{Name: "gw-instance-fromgraph", Namespace: "kro-system"}

	require.NoError(t, waitForResource(ctx, k8sClient, cmKey, cmObj, stdlibReconcileTimeout),
		"ConfigMap gw-instance-fromgraph not created within timeout")

	data, _, _ := unstructured.NestedStringMap(cmObj.Object, "data")
	assert.Equal(t, "hello from kind-in-graph", data["message"],
		"ConfigMap should carry the widget's message through the full Kind-in-Graph pipeline")
	t.Log("Kind-in-Graph pipeline works: Graph → Kind → CRD → instance → ConfigMap")
}
