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
// Latch pattern integration tests
//
// A latch expression uses self-reference + orValue to generate a value on first
// create and preserve it across subsequent reconciles:
//
//   target.?data.?key.orValue(computeNewValue())
//
// On first reconcile: GET returns 404 → scope is empty map → orValue fires.
// On subsequent reconciles: GET returns the live object → field exists → orValue
// short-circuits → existing value preserved.
//
// These tests prove the pattern works for:
// - Immutable values (generate once, keep forever even when inputs change)
// - Nillable values (deep optional chaining through absent intermediate maps)
// - Non-nillable values (field always present, latch holds against recomputation)
// - Time-gated values (timestamp generated on boolean flip, preserved while stable)
// ═══════════════════════════════════════════════════════════════════════════════

// TestLatch_ImmutableSecret proves the core latch pattern for secret generation:
// a random value is computed on first create using random.seededString, and after
// that it's preserved via self-reference even when the seed source changes.
//
// This is the canonical "generate ECDSA key once" use case.
//
// Cycle 1: target is empty map → orValue fires → value generated from seed-v1
// Cycle 2+: target.data.key exists → orValue short-circuits → value preserved
// After seed change: value STILL preserved (latch holds)
func TestLatch_ImmutableSecret(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a source ConfigMap that provides the seed.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "latch-seed-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"seed": "initial-seed-v1",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph:
	// - ref node "source" watching the seed ConfigMap
	// - template node "secret" creating a ConfigMap with a latched random value
	//
	// The latch expression: secret.?data.?key.orValue(random.seededString(32, source.data.seed))
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-latch-immutable",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "latch-seed-source"},
						},
					},
					map[string]any{
						"id": "secret",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "latch-secret-target",
							},
							"data": map[string]any{
								"key": "${secret.?data.?key.orValue(random.seededString(32, source.data.seed))}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Wait for graph ready and target to converge.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-latch-immutable", Namespace: ns}))

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "latch-secret-target", Namespace: ns}, target))

	key1, found, _ := unstructured.NestedString(target.Object, "data", "key")
	require.True(t, found, "data.key should exist after first create")
	assert.Len(t, key1, 32, "random.seededString(32,...) should produce 32-char string")
	t.Logf("Initial latched value: %s", key1)

	// Change the seed source — if latch works, the value must NOT change.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-seed-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "different-seed-v2", "data", "seed")
		}))

	// Wait for reconcile to process the source change.
	require.NoError(t, waitForSettle(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-secret-target", Namespace: ns}))

	// Read again — value must be unchanged.
	target2 := &unstructured.Unstructured{}
	target2.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-secret-target", Namespace: ns}, target2))

	key2, found, _ := unstructured.NestedString(target2.Object, "data", "key")
	require.True(t, found, "data.key should still exist")
	assert.Equal(t, key1, key2,
		"latched value must be preserved even after seed changes — orValue short-circuits when field exists")
	t.Logf("After seed change: value preserved (%s == %s)", key1, key2)

	// Change the seed again to a third value — latch must still hold.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-seed-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "yet-another-seed-v3", "data", "seed")
		}))

	require.NoError(t, waitForSettle(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-secret-target", Namespace: ns}))

	target3 := &unstructured.Unstructured{}
	target3.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-secret-target", Namespace: ns}, target3))

	key3, found, _ := unstructured.NestedString(target3.Object, "data", "key")
	require.True(t, found, "data.key should still exist after third seed change")
	assert.Equal(t, key1, key3,
		"latched value must survive multiple seed changes — latch is permanent once set")
	t.Logf("After third seed: value still preserved (%s)", key3)
}

// TestLatch_NillableAnnotation proves that the latch pattern works through
// deeply nested optional chaining where intermediate maps may be absent.
//
// Annotations are nillable: a newly created resource may not have any
// annotations at all (the metadata.annotations map doesn't exist). The
// expression must handle:
//   target.?metadata.?annotations.?myKey.orValue("computed-default")
//
// This tests the full nil chain: resource absent (404) → empty map →
// metadata absent → annotations absent → myKey absent → orValue fires.
func TestLatch_NillableAnnotation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD — annotations on CRDs are nillable (not pre-populated).
	group := uniqueGroup()
	crdName := fmt.Sprintf("latchwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "LatchWidget", "latchwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "LatchWidget"}

	// Source provides a generation counter that changes.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "latch-nillable-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"generation": "1",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: template node that latches an annotation value.
	// The annotation is generated from source.data.generation on first create,
	// then preserved via self-reference.
	//
	// Deep chain: widget.?metadata.?annotations['kro.run/latched-gen'].orValue(source.data.generation)
	// Note: we use annotations map syntax since the key has special chars.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-latch-nillable",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "latch-nillable-source"},
						},
					},
					map[string]any{
						"id": "widget",
						"template": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "LatchWidget",
							"metadata": map[string]any{
								"name": "nillable-instance",
								"annotations": map[string]any{
									"kro.run/latched-gen": "${widget.?metadata.?annotations[\"kro.run/latched-gen\"].orValue(source.data.generation)}",
								},
							},
							"spec": map[string]any{
								"value": "placeholder",
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
		types.NamespacedName{Name: "test-latch-nillable", Namespace: ns}))

	// Wait for the instance to appear.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "nillable-instance", Namespace: ns}, obj))

	// Read the latched annotation.
	annotations := obj.GetAnnotations()
	require.NotNil(t, annotations, "annotations map should exist")
	val1, ok := annotations["kro.run/latched-gen"]
	require.True(t, ok, "kro.run/latched-gen annotation should exist")
	assert.Equal(t, "1", val1,
		"annotation should capture generation=1 from source on first create")
	t.Logf("Nillable annotation latched: kro.run/latched-gen=%s", val1)

	// Advance the source generation — latch must hold.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-nillable-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "2", "data", "generation")
		}))

	require.NoError(t, waitForSettle(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "nillable-instance", Namespace: ns}))

	obj2 := &unstructured.Unstructured{}
	obj2.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "nillable-instance", Namespace: ns}, obj2))

	annotations2 := obj2.GetAnnotations()
	val2 := annotations2["kro.run/latched-gen"]
	assert.Equal(t, "1", val2,
		"annotation must stay '1' after source advances to generation=2 — latch preserves through nillable chain")
	t.Logf("After source generation=2: annotation still %s (latch holds)", val2)

	// Advance again.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-nillable-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "3", "data", "generation")
		}))

	require.NoError(t, waitForSettle(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "nillable-instance", Namespace: ns}))

	obj3 := &unstructured.Unstructured{}
	obj3.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "nillable-instance", Namespace: ns}, obj3))

	annotations3 := obj3.GetAnnotations()
	val3 := annotations3["kro.run/latched-gen"]
	assert.Equal(t, "1", val3,
		"annotation must stay '1' after source advances to generation=3")
	t.Logf("After source generation=3: annotation still %s (nillable latch proven)", val3)
}

