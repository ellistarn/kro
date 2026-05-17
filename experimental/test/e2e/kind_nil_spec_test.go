package graphcontroller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Kind with nil/empty spec
//
// A Kind may declare a schema with no spec fields — e.g., a cluster-scoped
// marker resource that carries no user-facing configuration. The system must:
//
//   1. Generate a valid CRD (simpleSchema.toOpenAPI must handle nil spec)
//   2. Allow instances to be created without spec
//   3. Write status conditions to instances (status subresource must work)
//
// Previously, a nil spec caused celSimpleSchemaFunction to pass the entire
// schema envelope to simpleschema.ToOpenAPISpec, which crashed trying to
// parse "apiVersion: mygroup.io/v1" as a type string.
// ═══════════════════════════════════════════════════════════════════════════════

// TestStdlibKindNilSpec proves that a Kind with no spec fields in its schema
// compiles successfully and produces a working CRD. This is the minimal Kind:
// just apiVersion + kind, no spec, no status, no nodes.
func TestStdlibKindNilSpec(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	// Phase 1: Create a Kind whose schema has NO spec field.
	t.Log("creating Kind with nil spec: ClusterMarker")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "clustermarker",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"scope": "Cluster",
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "ClusterMarker",
				// No "spec" key — this is the nil-spec case.
			},
			"nodes": []any{},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	// Phase 2: The CRD must be created and established.
	// This is the critical assertion: if simpleSchema.toOpenAPI fails on nil
	// spec, the CRD never gets created and this times out.
	t.Log("waiting for ClusterMarker CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "clustermarkers.test.stdlib.kro.run", stdlibCRDTimeout),
		"CRD not established — simpleSchema.toOpenAPI likely failed on nil spec")
	t.Log("ClusterMarker CRD established")

	// Phase 3: Create a cluster-scoped instance (no namespace, no spec).
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "ClusterMarker",
		"metadata": map[string]any{
			"name": "my-marker",
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, instance))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), instance) })

	// Phase 4: Verify the Kind resource itself becomes ready (status subresource works).
	// The Kind controller writes status.ready + status.items to the Kind resource.
	t.Log("waiting for Kind to report ready...")
	kindKey := types.NamespacedName{Name: "clustermarker", Namespace: "kro-system"}
	require.NoError(t, waitForResource(ctx, k8sClient, kindKey, kind, stdlibReconcileTimeout))

	t.Log("Kind with nil spec works: schema compiled, CRD created, instance accepted")
}
