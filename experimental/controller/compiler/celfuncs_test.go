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
