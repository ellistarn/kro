package graphcontroller

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/kubernetes-sigs/kro/experimental/deploy"
	"github.com/kubernetes-sigs/kro/experimental/stdlib"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Standard library tests
//
// The standard library is a set of reusable types built on Graph:
//
//   Kind      — define a new Kubernetes Kind (CRD + per-instance Graphs)
//   Decorator — watch instances of a Kind, attach resources per-instance
//   Singleton — declare a resource that should exist exactly once
//
// Graph is the core runtime. The stdlib types are Graphs that compose
// into higher-level behavior. These tests validate:
//   1. Implementation files compile through the real Graph compiler.
//   2. The CEL expressions produce correct results with mock data
//      (resolution logic, conflict detection, edge cases).
//   3. Embedded resources parse correctly.
// ═══════════════════════════════════════════════════════════════════════════════

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// stdlibExamplesDir returns the path to the interface examples.
func stdlibExamplesDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "examples", "stdlib")
}

// stdlibImplDir returns the path to the implementation controllers.
func stdlibImplDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "stdlib")
}

func splitYAMLDocs(data []byte) [][]byte {
	var docs [][]byte
	for _, part := range bytes.Split(data, []byte("\n---")) {
		docs = append(docs, part)
	}
	return docs
}

func parseGraphDocsFrom(t *testing.T, dir, filename string) []*GraphSpec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filename))
	require.NoError(t, err, "reading %s", filename)

	var specs []*GraphSpec
	for i, doc := range splitYAMLDocs(data) {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		var obj map[string]any
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			continue
		}
		if obj == nil {
			continue
		}
		kind, _ := obj["kind"].(string)
		if kind != "Graph" {
			continue
		}
		spec, err := extractGraphSpec(obj)
		if err != nil {
			continue
		}
		require.NotEmpty(t, spec.Nodes, "doc %d in %s has empty nodes", i, filename)
		specs = append(specs, spec)
	}
	return specs
}

func parseDocsByKindFrom(t *testing.T, dir, filename, targetKind string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filename))
	require.NoError(t, err)

	var docs []map[string]any
	for _, doc := range splitYAMLDocs(data) {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		var obj map[string]any
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			continue
		}
		if obj == nil {
			continue
		}
		kind, _ := obj["kind"].(string)
		if kind == targetKind {
			docs = append(docs, obj)
		}
	}
	return docs
}

// Convenience wrappers for the examples directory.
func parseGraphDocs(t *testing.T, filename string) []*GraphSpec {
	return parseGraphDocsFrom(t, stdlibExamplesDir(), filename)
}

func parseDocsByKind(t *testing.T, filename, targetKind string) []map[string]any {
	return parseDocsByKindFrom(t, stdlibExamplesDir(), filename, targetKind)
}

// evalIncludeWhen evaluates all includeWhen conditions for a node with
// the given item and items in scope. Returns true if all conditions pass.
func evalIncludeWhen(t *testing.T, compiled *compiledGraph, nodeID string, item map[string]any, items []any) bool {
	t.Helper()
	node := compiled.dag.Nodes[compiled.dag.Index[nodeID]]
	eval := &evaluator{
		compiled: compiled,
		scope: map[string]any{
			"items": items,
			"item":  item,
		},
	}
	result, err := eval.includeWhen(node.IncludeWhen)
	require.NoError(t, err)
	return result
}

// makeSingleton creates a Singleton CR with single-template API.
// resource is "apiVersion/Kind/namespace/name".
func makeSingleton(name string, priority int64, resource string) map[string]any {
	parts := strings.SplitN(resource, "/", 4)
	template := map[string]any{
		"apiVersion": parts[0],
		"kind":       parts[1],
		"metadata":   map[string]any{"namespace": parts[2], "name": parts[3]},
	}
	return map[string]any{
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"priority": priority,
			"template": template,
		},
	}
}

