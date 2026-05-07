package graphcontroller

import (
	"encoding/json"
	"hash/fnv"
	"strconv"
	"testing"
)

// jsonFNV64a computes the hash the old way: json.Marshal + FNV-64a.
func jsonFNV64a(obj map[string]any) uint64 {
	data, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	h := fnv.New64a()
	h.Write(data)
	return h.Sum64()
}

func TestHashObject_Determinism(t *testing.T) {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "test",
			"namespace": "default",
		},
		"data": map[string]any{
			"key1": "value1",
			"key2": "value2",
		},
	}

	h1 := HashObject(obj)
	h2 := HashObject(obj)
	if h1 != h2 {
		t.Fatalf("same input produced different hashes: %d vs %d", h1, h2)
	}
}

func TestHashObject_DifferentInputs(t *testing.T) {
	obj1 := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": "test1",
		},
	}
	obj2 := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": "test2",
		},
	}

	h1 := HashObject(obj1)
	h2 := HashObject(obj2)
	if h1 == h2 {
		t.Fatalf("different inputs produced same hash: %d", h1)
	}
}

func TestHashObject_MatchesJSONMarshal(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
	}{
		{
			name: "simple flat map",
			obj: map[string]any{
				"a": "hello",
				"b": float64(42),
				"c": true,
			},
		},
		{
			name: "nested maps",
			obj: map[string]any{
				"metadata": map[string]any{
					"name":      "foo",
					"namespace": "bar",
					"labels": map[string]any{
						"app": "test",
					},
				},
			},
		},
		{
			name: "slices",
			obj: map[string]any{
				"items": []any{
					"one", "two", "three",
				},
				"nested": []any{
					map[string]any{"x": float64(1)},
					map[string]any{"x": float64(2)},
				},
			},
		},
		{
			name: "nil values",
			obj: map[string]any{
				"present": "yes",
				"absent":  nil,
			},
		},
		{
			name: "booleans",
			obj: map[string]any{
				"enabled":  true,
				"disabled": false,
			},
		},
		{
			name: "numbers",
			obj: map[string]any{
				"integer": float64(100),
				"zero":    float64(0),
				"neg":     float64(-3.14),
				"large":   float64(1e18),
			},
		},
		{
			name: "empty containers",
			obj: map[string]any{
				"emptyMap":   map[string]any{},
				"emptySlice": []any{},
			},
		},
		{
			name: "string escaping",
			obj: map[string]any{
				"quoted":   `he said "hello"`,
				"newline":  "line1\nline2",
				"tab":      "col1\tcol2",
				"backslash": `path\to\file`,
			},
		},
		{
			name: "realistic configmap",
			obj: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "app-config",
					"namespace": "production",
					"labels": map[string]any{
						"app.kubernetes.io/name":      "myapp",
						"app.kubernetes.io/version":   "1.2.3",
						"app.kubernetes.io/component": "backend",
					},
					"annotations": map[string]any{
						"kro.run/graph": "my-graph",
					},
				},
				"data": map[string]any{
					"DATABASE_URL": "postgres://db:5432/app",
					"REDIS_URL":    "redis://cache:6379",
					"LOG_LEVEL":    "info",
				},
			},
		},
		{
			name: "deeply nested",
			obj: map[string]any{
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name":  "app",
									"image": "nginx:1.21",
									"ports": []any{
										map[string]any{
											"containerPort": float64(8080),
											"protocol":      "TCP",
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "mixed slice types",
			obj: map[string]any{
				"args": []any{
					"--port=8080",
					float64(42),
					true,
					nil,
					map[string]any{"key": "val"},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HashObject(tc.obj)
			want := jsonFNV64a(tc.obj)
			if got != want {
				t.Errorf("hash mismatch:\n  tree-walk: %d\n  json+fnv:  %d", got, want)
			}
		})
	}
}

func TestHashObject_KeyOrderIndependence(t *testing.T) {
	// Build two maps with same keys inserted in different order.
	// Go maps don't guarantee iteration order, but sorted keys
	// should produce the same serialization regardless.
	obj1 := map[string]any{
		"z": "last",
		"a": "first",
		"m": "middle",
	}
	obj2 := map[string]any{
		"a": "first",
		"m": "middle",
		"z": "last",
	}

	h1 := HashObject(obj1)
	h2 := HashObject(obj2)
	if h1 != h2 {
		t.Fatalf("maps with same content got different hashes: %d vs %d", h1, h2)
	}
}

func TestHashObject_CollisionResistance(t *testing.T) {
	// Ensure that key/value boundaries don't collide.
	// "ab":"c" vs "a":"bc" should produce different hashes.
	obj1 := map[string]any{"ab": "c"}
	obj2 := map[string]any{"a": "bc"}

	h1 := HashObject(obj1)
	h2 := HashObject(obj2)
	if h1 == h2 {
		t.Fatalf("collision between {ab:c} and {a:bc}: %d", h1)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkHashObject_TreeWalk(b *testing.B) {
	obj := buildBenchObject()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		HashObject(obj)
	}
}

func BenchmarkHashObject_JSONMarshal(b *testing.B) {
	obj := buildBenchObject()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		jsonFNV64a(obj)
	}
}

func buildBenchObject() map[string]any {
	containers := make([]any, 3)
	for i := range containers {
		containers[i] = map[string]any{
			"name":  "container-" + strconv.Itoa(i),
			"image": "nginx:1.21",
			"ports": []any{
				map[string]any{
					"containerPort": float64(8080 + i),
					"protocol":      "TCP",
				},
			},
			"env": []any{
				map[string]any{"name": "FOO", "value": "bar"},
				map[string]any{"name": "BAZ", "value": "qux"},
			},
		}
	}
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      "bench-deploy",
			"namespace": "default",
			"labels": map[string]any{
				"app":     "bench",
				"version": "v1",
			},
		},
		"spec": map[string]any{
			"replicas": float64(3),
			"selector": map[string]any{
				"matchLabels": map[string]any{
					"app": "bench",
				},
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"app": "bench",
					},
				},
				"spec": map[string]any{
					"containers": containers,
				},
			},
		},
	}
}

