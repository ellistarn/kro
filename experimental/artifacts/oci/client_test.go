package oci

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseReference(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Reference
		wantErr bool
	}{
		{
			name:  "simple tag",
			input: "example.com/repo:v1.0.0",
			want:  Reference{Registry: "example.com", Repository: "repo", Tag: "v1.0.0"},
		},
		{
			name:  "nested repository",
			input: "example.com/org/repo/sub:latest",
			want:  Reference{Registry: "example.com", Repository: "org/repo/sub", Tag: "latest"},
		},
		{
			name:  "digest reference",
			input: "example.com/repo@sha256:abc123",
			want:  Reference{Registry: "example.com", Repository: "repo", Digest: "sha256:abc123"},
		},
		{
			name:  "ECR style",
			input: "123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v1.0.0",
			want:  Reference{Registry: "123456789.dkr.ecr.us-west-2.amazonaws.com", Repository: "graphs/networking", Tag: "v1.0.0"},
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "no slash",
			input:   "example.com",
			wantErr: true,
		},
		{
			name:    "no tag or digest",
			input:   "example.com/repo",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseReference(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReferenceString(t *testing.T) {
	assert.Equal(t, "example.com/repo:v1.0.0",
		Reference{Registry: "example.com", Repository: "repo", Tag: "v1.0.0"}.String())
	assert.Equal(t, "example.com/repo@sha256:abc123",
		Reference{Registry: "example.com", Repository: "repo", Digest: "sha256:abc123"}.String())
}