// TestLatch_NonNillableDataField proves the latch works for fields that are
// always present after first creation (non-nillable). A ConfigMap data field
// is always present once written. The test shows that even though the expression
// COULD produce a different value on each reconcile, orValue short-circuits
// because the field already exists.
//
// Pattern: target.?data.?version.orValue("v" + source.data.counter)
//
// Unlike nillable fields where intermediate maps may be absent, here
// target.data.version will always be a concrete string after first create.
func TestLatch_NonNillableDataField(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Source provides a counter that changes over time.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "latch-counter-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"counter": "1",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph:
	// - ref node "source" watching counter
	// - template node "target" with latched version string
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-latch-nonnillable",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "latch-counter-source"},
						},
					},
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "latch-nonnillable-target",
							},
							"data": map[string]any{
								// Latch: use existing version if present, else compute from counter.
								"version": `${target.?data.?version.orValue("v" + source.data.counter)}`,
								// Non-latched field: always reflects current counter (for comparison).
								"currentCounter": "${source.data.counter}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Wait for convergence.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-latch-nonnillable", Namespace: ns}))

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "latch-nonnillable-target", Namespace: ns}, target))

	version1, found, _ := unstructured.NestedString(target.Object, "data", "version")
	require.True(t, found, "data.version should exist")
	assert.Equal(t, "v1", version1, "initial version should be v1 (from counter=1)")

	counter1, found, _ := unstructured.NestedString(target.Object, "data", "currentCounter")
	require.True(t, found, "data.currentCounter should exist")
	assert.Equal(t, "1", counter1)
	t.Logf("Initial state: version=%s, currentCounter=%s", version1, counter1)

	// Advance counter to 2.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-counter-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "2", "data", "counter")
		}))

	// Wait for currentCounter to update (proves reconcile happened).
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-nonnillable-target", Namespace: ns},
		[]string{"data", "currentCounter"}, "2"))

	// Read version — must still be "v1" (latched).
	target2 := &unstructured.Unstructured{}
	target2.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-nonnillable-target", Namespace: ns}, target2))

	version2, _, _ := unstructured.NestedString(target2.Object, "data", "version")
	assert.Equal(t, "v1", version2,
		"version must stay 'v1' even though counter is now 2 — latch holds on non-nillable field")
	t.Logf("Counter=2: version=%s (latched), currentCounter=2 (live)", version2)

	// Advance counter to 3.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-counter-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "3", "data", "counter")
		}))

	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-nonnillable-target", Namespace: ns},
		[]string{"data", "currentCounter"}, "3"))

	target3 := &unstructured.Unstructured{}
	target3.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-nonnillable-target", Namespace: ns}, target3))

	version3, _, _ := unstructured.NestedString(target3.Object, "data", "version")
	assert.Equal(t, "v1", version3,
		"version must stay 'v1' after counter=3 — non-nillable latch proven across multiple reconciles")

	counter3, _, _ := unstructured.NestedString(target3.Object, "data", "currentCounter")
	assert.Equal(t, "3", counter3, "currentCounter should be live (not latched)")
	t.Logf("Counter=3: version=%s (latched), currentCounter=%s (live) — proves latch vs non-latch contrast", version3, counter3)
}

