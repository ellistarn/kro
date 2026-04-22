package graphcontroller

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestIsKroFieldManager(t *testing.T) {
	tests := []struct {
		name    string
		manager string
		want    bool
	}{
		{"kro field manager", "my-app.default.internal.kro.run", true},
		{"non-kro manager", "some-manager", false},
		{"kube-apiserver", "kube-apiserver", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isKroFieldManager(tt.manager))
		})
	}
}

func TestCoOwnershipCheck(t *testing.T) {
	tests := []struct {
		name            string
		managedFields   []metav1.ManagedFieldsEntry
		ownFieldManager string
		wantErr         bool
		wantConflict    bool
		wantSubstr      string
	}{
		{
			name:            "no managed fields",
			managedFields:   nil,
			ownFieldManager: "my-app.default.internal.kro.run",
		},
		{
			name: "only our own manager",
			managedFields: []metav1.ManagedFieldsEntry{{
				Manager: "my-app.default.internal.kro.run", Operation: metav1.ManagedFieldsOperationApply,
				FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:key1":{}}}`)},
			}},
			ownFieldManager: "my-app.default.internal.kro.run",
		},
		{
			name: "our manager + non-kro manager with overlapping fields",
			managedFields: []metav1.ManagedFieldsEntry{
				{Manager: "my-app.default.internal.kro.run", Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:key1":{}}}`)},
				},
				{Manager: "some-external-controller", Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:key1":{}}}`)},
				},
			},
			ownFieldManager: "my-app.default.internal.kro.run",
		},
		{
			name: "our manager + other kro manager, disjoint fields",
			managedFields: []metav1.ManagedFieldsEntry{
				{Manager: "my-app.default.internal.kro.run", Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{".":{},"f:labels":{".":{},"f:key-a":{}}}}`)},
				},
				{Manager: "other-app.default.internal.kro.run", Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:key-b":{}}}`)},
				},
			},
			ownFieldManager: "my-app.default.internal.kro.run",
		},
		{
			name: "our manager + other kro manager, overlapping fields → Conflict",
			managedFields: []metav1.ManagedFieldsEntry{
				{Manager: "my-app.default.internal.kro.run", Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:shared-key":{}}}`)},
				},
				{Manager: "other-app.default.internal.kro.run", Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:shared-key":{}}}`)},
				},
			},
			ownFieldManager: "my-app.default.internal.kro.run",
			wantErr:         true,
			wantConflict:    true,
			wantSubstr:      "other-app.default.internal.kro.run",
		},
		{
			name: "API server Update manager ignored",
			managedFields: []metav1.ManagedFieldsEntry{
				{Manager: "my-app.default.internal.kro.run", Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:key1":{}}}`)},
				},
				{Manager: "kube-apiserver", Operation: metav1.ManagedFieldsOperationUpdate,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:key1":{}}}`)},
				},
			},
			ownFieldManager: "my-app.default.internal.kro.run",
		},
		{
			name: "multiple other kro managers → first overlap wins",
			managedFields: []metav1.ManagedFieldsEntry{
				{Manager: "my-app.default.internal.kro.run", Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:shared":{}}}`)},
				},
				{Manager: "app-a.default.internal.kro.run", Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:shared":{}}}`)},
				},
				{Manager: "app-b.default.internal.kro.run", Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{},"f:shared":{}}}`)},
				},
			},
			ownFieldManager: "my-app.default.internal.kro.run",
			wantErr:         true,
			wantConflict:    true,
			wantSubstr:      "app-a.default.internal.kro.run",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "test", "namespace": "default"},
			}}
			obj.SetManagedFields(tt.managedFields)

			err := coOwnershipCheck(obj, tt.ownFieldManager)
			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err)
			if tt.wantConflict {
				assert.True(t, errors.Is(err, ErrFieldConflict), "error should wrap ErrFieldConflict, got: %v", err)
			}
			if tt.wantSubstr != "" {
				assert.Contains(t, err.Error(), tt.wantSubstr)
			}
		})
	}
}