// makeSingletonClusterScoped creates a Singleton CR with a cluster-scoped resource
// (no namespace field in template metadata). resource is "apiVersion/Kind/name".
func makeSingletonClusterScoped(name string, priority int64, resource string) map[string]any {
	parts := strings.SplitN(resource, "/", 3)
	template := map[string]any{
		"apiVersion": parts[0],
		"kind":       parts[1],
		"metadata":   map[string]any{"name": parts[2]},
	}
	return map[string]any{
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"priority": priority,
			"template": template,
		},
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Kind Controller (stdlib/kind-controller.yaml)
//
// The Kind controller is a Graph that watches all Kinds, creates CRDs
// per Kind, watches instances, and creates per-instance Graphs.
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibKindController(t *testing.T) {
	specs := parseGraphDocsFrom(t, stdlibImplDir(), "kind.yaml")
	require.Len(t, specs, 1)

	compiled, err := compileGraphSpec(specs[0], nil)
	require.NoError(t, err)
	dag := compiled.dag

	// watchKinds should depend on kindCrd (includeWhen references it).
	assert.Contains(t, dag.Dependents["kindCrd"], dag.Index["watchKinds"])

	// controllers depends on watchKinds (forEach over watchKinds).
	assert.Contains(t, dag.Dependents["watchKinds"], dag.Index["controllers"])

	// controllers should have forEach.
	controllerNode := dag.Nodes[dag.Index["controllers"]]
	assert.NotNil(t, controllerNode.ForEach, "controllers should have forEach")

	t.Logf("Kind controller: %d nodes, %d levels", len(dag.Nodes), len(dag.Levels))
	for _, node := range dag.Nodes {
		t.Logf("  %s: %s", node.ID, dag.References[node.ID])
	}
}

func TestStdlibKindEscapeLevels(t *testing.T) {
	specs := parseGraphDocsFrom(t, stdlibImplDir(), "kind.yaml")
	require.Len(t, specs, 1)

	compiled, err := compileGraphSpec(specs[0], nil)
	require.NoError(t, err)

	// L1 expressions ($${...}) should not be compiled at L0.
	for expr := range compiled.programs {
		assert.NotContains(t, expr, "plural(k.spec.kind)", "L1 expression leaked into L0")
		assert.NotContains(t, expr, "simpleSchema.toOpenAPI", "L1 expression leaked into L0")
		assert.NotContains(t, expr, "k.spec.group", "L1 expression leaked into L0")
	}

	// L0 should compile: watchKinds (WatchKind), k.metadata.* (forEach variable).
	foundWatchKinds, foundKMeta := false, false
	for expr := range compiled.programs {
		if expr == "watchKinds" {
			foundWatchKinds = true
		}
		if expr == "k.metadata.name" || expr == "k.metadata.namespace" {
			foundKMeta = true
		}
	}
	assert.True(t, foundWatchKinds, "'watchKinds' should be compiled at L0")
	assert.True(t, foundKMeta, "'k.metadata.*' should be compiled at L0")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Decorator → Kind (stdlib/decorator.yaml)
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibDecoratorCompilation(t *testing.T) {
	t.Run("WatchKind Decorator gets implicit forEach", func(t *testing.T) {
		docs := parseDocsByKind(t, "2-decorator.yaml", "Decorator")
		require.GreaterOrEqual(t, len(docs), 1)

		doc := docs[0]
		spec := doc["spec"].(map[string]any)
		watch := spec["watch"].(map[string]any)
		rawNodes := spec["nodes"]
		nodes, err := parseNodeList(rawNodes)
		require.NoError(t, err)

		graphNodes := []Node{{ID: "items", Template: watch}}
		for _, n := range nodes {
			if n.ForEach == nil {
				n.ForEach = map[string]string{"item": "${items}"}
			}
			graphNodes = append(graphNodes, n)
		}
		graph := &GraphSpec{Nodes: graphNodes}
		compiled, err := compileGraphSpec(graph, nil)
		require.NoError(t, err)
		dag := compiled.dag

		assert.Equal(t, ReferenceWatchKind, dag.References["items"])
		assert.NotNil(t, dag.Nodes[dag.Index["policy"]].ForEach,
			"policy should have forEach after Decorator compilation")
	})

	t.Run("Watch Decorator has no forEach", func(t *testing.T) {
		docs := parseDocsByKind(t, "2-decorator.yaml", "Decorator")
		require.GreaterOrEqual(t, len(docs), 3)

		doc := docs[2]
		spec := doc["spec"].(map[string]any)
		watch := spec["watch"].(map[string]any)
		rawNodes := spec["nodes"]
		nodes, err := parseNodeList(rawNodes)
		require.NoError(t, err)

		graphNodes := []Node{{ID: "item", Template: watch}}
		graphNodes = append(graphNodes, nodes...)

		graph := &GraphSpec{Nodes: graphNodes}
		compiled, err := compileGraphSpec(graph, nil)
		require.NoError(t, err)
		dag := compiled.dag

		assert.Equal(t, ReferenceWatch, dag.References["item"])
		assert.Nil(t, dag.Nodes[dag.Index["derived"]].ForEach,
			"Watch Decorator should not add forEach")
	})

	t.Run("dependent nodes compile with cross-references", func(t *testing.T) {
		docs := parseDocsByKind(t, "2-decorator.yaml", "Decorator")
		require.GreaterOrEqual(t, len(docs), 2)

		doc := docs[1]
		spec := doc["spec"].(map[string]any)
		watch := spec["watch"].(map[string]any)
		rawNodes := spec["nodes"]
		nodes, err := parseNodeList(rawNodes)
		require.NoError(t, err)

		graphNodes := []Node{{ID: "items", Template: watch}}
		for _, n := range nodes {
			if n.ForEach == nil {
				n.ForEach = map[string]string{"item": "${items}"}
			}
			graphNodes = append(graphNodes, n)
		}
		graph := &GraphSpec{Nodes: graphNodes}
		compiled, err := compileGraphSpec(graph, nil)
		require.NoError(t, err)
		dag := compiled.dag

		policyDeps := dag.Nodes[dag.Index["policy"]].Dependencies
		assert.True(t, policyDeps["quota"], "policy should depend on quota")
		assert.True(t, policyDeps["items"], "policy should depend on items")
	})

	t.Run("explicit forEach is preserved", func(t *testing.T) {
		watch := map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"selector":   map[string]any{},
		}
		node := Node{
			ID:      "perNs",
			ForEach: map[string]string{"ns": "${items.map(i, i.metadata.namespace).distinct()}"},
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata":   map[string]any{"name": "monitor", "namespace": "${ns}"},
			},
		}

		graphNodes := []Node{{ID: "items", Template: watch}, node}
		graph := &GraphSpec{Nodes: graphNodes}
		compiled, err := compileGraphSpec(graph, nil)
		require.NoError(t, err)

		compiledNode := compiled.dag.Nodes[compiled.dag.Index["perNs"]]
		assert.Contains(t, compiledNode.ForEach, "ns",
			"explicit forEach variable 'ns' should be preserved")
		assert.NotContains(t, compiledNode.ForEach, "item",
			"implicit 'item' forEach should NOT be added")
	})

	t.Run("empty-string metadata.name routes to WatchKind path", func(t *testing.T) {
		watch := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": ""},
			"selector":   map[string]any{},
		}
		userNode := Node{
			ID: "policy",
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "derived", "namespace": "default"},
			},
		}

		graphNodes := []Node{{ID: "items", Template: watch}}
		node := userNode
		node.ForEach = map[string]string{"item": "${items}"}
		graphNodes = append(graphNodes, node)

		graph := &GraphSpec{Nodes: graphNodes}
		compiled, err := compileGraphSpec(graph, nil)
		require.NoError(t, err)

		compiledNode := compiled.dag.Nodes[compiled.dag.Index["policy"]]
		assert.NotNil(t, compiledNode.ForEach,
			"policy should have forEach (empty-name routes to WatchKind path)")
		assert.Contains(t, compiledNode.ForEach, "item",
			"forEach should use 'item' variable")
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// Singleton resolution (stdlib/singleton.yaml)
//
// Tests evaluate the actual includeWhen CEL expressions from the
// Singleton Kind's embedded Decorator with mock Singleton data.
// Singleton uses single-template API: spec.template instead of spec.nodes[].
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibSingletonResolution(t *testing.T) {
	// The Singleton Kind (singleton.yaml) has a node whose template is
	// a Decorator. Extract that Decorator template and compile it.
	docs := parseDocsByKindFrom(t, stdlibImplDir(), "singleton.yaml", "Kind")
	require.NotEmpty(t, docs)
	spec, _ := docs[0]["spec"].(map[string]any)
	rawNodes, _ := spec["nodes"]
	nodes, err := parseNodeList(rawNodes)
	require.NoError(t, err)
	require.NotEmpty(t, nodes)

	// The first node's template is the Decorator spec.
	decoratorTemplate := nodes[0].Template
	decoratorSpec, _ := decoratorTemplate["spec"].(map[string]any)
	watch, _ := decoratorSpec["watch"].(map[string]any)
	decoratorRawNodes, _ := decoratorSpec["nodes"]
	decoratorNodes, err := parseNodeList(decoratorRawNodes)
	require.NoError(t, err)

	graphNodes := []Node{{ID: "items", Template: watch}}
	var firstNodeID string
	for _, n := range decoratorNodes {
		if firstNodeID == "" {
			firstNodeID = n.ID
		}
		if n.ForEach == nil {
			n.ForEach = map[string]string{"item": "${items}"}
		}
		graphNodes = append(graphNodes, n)
	}
	graph := &GraphSpec{Nodes: graphNodes}
	compiled, err := compileGraphSpec(graph, nil)
	require.NoError(t, err)
	nodeID := firstNodeID

	t.Run("single Singleton always wins", func(t *testing.T) {
		s := makeSingleton("only-one", 100, "v1/ConfigMap/default/foo")
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, s, []any{s}))
	})

	t.Run("higher priority wins", func(t *testing.T) {
		a := makeSingleton("team-a", 100, "v1/ConfigMap/monitoring/dashboard")
		b := makeSingleton("team-b", 200, "v1/ConfigMap/monitoring/dashboard")
		items := []any{a, b}
		assert.False(t, evalIncludeWhen(t, compiled, nodeID, a, items),
			"team-a (100) should lose to team-b (200)")
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, b, items),
			"team-b (200) should win over team-a (100)")
	})

	t.Run("lower priority loses", func(t *testing.T) {
		a := makeSingleton("alpha", 500, "v1/ConfigMap/default/shared")
		b := makeSingleton("beta", 50, "v1/ConfigMap/default/shared")
		items := []any{a, b}
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, a, items),
			"alpha (500) should win")
		assert.False(t, evalIncludeWhen(t, compiled, nodeID, b, items),
			"beta (50) should lose")
	})

	t.Run("same priority tie broken by name", func(t *testing.T) {
		a := makeSingleton("alpha", 100, "v1/ConfigMap/default/shared")
		b := makeSingleton("beta", 100, "v1/ConfigMap/default/shared")
		items := []any{a, b}
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, a, items),
			"alpha wins tie (lexicographically lower)")
		assert.False(t, evalIncludeWhen(t, compiled, nodeID, b, items),
			"beta loses tie")
	})

	t.Run("different resources both win", func(t *testing.T) {
		a := makeSingleton("logging", 100, "v1/ConfigMap/monitoring/fluentd")
		b := makeSingleton("metrics", 200, "v1/ConfigMap/monitoring/prometheus")
		items := []any{a, b}
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, a, items),
			"logging should win (different resource)")
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, b, items),
			"metrics should win (different resource)")
	})

	t.Run("three-way conflict highest wins", func(t *testing.T) {
		a := makeSingleton("team-a", 100, "v1/ConfigMap/default/config")
		b := makeSingleton("team-b", 200, "v1/ConfigMap/default/config")
		c := makeSingleton("team-c", 300, "v1/ConfigMap/default/config")
		items := []any{a, b, c}
		assert.False(t, evalIncludeWhen(t, compiled, nodeID, a, items), "100 loses")
		assert.False(t, evalIncludeWhen(t, compiled, nodeID, b, items), "200 loses")
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, c, items), "300 wins")
	})

	t.Run("different GVK same name not conflicting", func(t *testing.T) {
		a := makeSingleton("sa", 100, "v1/ServiceAccount/default/monitor")
		b := makeSingleton("cm", 100, "v1/ConfigMap/default/monitor")
		items := []any{a, b}
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, a, items),
			"ServiceAccount and ConfigMap are different GVKs")
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, b, items))
	})

	t.Run("different namespace not conflicting", func(t *testing.T) {
		a := makeSingleton("prod", 100, "v1/ConfigMap/production/config")
		b := makeSingleton("staging", 100, "v1/ConfigMap/staging/config")
		items := []any{a, b}
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, a, items),
			"same name in different namespaces should not conflict")
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, b, items))
	})

	t.Run("many Singletons one resource", func(t *testing.T) {
		items := make([]any, 10)
		for i := range items {
			items[i] = makeSingleton(
				fmt.Sprintf("s%02d", i),
				int64(i*10),
				"v1/ConfigMap/default/contested",
			)
		}
		for i, item := range items {
			result := evalIncludeWhen(t, compiled, nodeID, item.(map[string]any), items)
			if i == 9 {
				assert.True(t, result, "s09 (highest) should win")
			} else {
				assert.False(t, result, "s%02d should lose", i)
			}
		}
	})

	t.Run("negative priority", func(t *testing.T) {
		a := makeSingleton("fallback", -100, "v1/ConfigMap/default/config")
		b := makeSingleton("override", -1, "v1/ConfigMap/default/config")
		items := []any{a, b}
		assert.False(t, evalIncludeWhen(t, compiled, nodeID, a, items),
			"fallback (-100) should lose to override (-1)")
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, b, items),
			"override (-1) should win")
	})

	t.Run("zero priority", func(t *testing.T) {
		a := makeSingleton("alpha", 0, "v1/ConfigMap/default/config")
		b := makeSingleton("beta", 0, "v1/ConfigMap/default/config")
		items := []any{a, b}
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, a, items),
			"alpha wins tie at priority 0 (lexicographically lower)")
		assert.False(t, evalIncludeWhen(t, compiled, nodeID, b, items),
			"beta loses tie at priority 0")
	})

	t.Run("cluster-scoped resource no namespace", func(t *testing.T) {
		a := makeSingletonClusterScoped("role-a", 100, "rbac.authorization.k8s.io/v1/ClusterRole/admin")
		b := makeSingletonClusterScoped("role-b", 200, "rbac.authorization.k8s.io/v1/ClusterRole/admin")
		items := []any{a, b}
		assert.False(t, evalIncludeWhen(t, compiled, nodeID, a, items),
			"role-a (100) should lose to role-b (200)")
		assert.True(t, evalIncludeWhen(t, compiled, nodeID, b, items),
			"role-b (200) should win")
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// Interface file structural validation
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibInterfaceFiles(t *testing.T) {
	cases := []struct {
		file    string
		kind    string
		minDocs int
	}{
		{"1-kind.yaml", "Kind", 1},
		{"2-decorator.yaml", "Decorator", 3},
		{"3-singleton.yaml", "Singleton", 3},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			docs := parseDocsByKind(t, tc.file, tc.kind)
			assert.GreaterOrEqual(t, len(docs), tc.minDocs,
				"expected at least %d %s documents", tc.minDocs, tc.kind)

			for i, doc := range docs {
				meta, _ := doc["metadata"].(map[string]any)
				name, _ := meta["name"].(string)
				assert.NotEmpty(t, name, "doc %d should have metadata.name", i)

				_, ok := doc["spec"].(map[string]any)
				assert.True(t, ok, "doc %d should have spec", i)

				t.Logf("  %s/%s", tc.kind, name)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// CEL patterns used by the standard library
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibSizeGuard(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{
				ID: "webapps",
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "WebApp",
					"selector":   map[string]any{},
				},
			},
			{
				ID:          "dashboard",
				IncludeWhen: []string{"${webapps.size() > 0}"},
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "webapp-dashboard", "namespace": "monitoring"},
					"data":       map[string]any{},
				},
			},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)
	found := false
	for expr := range compiled.programs {
		if expr == "webapps.size() > 0" {
			found = true
		}
	}
	assert.True(t, found, "webapps.size() > 0 should be compiled")
}

func TestStdlibDistinctCEL(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{
				ID: "items",
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "Pod",
					"selector":   map[string]any{"app": "monitoring"},
				},
			},
			{
				ID:      "sa",
				ForEach: map[string]string{"ns": "${items.map(i, i.metadata.namespace).distinct()}"},
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ServiceAccount",
					"metadata":   map[string]any{"name": "monitoring", "namespace": "${ns}"},
				},
			},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)
	found := false
	for expr := range compiled.programs {
		if expr == "items.map(i, i.metadata.namespace).distinct()" {
			found = true
		}
	}
	assert.True(t, found, "distinct() expression should be compiled")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Kind controller Graph compiles
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibKindGraphCompiles(t *testing.T) {
	specs := parseGraphDocsFrom(t, stdlibImplDir(), "kind.yaml")
	require.Len(t, specs, 1, "kind.yaml should contain exactly one Graph")

	compiled, err := compileGraphSpec(specs[0], nil)
	require.NoError(t, err, "stdlib graph should compile")
	dag := compiled.dag
	assert.NotEmpty(t, dag.TopologicalOrder, "stdlib graph should have nodes")
	t.Logf("stdlib graph: %d nodes, %d levels", len(dag.Nodes), len(dag.Levels))
	for _, node := range dag.Nodes {
		t.Logf("  %s: %s", node.ID, dag.References[node.ID])
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Embedded resources parse correctly
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibEmbeddedCRDsParse(t *testing.T) {
	entries, err := fs.ReadDir(deploy.CRDs, ".")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(entries), 2, "expected at least 2 CRD files (Graph, GraphRevision)")

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := fs.ReadFile(deploy.CRDs, entry.Name())
			require.NoError(t, err)

			var obj map[string]any
			require.NoError(t, yaml.Unmarshal(data, &obj))
			assert.Equal(t, "CustomResourceDefinition", obj["kind"])

			meta, _ := obj["metadata"].(map[string]any)
			name, _ := meta["name"].(string)
			assert.NotEmpty(t, name)
			t.Logf("  CRD: %s", name)
		})
	}
}

func TestStdlibEmbeddedResourcesParse(t *testing.T) {
	entries, err := fs.ReadDir(stdlib.Resources, ".")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(entries), 3, "expected at least 3 stdlib files")

	expectedKinds := map[string]string{
		"kind.yaml":      "Graph",
		"decorator.yaml": "Kind",
		"singleton.yaml": "Kind",
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := fs.ReadFile(stdlib.Resources, entry.Name())
			require.NoError(t, err)

			var foundKind string
			for _, doc := range splitYAMLDocs(data) {
				doc = bytes.TrimSpace(doc)
				if len(doc) == 0 {
					continue
				}
				var obj map[string]any
				if err := yaml.Unmarshal(doc, &obj); err != nil || obj == nil {
					continue
				}
				kind, _ := obj["kind"].(string)
				foundKind = kind
				meta, _ := obj["metadata"].(map[string]any)
				name, _ := meta["name"].(string)
				t.Logf("  %s (kind: %s)", name, kind)
			}
			if expected, ok := expectedKinds[entry.Name()]; ok {
				assert.Equal(t, expected, foundKind, "%s should be kind %s", entry.Name(), expected)
			}
		})
	}
}