// TestLatch_TimestampOnBooleanFlip proves time-gated latching: a timestamp is
// generated when a boolean elsewhere in the graph becomes "true", preserved
// while the boolean stays "true", and reset to empty when the boolean becomes
// "false" — allowing re-generation on the next true flip.
//
// This demonstrates the pattern for "record when something happened" — e.g.,
// recording when a deployment became ready, or when a feature was enabled.
//
// Expression (nested ternary):
//   trigger.data.enabled == "true"
//     ? (target.?data.?activatedAt.orValue("") == "" ? string(time.now()) : target.data.activatedAt)
//     : ""
//
// Phase 1: enabled=false → activatedAt="" (empty string, no timestamp)
// Phase 2: enabled=true  → activatedAt was "" → time.now() fires → timestamp captured
// Phase 3: enabled=true  → activatedAt is non-empty → inner ternary keeps it → preserved
// Phase 4: enabled=false → activatedAt="" → timestamp cleared
// Phase 5: enabled=true  → activatedAt was "" → time.now() fires → new timestamp
func TestLatch_TimestampOnBooleanFlip(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create trigger ConfigMap (starts disabled).
	trigger := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "latch-trigger",
				"namespace": ns,
			},
			"data": map[string]any{
				"enabled": "false",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, trigger))

	// Graph:
	// - ref node "trigger" watches the boolean
	// - template node "target" with time-gated latch
	//
	// The expression uses a nested ternary:
	// - Outer: if enabled → latch logic, else → ""
	// - Inner: if activatedAt is empty → generate with time.now(), else → keep existing
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-latch-timestamp",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "trigger",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "latch-trigger"},
						},
					},
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "latch-timestamp-target",
							},
							"data": map[string]any{
								// Time-gated latch: capture time.now() when enabled AND field is empty.
								// Preserve when enabled AND field is non-empty. Clear when disabled.
								"activatedAt": `${trigger.data.enabled == "true" ? (target.?data.?activatedAt.orValue("") == "" ? string(time.now()) : target.data.activatedAt) : ""}`,
								// Always-present field to prove reconcile is running.
								"enabled": "${trigger.data.enabled}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Wait for graph ready and target to exist.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-latch-timestamp", Namespace: ns}))

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns}, target))

	// Phase 1: enabled=false → activatedAt should be empty string.
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns},
		[]string{"data", "enabled"}, "false"))

	target1 := &unstructured.Unstructured{}
	target1.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns}, target1))

	val1, _, _ := unstructured.NestedString(target1.Object, "data", "activatedAt")
	assert.Equal(t, "", val1,
		"Phase 1: activatedAt should be empty when enabled=false")
	t.Logf("Phase 1: enabled=false, activatedAt=%q (correct — empty)", val1)

	// Phase 2: flip enabled=true → timestamp should be generated.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-trigger", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "true", "data", "enabled")
		}))

	// Wait for the enabled field to propagate.
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns},
		[]string{"data", "enabled"}, "true"))

	// Wait for activatedAt to become non-empty.
	require.NoError(t, waitForNonEmptyField(ctx,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns},
		[]string{"data", "activatedAt"}, 15*time.Second))

	target2 := &unstructured.Unstructured{}
	target2.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns}, target2))

	ts1, found, _ := unstructured.NestedString(target2.Object, "data", "activatedAt")
	require.True(t, found, "Phase 2: activatedAt should exist after enabled=true")
	require.NotEmpty(t, ts1, "Phase 2: activatedAt should be non-empty")
	// Validate it's a valid timestamp.
	_, err := time.Parse(time.RFC3339, ts1)
	require.NoError(t, err, "activatedAt should be valid RFC3339: %s", ts1)
	t.Logf("Phase 2: enabled=true, activatedAt=%s (generated)", ts1)

	// Phase 3: keep enabled=true, trigger another reconcile → timestamp must be preserved.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-trigger", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			// Add an unrelated field to trigger reconcile without changing enabled.
			_ = unstructured.SetNestedField(obj.Object, "noise", "data", "extra")
		}))

	require.NoError(t, waitForSettle(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns}))

	target3 := &unstructured.Unstructured{}
	target3.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns}, target3))

	ts2, found, _ := unstructured.NestedString(target3.Object, "data", "activatedAt")
	require.True(t, found, "Phase 3: activatedAt should still exist")
	assert.Equal(t, ts1, ts2,
		"Phase 3: activatedAt must be preserved across reconciles while enabled=true — latch holds")
	t.Logf("Phase 3: enabled=true (unchanged), activatedAt=%s (preserved)", ts2)

	// Phase 4: flip enabled=false → activatedAt should become empty.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-trigger", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "false", "data", "enabled")
		}))

	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns},
		[]string{"data", "enabled"}, "false"))

	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns},
		[]string{"data", "activatedAt"}, ""))

	target4 := &unstructured.Unstructured{}
	target4.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-timestamp-target", Namespace: ns}, target4))

	val4, _, _ := unstructured.NestedString(target4.Object, "data", "activatedAt")
	assert.Equal(t, "", val4,
		"Phase 4: activatedAt should be empty when enabled flips back to false")
	t.Logf("Phase 4: enabled=false, activatedAt=%q (cleared)", val4)
}

