package compiler

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
)

// evalSimpleSchema creates a CEL environment with simpleSchema.toOpenAPI registered,
// evaluates it with the given schema map and resources list, and returns the result
// as a map[string]any.
func evalSimpleSchema(t *testing.T, schema map[string]any, resources []any) map[string]any {
	t.Helper()

	env, err := cel.NewEnv(
		append(celSimpleSchemaFunction(),
			cel.Variable("schema", cel.DynType),
			cel.Variable("resources", cel.DynType),
		)...,
	)
	require.NoError(t, err)

	ast, issues := env.Compile(`simpleSchema.toOpenAPI(schema, resources)`)
	require.NoError(t, issues.Err())

	prg, err := env.Program(ast)
	require.NoError(t, err)

	out, _, err := prg.Eval(map[string]any{
		"schema":    schema,
		"resources": resources,
	})
	require.NoError(t, err, "CEL evaluation failed — got error type: %T, value: %v", out, out)

	native, err := conversion.GoNativeType(out)
	require.NoError(t, err)

	result, ok := native.(map[string]any)
	require.True(t, ok, "expected map[string]any, got %T", native)
	return result
}

func TestSimpleSchemaToOpenAPI_NilSpec(t *testing.T) {
	// A Kind that declares no spec fields — only apiVersion/kind.
	// This is valid: it produces a CRD whose instances have no user-facing spec.
	schema := map[string]any{
		"apiVersion": "mygroup.io/v1",
		"kind":       "MyClusterThing",
	}

	result := evalSimpleSchema(t, schema, []any{})

	// Should produce a valid OpenAPI schema with the standard structure
	assert.Equal(t, "object", result["type"])

	props, ok := result["properties"].(map[string]any)
	require.True(t, ok, "expected properties map")

	// Standard Kubernetes fields must be present
	assert.Contains(t, props, "apiVersion")
	assert.Contains(t, props, "kind")
	assert.Contains(t, props, "metadata")
	assert.Contains(t, props, "spec")
	assert.Contains(t, props, "status")

	// spec should be a valid object schema (empty — no user fields)
	specSchema, ok := props["spec"].(map[string]any)
	require.True(t, ok, "spec should be a map")
	assert.Equal(t, "object", specSchema["type"])

	// status should have conditions with list-map semantics
	statusSchema, ok := props["status"].(map[string]any)
	require.True(t, ok, "status should be a map")
	assert.Equal(t, "object", statusSchema["type"])
}

func TestSimpleSchemaToOpenAPI_EmptySpec(t *testing.T) {
	// A Kind that explicitly declares spec: {} — same outcome as nil spec.
	schema := map[string]any{
		"apiVersion": "mygroup.io/v1",
		"kind":       "MyClusterThing",
		"spec":       map[string]any{},
	}

	result := evalSimpleSchema(t, schema, []any{})

	assert.Equal(t, "object", result["type"])

	props, ok := result["properties"].(map[string]any)
	require.True(t, ok)

	// spec should be a valid empty object
	specSchema, ok := props["spec"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", specSchema["type"])
}

func TestSimpleSchemaToOpenAPI_WithSpecFields(t *testing.T) {
	// Baseline: a Kind with actual spec fields works as expected.
	schema := map[string]any{
		"apiVersion": "mygroup.io/v1",
		"kind":       "MyApp",
		"spec": map[string]any{
			"replicas": "integer",
			"image":    "string",
		},
	}

	result := evalSimpleSchema(t, schema, []any{})

	props := result["properties"].(map[string]any)
	specSchema := props["spec"].(map[string]any)
	assert.Equal(t, "object", specSchema["type"])

	specProps, ok := specSchema["properties"].(map[string]any)
	require.True(t, ok, "spec should have properties")
	assert.Contains(t, specProps, "replicas")
	assert.Contains(t, specProps, "image")
}

// ---------------------------------------------------------------------------
// Singleton identity formula tests
//
// The singleton uses an identity string to match peers targeting the same
// resource. The formula must handle both namespaced and cluster-scoped
// targets. For cluster-scoped resources, metadata.namespace is absent from
// the stored map entirely (not empty string — absent), so has() is required
// to avoid a CEL "no such key" runtime error.
// ---------------------------------------------------------------------------

func TestSingletonIdentityFormula(t *testing.T) {
	env, err := cel.NewEnv(cel.Variable("s", cel.DynType))
	require.NoError(t, err)

	// Same formula used in singleton.yaml for both identities[] and identity.value
	expr := `s.apiVersion
		+ (has(s.metadata.namespace)
		   ? "/namespaces/" + s.metadata.namespace
		   : "")
		+ "/" + s.kind
		+ "/" + s.metadata.name`

	ast, issues := env.Compile(expr)
	require.NoError(t, issues.Err())
	prg, err := env.Program(ast)
	require.NoError(t, err)

	tests := []struct {
		name     string
		template map[string]any
		want     string
	}{
		{
			name: "namespaced resource",
			template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "contested", "namespace": "default"},
			},
			want: "v1/namespaces/default/ConfigMap/contested",
		},
		{
			name: "cluster-scoped resource (namespace absent)",
			template: map[string]any{
				"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind":       "ClusterRoleBinding",
				"metadata":   map[string]any{"name": "platform-admin"},
			},
			want: "rbac.authorization.k8s.io/v1/ClusterRoleBinding/platform-admin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := prg.Eval(map[string]any{"s": tt.template})
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.Value().(string))
		})
	}
}
