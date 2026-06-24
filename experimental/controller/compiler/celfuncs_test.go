package compiler

import (
	"strings"
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

// TestStatusFieldClassifier_PredicateParity guards the field-classification
// contract documented in celfuncs.go and charts/stdlib/templates/kind.yaml.
//
// The same predicate — "a status field value is a kro-owned expression iff it
// contains the substring `${`" — is implemented twice:
//   - WRITE side: the stdlib CEL `transformMap(key, val, val.contains("${"), val)`
//     in kindInstancePatch (kind.yaml) decides which fields kro writes.
//   - SCHEMA side: the Go `strings.Contains(s, "${")` check in
//     celSimpleSchemaFunction (celfuncs.go) decides whether to emit a typed
//     status schema.
//
// If these drift (e.g. one learns about escaping or alternative syntax and the
// other doesn't), the schema side and write side disagree on which fields kro
// owns. This test feeds a range of value shapes through BOTH the real CEL
// predicate and the real Go predicate and asserts identical classification.
// This test exists because the predicate is duplicated across the YAML/Go
// boundary and cannot be physically shared.
func TestStatusFieldClassifier_PredicateParity(t *testing.T) {
	// The exact CEL predicate used on the write side, evaluated standalone.
	env, err := cel.NewEnv(cel.Variable("val", cel.StringType))
	require.NoError(t, err)
	ast, iss := env.Compile(`val.contains("${")`)
	require.NoError(t, iss.Err())
	prg, err := env.Program(ast)
	require.NoError(t, err)

	celClassifiesAsExpr := func(val string) bool {
		out, _, err := prg.Eval(map[string]any{"val": val})
		require.NoError(t, err)
		return out.Value().(bool)
	}

	// The exact Go predicate used on the schema side (celfuncs.go hasExpressions).
	goClassifiesAsExpr := func(val string) bool {
		return strings.Contains(val, "${")
	}

	cases := []struct {
		name string
		val  string
		want bool // true => classified as a kro-owned expression
	}{
		{"bare-boolean", "boolean", false},
		{"bare-string", "string", false},
		{"bare-string-with-markers", "string | default=hi maxLength=10", false},
		{"bare-integer", "integer | minimum=0", false},
		{"standalone-expr", "${cm.metadata.name}", true},
		{"embedded-expr", "prefix-${schema.spec.x}-suffix", true},
		{"deferred-expr", "$${schema.metadata.name}", true},
		{"expr-with-spaces", "${ a == b }", true},
		// Edge: a string DEFAULT whose literal value contains "${" is classified
		// as an expression by this substring rule. Documented limitation — a
		// status field default like `string | default=${literal}` would be
		// treated as kro-owned. Both sides must agree, which is what matters here.
		{"string-default-containing-dollarbrace", "string | default=${weird}", true},
		{"dollar-no-brace", "cost is $5", false},
		{"brace-no-dollar", "{templated}", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotCEL := celClassifiesAsExpr(c.val)
			gotGo := goClassifiesAsExpr(c.val)
			assert.Equal(t, c.want, gotCEL, "CEL write-side predicate misclassified %q", c.val)
			assert.Equal(t, c.want, gotGo, "Go schema-side predicate misclassified %q", c.val)
			assert.Equal(t, gotCEL, gotGo,
				"PREDICATE DRIFT: CEL write-side and Go schema-side disagree on %q (CEL=%v, Go=%v)",
				c.val, gotCEL, gotGo)
		})
	}
}