// TestLatch_MultiFieldIndependent proves that multiple latched fields in the
// same resource are independent — each latches at its own time and changing
// one does not affect the others.
func TestLatch_MultiFieldIndependent(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Source provides distinct seeds for two fields.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "latch-multi-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"seedA": "alpha-seed",
				"seedB": "beta-seed",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph with two independently latched fields.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-latch-multifield",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "latch-multi-source"},
						},
					},
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "latch-multi-target",
							},
							"data": map[string]any{
								"keyA": "${target.?data.?keyA.orValue(random.seededString(16, source.data.seedA))}",
								"keyB": "${target.?data.?keyB.orValue(random.seededString(16, source.data.seedB))}",
								// Non-latched for reconcile proof.
								"witness": "${source.data.seedA}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-latch-multifield", Namespace: ns}))

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "latch-multi-target", Namespace: ns}, target))

	keyA1, foundA, _ := unstructured.NestedString(target.Object, "data", "keyA")
	require.True(t, foundA, "data.keyA should exist")
	keyB1, foundB, _ := unstructured.NestedString(target.Object, "data", "keyB")
	require.True(t, foundB, "data.keyB should exist")

	assert.Len(t, keyA1, 16)
	assert.Len(t, keyB1, 16)
	assert.NotEqual(t, keyA1, keyB1,
		"keyA and keyB should be different (different seeds)")
	t.Logf("Initial: keyA=%s, keyB=%s", keyA1, keyB1)

	// Change seedA — keyA must stay latched, keyB unaffected.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-multi-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "alpha-seed-changed", "data", "seedA")
		}))

	// Wait for witness to change (proves reconcile ran).
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-multi-target", Namespace: ns},
		[]string{"data", "witness"}, "alpha-seed-changed"))

	target2 := &unstructured.Unstructured{}
	target2.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-multi-target", Namespace: ns}, target2))

	keyA2, _, _ := unstructured.NestedString(target2.Object, "data", "keyA")
	keyB2, _, _ := unstructured.NestedString(target2.Object, "data", "keyB")

	assert.Equal(t, keyA1, keyA2, "keyA must be preserved despite seedA change")
	assert.Equal(t, keyB1, keyB2, "keyB must be preserved (seedB unchanged)")
	t.Logf("After seedA change: keyA=%s (latched), keyB=%s (latched)", keyA2, keyB2)

	// Change seedB — both must still be latched.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-multi-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "beta-seed-changed", "data", "seedB")
		}))

	require.NoError(t, waitForSettle(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-multi-target", Namespace: ns}))

	target3 := &unstructured.Unstructured{}
	target3.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-multi-target", Namespace: ns}, target3))

	keyA3, _, _ := unstructured.NestedString(target3.Object, "data", "keyA")
	keyB3, _, _ := unstructured.NestedString(target3.Object, "data", "keyB")

	assert.Equal(t, keyA1, keyA3, "keyA must still be preserved")
	assert.Equal(t, keyB1, keyB3, "keyB must still be preserved despite seedB change")
	t.Logf("After both seeds changed: keyA=%s, keyB=%s (both latched independently)", keyA3, keyB3)
}

// TestLatch_RegenerationAfterDeletion proves that when a latched resource is
// externally deleted, the controller re-creates it and re-fires orValue since
// the GET returns 404 (empty map in scope). This is the expected behavior:
// the latch is tied to the resource's lifecycle, not to the graph's history.
//
// Cycle after delete: GET returns 404 → scope is empty map → orValue fires → new value
func TestLatch_RegenerationAfterDeletion(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Source provides the seed.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "latch-regen-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"seed": "stable-seed",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph with a latched random value.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-latch-regen",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "latch-regen-source"},
						},
					},
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "latch-regen-target",
							},
							"data": map[string]any{
								"key": "${target.?data.?key.orValue(random.seededString(32, source.data.seed))}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-latch-regen", Namespace: ns}))

	// Read initial latched value.
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "latch-regen-target", Namespace: ns}, target))

	key1, found, _ := unstructured.NestedString(target.Object, "data", "key")
	require.True(t, found, "data.key should exist")
	assert.Len(t, key1, 32)
	t.Logf("Initial latched value: %s", key1)

	// Externally delete the target (simulates accidental deletion or namespace cleanup race).
	toDelete := &unstructured.Unstructured{}
	toDelete.SetGroupVersionKind(cmGVK)
	toDelete.SetName("latch-regen-target")
	toDelete.SetNamespace(ns)
	require.NoError(t, k8sClient.Delete(ctx, toDelete))

	// Wait for the controller to re-create it.
	target2 := &unstructured.Unstructured{}
	target2.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "latch-regen-target", Namespace: ns}, target2))

	key2, found, _ := unstructured.NestedString(target2.Object, "data", "key")
	require.True(t, found, "data.key should exist after re-creation")
	assert.Len(t, key2, 32)

	// Since random.seededString is deterministic with the same seed, the value
	// should be the same — orValue fires again but produces the same result.
	assert.Equal(t, key1, key2,
		"re-generated value should be identical (deterministic seed) — latch re-fired but result stable")
	t.Logf("After deletion and re-creation: key=%s (same — deterministic seed)", key2)
}

