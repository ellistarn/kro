package compiler

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeOCIName(t *testing.T) {
	tests := []struct {
		name string
		uri  string
	}{
		{
			name: "ecr tag",
			uri:  "123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v1.0.0",
		},
		{
			name: "ecr digest",
			uri:  "123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking@sha256:a3f2b7c4e5d6f7890abcdef1234567890abcdef1234567890abcdef12345678",
		},
		{
			name: "nested repo",
			uri:  "123456789.dkr.ecr.us-west-2.amazonaws.com/org/team/graphs/networking:latest",
		},
		{
			name: "short tag",
			uri:  "example.com/repo:v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := EncodeOCIName(tt.uri)

			// Encoded name must be DNS-compliant: only [a-z2-7]
			for _, c := range encoded {
				assert.True(t, (c >= 'a' && c <= 'z') || (c >= '2' && c <= '7'),
					"character %c is not base32", c)
			}

			// Must be under 253 characters (DNS name limit)
			assert.LessOrEqual(t, len(encoded), 253)

			// Round-trip
			decoded, err := DecodeOCIName(encoded)
			require.NoError(t, err)
			assert.Equal(t, tt.uri, decoded)
		})
	}
}

func TestEncodeOCIName_Deterministic(t *testing.T) {
	uri := "123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v1.0.0"
	a := EncodeOCIName(uri)
	b := EncodeOCIName(uri)
	assert.Equal(t, a, b)
}

func TestDecodeOCIName_Invalid(t *testing.T) {
	_, err := DecodeOCIName("not-valid-base32!!!")
	assert.Error(t, err)
}

func TestCelOCIFunction(t *testing.T) {
	env, err := cel.NewEnv(celOCIFunction()...)
	require.NoError(t, err)

	ast, issues := env.Compile(`oci("example.com/repo:v1.0.0")`)
	require.NoError(t, issues.Err())

	prg, err := env.Program(ast)
	require.NoError(t, err)

	out, _, err := prg.Eval(cel.NoVars())
	require.NoError(t, err)

	result := out.Value().(string)
	expected := EncodeOCIName("example.com/repo:v1.0.0")
	assert.Equal(t, expected, result)

	// Verify round-trip through decode
	decoded, err := DecodeOCIName(result)
	require.NoError(t, err)
	assert.Equal(t, "example.com/repo:v1.0.0", decoded)
}

func TestCelOCIFunction_Empty(t *testing.T) {
	env, err := cel.NewEnv(celOCIFunction()...)
	require.NoError(t, err)

	ast, issues := env.Compile(`oci("")`)
	require.NoError(t, issues.Err())

	prg, err := env.Program(ast)
	require.NoError(t, err)

	_, _, err = prg.Eval(cel.NoVars())
	assert.Error(t, err) // empty URI should error
}

func TestCelOCIFunction_Concatenation(t *testing.T) {
	env, err := cel.NewEnv(
		append(celOCIFunction(), cel.Variable("registry", cel.StringType), cel.Variable("version", cel.StringType))...,
	)
	require.NoError(t, err)

	ast, issues := env.Compile(`oci(registry + "/graphs/networking:" + version)`)
	require.NoError(t, issues.Err())

	prg, err := env.Program(ast)
	require.NoError(t, err)

	out, _, err := prg.Eval(map[string]any{
		"registry": "123456789.dkr.ecr.us-west-2.amazonaws.com",
		"version":  "v2.1.0",
	})
	require.NoError(t, err)

	result := out.Value().(string)
	decoded, err := DecodeOCIName(result)
	require.NoError(t, err)
	assert.Equal(t, "123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v2.1.0", decoded)
}