// TestLatch_ExternalMutationPreserved proves that if an external actor (kubectl
// edit, another controller) modifies a latched field, the controller preserves
// the edited value. The latch reads live state — it doesn't remember the
// original computed value.
//
// This is the "latch preserves live state, not original state" property.
func TestLatch_ExternalMutationPreserved(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "latch-mutation-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"seed": "original-seed",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-latch-mutation",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "latch-mutation-source"},
						},
					},
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "latch-mutation-target",
							},
							"data": map[string]any{
								"key": "${target.?data.?key.orValue(random.seededString(32, source.data.seed))}",
								// Witness field to prove reconcile runs after mutation.
								"witness": "${source.data.seed}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-latch-mutation", Namespace: ns}))

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "latch-mutation-target", Namespace: ns}, target))

	key1, found, _ := unstructured.NestedString(target.Object, "data", "key")
	require.True(t, found)
	t.Logf("Initial latched value: %s", key1)

	// Externally mutate the latched field (simulates kubectl edit).
	externalValue := "externally-set-value-by-admin"
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-mutation-target", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, externalValue, "data", "key")
		}))

	// Trigger a reconcile by changing the source (to prove controller runs).
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-mutation-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "changed-seed", "data", "seed")
		}))

	// Wait for witness to update (proves reconcile happened).
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "latch-mutation-target", Namespace: ns},
		[]string{"data", "witness"}, "changed-seed"))

	// Read the key — it should be the externally-set value, not the original
	// computed value and not a recomputed value from the new seed.
	target2 := &unstructured.Unstructured{}
	target2.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "latch-mutation-target", Namespace: ns}, target2))

	key2, found, _ := unstructured.NestedString(target2.Object, "data", "key")
	require.True(t, found)
	assert.Equal(t, externalValue, key2,
		"latch must preserve externally-mutated value — it reads live state, not original computed state")
	t.Logf("After external mutation + reconcile: key=%s (preserved external edit)", key2)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// waitForNonEmptyField polls until the given field path has a non-empty string value.
func waitForNonEmptyField(ctx context.Context, key types.NamespacedName, path []string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, key, obj); err != nil {
			return false, nil
		}
		val, found, _ := unstructured.NestedString(obj.Object, path...)
		return found && val != "", nil
	})
}
