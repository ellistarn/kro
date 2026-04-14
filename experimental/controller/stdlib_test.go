package graphcontroller

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Standard library types and compilers
//
// Each type compiles to the type below it:
//   Package → Singleton (semver → priority resolution)
//   Singleton → Graph (priority-based winner selection)
//   Decorator → Graph (WatchKind/Watch + implicit forEach)
//   GraphDefinition → Graph (WatchKind + forEach + CRD)
// ═══════════════════════════════════════════════════════════════════════════════

// --- Types ---

type DecoratorSpec struct {
	Watch map[string]any // same structure as Graph node template
	Nodes []Node
}

type SingletonSpec struct {
	Name     string // from metadata.name
	Priority int
	Nodes    []Node
}

type PackageSpec struct {
	Name    string // from metadata.name
	Version string // semver: v1.2.3
	Nodes   []Node
}

type semver struct {
	Major, Minor, Patch int
}

// --- Parsers (from YAML map[string]any) ---

func extractDecoratorSpec(obj map[string]any) (*DecoratorSpec, error) {
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing spec")
	}
	watch, ok := spec["watch"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing spec.watch")
	}
	rawNodes, ok := spec["nodes"]
	if !ok {
		return nil, fmt.Errorf("missing spec.nodes")
	}
	nodes, err := parseNodeList(rawNodes)
	if err != nil {
		return nil, err
	}
	return &DecoratorSpec{Watch: watch, Nodes: nodes}, nil
}

func extractSingletonSpec(obj map[string]any) (*SingletonSpec, error) {
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing spec")
	}
	// Priority may come as int or float64 from YAML.
	var priority int
	switch p := spec["priority"].(type) {
	case int:
		priority = p
	case int64:
		priority = int(p)
	case float64:
		priority = int(p)
	}
	rawNodes, ok := spec["nodes"]
	if !ok {
		return nil, fmt.Errorf("missing spec.nodes")
	}
	nodes, err := parseNodeList(rawNodes)
	if err != nil {
		return nil, err
	}
	return &SingletonSpec{Name: name, Priority: priority, Nodes: nodes}, nil
}

func extractPackageSpec(obj map[string]any) (*PackageSpec, error) {
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing spec")
	}
	version, _ := spec["version"].(string)
	if version == "" {
		return nil, fmt.Errorf("missing spec.version")
	}
	rawNodes, ok := spec["nodes"]
	if !ok {
		return nil, fmt.Errorf("missing spec.nodes")
	}
	nodes, err := parseNodeList(rawNodes)
	if err != nil {
		return nil, err
	}
	return &PackageSpec{Name: name, Version: version, Nodes: nodes}, nil
}

// --- Compilers ---

// compileDecorator translates a Decorator into a Graph.
//
//	WatchKind (no metadata.name):
//	  → WatchKind node `items` + forEach { item: ${items} } on each node
//	Watch (has metadata.name):
//	  → Watch node `item` + nodes reference ${item.*}
func compileDecorator(dec *DecoratorSpec) (*GraphSpec, error) {
	md, _ := dec.Watch["metadata"].(map[string]any)
	_, hasName := md["name"]

	if hasName {
		// Watch (single resource) — `item` node + side-effect nodes.
		watchNode := Node{
			ID:       "item",
			Template: dec.Watch,
		}
		nodes := []Node{watchNode}
		nodes = append(nodes, dec.Nodes...)
		return &GraphSpec{Nodes: nodes}, nil
	}

	// WatchKind — `items` node + forEach { item: ${items} } on each node.
	watchNode := Node{
		ID:       "items",
		Template: dec.Watch,
	}

	var forEachNodes []Node
	for _, n := range dec.Nodes {
		compiled := Node{
			ID:          n.ID,
			Template:    n.Template,
			IncludeWhen: n.IncludeWhen,
			ForEach:     map[string]string{"item": "${items}"},
		}
		forEachNodes = append(forEachNodes, compiled)
	}

	nodes := []Node{watchNode}
	nodes = append(nodes, forEachNodes...)
	return &GraphSpec{Nodes: nodes}, nil
}

// resolveSingletons takes a set of Singletons competing for the same
// resources and returns the winner (highest priority, name as tiebreaker).
func resolveSingletons(singletons []*SingletonSpec) *SingletonSpec {
	if len(singletons) == 0 {
		return nil
	}
	winner := singletons[0]
	for _, s := range singletons[1:] {
		if s.Priority > winner.Priority {
			winner = s
		} else if s.Priority == winner.Priority && s.Name < winner.Name {
			winner = s
		}
	}
	return winner
}

// parseSemver parses "v1.2.3" into components.
func parseSemver(v string) (semver, error) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("invalid semver %q: expected 3 components", v)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return semver{}, fmt.Errorf("invalid major %q: %w", parts[0], err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return semver{}, fmt.Errorf("invalid minor %q: %w", parts[1], err)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return semver{}, fmt.Errorf("invalid patch %q: %w", parts[2], err)
	}
	return semver{Major: major, Minor: minor, Patch: patch}, nil
}

// compareSemver returns -1, 0, or 1. Compares major first, then minor, then patch.
func compareSemver(a, b semver) int {
	if a.Major != b.Major {
		if a.Major < b.Major {
			return -1
		}
		return 1
	}
	if a.Minor != b.Minor {
		if a.Minor < b.Minor {
			return -1
		}
		return 1
	}
	if a.Patch != b.Patch {
		if a.Patch < b.Patch {
			return -1
		}
		return 1
	}
	return 0
}

type packageResolution struct {
	Winner               *PackageSpec
	Superseded           []*PackageSpec
	MajorVersionConflict bool
}

// resolvePackages groups packages by major version, detects major conflicts,
// and picks the highest minor.patch winner within each major group.
func resolvePackages(packages []*PackageSpec) (*packageResolution, error) {
	if len(packages) == 0 {
		return nil, fmt.Errorf("no packages")
	}

	type parsed struct {
		pkg *PackageSpec
		ver semver
	}
	var all []parsed
	for _, p := range packages {
		v, err := parseSemver(p.Version)
		if err != nil {
			return nil, fmt.Errorf("package %q: %w", p.Name, err)
		}
		all = append(all, parsed{pkg: p, ver: v})
	}

	// Group by major version.
	majors := map[int][]parsed{}
	for _, a := range all {
		majors[a.ver.Major] = append(majors[a.ver.Major], a)
	}

	// Major version conflict?
	if len(majors) > 1 {
		return &packageResolution{
			MajorVersionConflict: true,
		}, nil
	}

	// Single major group — pick highest minor.patch.
	sort.Slice(all, func(i, j int) bool {
		cmp := compareSemver(all[i].ver, all[j].ver)
		if cmp != 0 {
			return cmp > 0 // highest first
		}
		// Same version — oldest creation timestamp wins (stable).
		// In tests we use name as proxy for creation order.
		return all[i].pkg.Name < all[j].pkg.Name
	})

	winner := all[0]
	var superseded []*PackageSpec
	for _, a := range all[1:] {
		superseded = append(superseded, a.pkg)
	}

	return &packageResolution{
		Winner:     winner.pkg,
		Superseded: superseded,
	}, nil
}

// semverToPriority converts semver to a priority number that preserves ordering.
// priority = major * 1000000 + minor * 1000 + patch
func semverToPriority(v semver) int {
	return v.Major*1000000 + v.Minor*1000 + v.Patch
}

// compilePackageToSingleton converts a Package to a Singleton.
// Semver is encoded as priority so the Singleton machinery resolves it.
func compilePackageToSingleton(pkg *PackageSpec) (*SingletonSpec, error) {
	v, err := parseSemver(pkg.Version)
	if err != nil {
		return nil, err
	}
	return &SingletonSpec{
		Name:     pkg.Name + "-" + strings.ReplaceAll(strings.TrimPrefix(pkg.Version, "v"), ".", "-"),
		Priority: semverToPriority(v),
		Nodes:    pkg.Nodes,
	}, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════════════════

func stdlibDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "examples", "stdlib")
}

func parseGraphDocs(t *testing.T, filename string) []*GraphSpec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stdlibDir(), filename))
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

func parseDocsByKind(t *testing.T, filename, targetKind string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stdlibDir(), filename))
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

func splitYAMLDocs(data []byte) [][]byte {
	var docs [][]byte
	for _, part := range bytes.Split(data, []byte("\n---")) {
		docs = append(docs, part)
	}
	return docs
}

// ═══════════════════════════════════════════════════════════════════════════════
// Tests
// ═══════════════════════════════════════════════════════════════════════════════

// ---------------------------------------------------------------------------
// GraphDefinition → Graph (file 5)
// ---------------------------------------------------------------------------

func TestStdlibGraphDefinitionImplementation(t *testing.T) {
	specs := parseGraphDocs(t, "5-graph-definition-implementation.yaml")
	require.Len(t, specs, 1)

	compiled, err := compileGraphSpec(specs[0])
	require.NoError(t, err)
	dag := compiled.dag

	assert.Equal(t, ReferenceWatchKind, dag.References["items"])
	assert.Contains(t, dag.Dependents["items"], dag.Index["perItem"])

	t.Logf("GraphDefinition L0: %d nodes, %d levels", len(dag.Nodes), len(dag.Levels))
	for _, node := range dag.Nodes {
		t.Logf("  %s: %s", node.ID, dag.References[node.ID])
	}
}

// ---------------------------------------------------------------------------
// Decorator → Graph (parse file 2, compile, verify through compileGraphSpec)
// ---------------------------------------------------------------------------

func TestStdlibCompileDecorator(t *testing.T) {
	docs := parseDocsByKind(t, "2-decorator.yaml", "Decorator")
	require.Len(t, docs, 3, "expected 3 Decorator documents")

	t.Run("namespace-policies (WatchKind, flat)", func(t *testing.T) {
		dec, err := extractDecoratorSpec(docs[0])
		require.NoError(t, err)

		graph, err := compileDecorator(dec)
		require.NoError(t, err)

		// The compiled Graph should have: items (WatchKind) + policy (forEach)
		require.Len(t, graph.Nodes, 2)
		assert.Equal(t, "items", graph.Nodes[0].ID)
		assert.Equal(t, "policy", graph.Nodes[1].ID)
		assert.Equal(t, map[string]string{"item": "${items}"}, graph.Nodes[1].ForEach)

		// Feed through the real compiler.
		compiled, err := compileGraphSpec(graph)
		require.NoError(t, err)
		dag := compiled.dag

		assert.Equal(t, ReferenceWatchKind, dag.References["items"])
		policyDeps := dag.Nodes[dag.Index["policy"]].Dependencies
		assert.True(t, policyDeps["items"], "policy should depend on items")

		t.Logf("Decorator→Graph: %d nodes, refs: items=%s policy=%s",
			len(dag.Nodes), dag.References["items"], dag.References["policy"])
	})

	t.Run("namespace-hardening (WatchKind, dependent nodes)", func(t *testing.T) {
		dec, err := extractDecoratorSpec(docs[1])
		require.NoError(t, err)
		require.Len(t, dec.Nodes, 2, "quota + policy")

		graph, err := compileDecorator(dec)
		require.NoError(t, err)

		// items + quota (forEach) + policy (forEach)
		require.Len(t, graph.Nodes, 3)
		assert.Equal(t, "items", graph.Nodes[0].ID)

		// Both side-effect nodes get forEach.
		for _, n := range graph.Nodes[1:] {
			assert.Equal(t, map[string]string{"item": "${items}"}, n.ForEach,
				"node %s should have forEach", n.ID)
		}

		compiled, err := compileGraphSpec(graph)
		require.NoError(t, err)
		dag := compiled.dag

		assert.Equal(t, ReferenceWatchKind, dag.References["items"])
		// quota and policy should both depend on items.
		quotaDeps := dag.Nodes[dag.Index["quota"]].Dependencies
		assert.True(t, quotaDeps["items"], "quota should depend on items")
		policyDeps := dag.Nodes[dag.Index["policy"]].Dependencies
		assert.True(t, policyDeps["items"], "policy should depend on items")
		// policy also depends on quota (via ${quota.metadata.name}).
		assert.True(t, policyDeps["quota"], "policy should depend on quota")

		t.Logf("Decorator→Graph (dependent): %d nodes, %d levels",
			len(dag.Nodes), len(dag.Levels))
	})

	t.Run("config-derived-secret (Watch, single resource)", func(t *testing.T) {
		dec, err := extractDecoratorSpec(docs[2])
		require.NoError(t, err)

		graph, err := compileDecorator(dec)
		require.NoError(t, err)

		// item (Watch) + derived (no forEach — single resource).
		require.Len(t, graph.Nodes, 2)
		assert.Equal(t, "item", graph.Nodes[0].ID)
		assert.Equal(t, "derived", graph.Nodes[1].ID)
		assert.Nil(t, graph.Nodes[1].ForEach, "Watch Decorator should not add forEach")

		compiled, err := compileGraphSpec(graph)
		require.NoError(t, err)
		dag := compiled.dag

		assert.Equal(t, ReferenceWatch, dag.References["item"])
		derivedDeps := dag.Nodes[dag.Index["derived"]].Dependencies
		assert.True(t, derivedDeps["item"], "derived should depend on item")

		t.Logf("Decorator→Graph (Watch): %d nodes", len(dag.Nodes))
	})
}

// ---------------------------------------------------------------------------
// Singleton resolution (parse file 3, resolve priorities)
// ---------------------------------------------------------------------------

func TestStdlibResolveSingletons(t *testing.T) {
	docs := parseDocsByKind(t, "3-singleton.yaml", "Singleton")
	require.GreaterOrEqual(t, len(docs), 3, "expected at least 3 Singleton documents")

	t.Run("priority resolution — team-b wins", func(t *testing.T) {
		// team-a (priority 100) and team-b (priority 200) target the same ConfigMap.
		var singletons []*SingletonSpec
		for _, doc := range docs {
			s, err := extractSingletonSpec(doc)
			require.NoError(t, err)
			// Filter to just the dashboard Singletons.
			if strings.HasSuffix(s.Name, "-dashboard") {
				singletons = append(singletons, s)
			}
		}
		require.Len(t, singletons, 2, "expected 2 dashboard Singletons")

		winner := resolveSingletons(singletons)
		require.NotNil(t, winner)
		assert.Equal(t, 200, winner.Priority, "team-b (priority 200) should win")
		assert.Equal(t, "team-b-dashboard", winner.Name)

		t.Logf("Winner: %s (priority %d)", winner.Name, winner.Priority)
	})

	t.Run("tie-breaking by name", func(t *testing.T) {
		s1 := &SingletonSpec{Name: "beta", Priority: 100}
		s2 := &SingletonSpec{Name: "alpha", Priority: 100}
		winner := resolveSingletons([]*SingletonSpec{s1, s2})
		assert.Equal(t, "alpha", winner.Name, "alphabetically first name wins ties")
	})

	t.Run("single singleton always wins", func(t *testing.T) {
		s, err := extractSingletonSpec(docs[2]) // cluster-admin-binding
		require.NoError(t, err)
		winner := resolveSingletons([]*SingletonSpec{s})
		assert.Equal(t, s.Name, winner.Name)
	})

	t.Run("winner nodes compile as Graph", func(t *testing.T) {
		// The winning Singleton's nodes should compile as a valid Graph.
		s, err := extractSingletonSpec(docs[2]) // cluster-admin-binding
		require.NoError(t, err)
		graph := &GraphSpec{Nodes: s.Nodes}
		_, err = compileGraphSpec(graph)
		require.NoError(t, err, "winner's nodes should compile as a Graph")
	})
}

// ---------------------------------------------------------------------------
// Semver comparison
// ---------------------------------------------------------------------------

func TestStdlibSemverComparison(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.2.0", "v0.2.1", -1},  // patch: 0 < 1
		{"v0.2.1", "v0.2.0", 1},   // patch: 1 > 0
		{"v0.2.1", "v0.3.0", -1},  // minor: 2 < 3
		{"v0.3.0", "v0.2.1", 1},   // minor: 3 > 2
		{"v0.2.1", "v1.0.0", -1},  // major: 0 < 1
		{"v1.0.0", "v0.2.1", 1},   // major: 1 > 0
		{"v1.0.0", "v1.0.0", 0},   // equal
		{"v2.0.0", "v1.99.99", 1}, // major wins over minor+patch
		{"v0.0.1", "v0.0.2", -1},  // patch only
		{"v10.0.0", "v9.0.0", 1},  // double-digit major
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_vs_%s", tc.a, tc.b), func(t *testing.T) {
			a, err := parseSemver(tc.a)
			require.NoError(t, err)
			b, err := parseSemver(tc.b)
			require.NoError(t, err)
			assert.Equal(t, tc.want, compareSemver(a, b))
		})
	}
}

func TestStdlibParseSemver(t *testing.T) {
	v, err := parseSemver("v1.2.3")
	require.NoError(t, err)
	assert.Equal(t, semver{1, 2, 3}, v)

	v, err = parseSemver("v0.2.0")
	require.NoError(t, err)
	assert.Equal(t, semver{0, 2, 0}, v)

	_, err = parseSemver("1.2")
	assert.Error(t, err, "should reject missing patch")

	_, err = parseSemver("vx.y.z")
	assert.Error(t, err, "should reject non-numeric")
}

func TestStdlibSemverToPriority(t *testing.T) {
	// Verify the priority encoding preserves semver ordering.
	cases := []struct {
		version  string
		priority int
	}{
		{"v0.0.0", 0},
		{"v0.0.1", 1},
		{"v0.2.0", 2000},
		{"v0.2.1", 2001},
		{"v1.0.0", 1000000},
		{"v1.2.3", 1002003},
		{"v2.0.0", 2000000},
	}
	for _, tc := range cases {
		v, err := parseSemver(tc.version)
		require.NoError(t, err)
		assert.Equal(t, tc.priority, semverToPriority(v), "%s → %d", tc.version, tc.priority)
	}
	// Verify ordering: for any a < b (semver), priority(a) < priority(b).
	ordered := []string{"v0.0.0", "v0.0.1", "v0.1.0", "v0.2.0", "v0.2.1", "v0.3.0", "v1.0.0", "v2.0.0"}
	for i := 1; i < len(ordered); i++ {
		a, _ := parseSemver(ordered[i-1])
		b, _ := parseSemver(ordered[i])
		assert.Less(t, semverToPriority(a), semverToPriority(b),
			"%s (%d) should be less than %s (%d)",
			ordered[i-1], semverToPriority(a), ordered[i], semverToPriority(b))
	}
}

// ---------------------------------------------------------------------------
// Package → Singleton (parse file 4, resolve, compile)
// ---------------------------------------------------------------------------

func TestStdlibCompilePackage(t *testing.T) {
	docs := parseDocsByKind(t, "4-package.yaml", "Package")
	require.GreaterOrEqual(t, len(docs), 2, "expected at least 2 Package documents")

	t.Run("semver resolution — v0.2.1 wins over v0.2.0", func(t *testing.T) {
		var packages []*PackageSpec
		for _, doc := range docs {
			p, err := extractPackageSpec(doc)
			require.NoError(t, err)
			packages = append(packages, p)
		}

		result, err := resolvePackages(packages)
		require.NoError(t, err)
		require.False(t, result.MajorVersionConflict, "same major should not conflict")
		require.NotNil(t, result.Winner)

		assert.Equal(t, "v0.2.1", result.Winner.Version, "v0.2.1 should win")
		assert.Len(t, result.Superseded, len(packages)-1, "losers should be superseded")

		t.Logf("Winner: %s %s, superseded: %d packages",
			result.Winner.Name, result.Winner.Version, len(result.Superseded))
	})

	t.Run("winner compiles to Singleton with nodes", func(t *testing.T) {
		var packages []*PackageSpec
		for _, doc := range docs {
			p, err := extractPackageSpec(doc)
			require.NoError(t, err)
			packages = append(packages, p)
		}

		result, err := resolvePackages(packages)
		require.NoError(t, err)

		singleton, err := compilePackageToSingleton(result.Winner)
		require.NoError(t, err)
		assert.Equal(t, "kro-0-2-1", singleton.Name)
		assert.Equal(t, 2001, singleton.Priority, "v0.2.1 → priority 2001")
		assert.NotEmpty(t, singleton.Nodes, "Singleton should have the winner's nodes")

		t.Logf("Singleton: %s, %d nodes", singleton.Name, len(singleton.Nodes))
	})

	t.Run("major version conflict", func(t *testing.T) {
		packages := []*PackageSpec{
			{Name: "kro", Version: "v0.2.1"},
			{Name: "kro", Version: "v1.0.0"},
		}

		result, err := resolvePackages(packages)
		require.NoError(t, err)
		assert.True(t, result.MajorVersionConflict,
			"different major versions should produce MajorVersionConflict")
		assert.Nil(t, result.Winner, "no winner on major conflict")
	})
}

// ---------------------------------------------------------------------------
// Full chain: Package → Singleton → Graph
// ---------------------------------------------------------------------------

func TestStdlibFullChain(t *testing.T) {
	// Parse Package source (file 4).
	docs := parseDocsByKind(t, "4-package.yaml", "Package")
	require.NotEmpty(t, docs)

	var packages []*PackageSpec
	for _, doc := range docs {
		p, err := extractPackageSpec(doc)
		require.NoError(t, err)
		packages = append(packages, p)
	}

	// Step 1: Check for major version conflicts.
	result, err := resolvePackages(packages)
	require.NoError(t, err)
	require.False(t, result.MajorVersionConflict)
	require.NotNil(t, result.Winner)
	t.Logf("Package resolution: %s %s wins over %d others",
		result.Winner.Name, result.Winner.Version, len(result.Superseded))

	// Step 2: Package → Singleton (every Package becomes a Singleton with semver-derived priority).
	var singletons []*SingletonSpec
	for _, pkg := range packages {
		s, err := compilePackageToSingleton(pkg)
		require.NoError(t, err)
		singletons = append(singletons, s)
		t.Logf("  Package %s %s → Singleton %s (priority %d)",
			pkg.Name, pkg.Version, s.Name, s.Priority)
	}

	// Step 3: Singleton resolution — highest priority wins.
	winner := resolveSingletons(singletons)
	require.NotNil(t, winner)
	assert.Equal(t, 2001, winner.Priority, "v0.2.1 (priority 2001) should win over v0.2.0 (2000)")
	t.Logf("Singleton resolution: %s (priority %d) wins", winner.Name, winner.Priority)

	// Step 4: Winner's nodes compile as a valid Graph.
	graph := &GraphSpec{Nodes: winner.Nodes}
	compiled, err := compileGraphSpec(graph)
	require.NoError(t, err)
	dag := compiled.dag

	t.Logf("Singleton → Graph: %d nodes, %d levels", len(dag.Nodes), len(dag.Levels))
	for _, node := range dag.Nodes {
		t.Logf("  %s: %s", node.ID, dag.References[node.ID])
	}

	// Verify the kro installation resources are present.
	_, hasNamespace := dag.Index["namespace"]
	_, hasController := dag.Index["controller"]
	assert.True(t, hasNamespace, "should have namespace node")
	assert.True(t, hasController, "should have controller node")
}

// ---------------------------------------------------------------------------
// Escape levels (unchanged — validates GraphDefinition → Graph)
// ---------------------------------------------------------------------------

func TestStdlibEscapeLevels(t *testing.T) {
	specs := parseGraphDocs(t, "5-graph-definition-implementation.yaml")
	require.Len(t, specs, 1)

	compiled, err := compileGraphSpec(specs[0])
	require.NoError(t, err)

	for expr := range compiled.programs {
		assert.NotContains(t, expr, "schema.metadata.name", "L1 expression leaked into L0")
		assert.NotContains(t, expr, "schema.spec.replicas", "L1 expression leaked into L0")
	}

	foundItems, foundItemMeta := false, false
	for expr := range compiled.programs {
		if expr == "items" {
			foundItems = true
		}
		if expr == "item.metadata.name" || expr == "item.metadata.namespace" {
			foundItemMeta = true
		}
	}
	assert.True(t, foundItems, "'items' should be compiled at L0")
	assert.True(t, foundItemMeta, "'item.metadata.*' should be compiled at L0")
}

// ---------------------------------------------------------------------------
// CEL patterns
// ---------------------------------------------------------------------------

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
	compiled, err := compileGraphSpec(spec)
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
	compiled, err := compileGraphSpec(spec)
	require.NoError(t, err)
	found := false
	for expr := range compiled.programs {
		if expr == "items.map(i, i.metadata.namespace).distinct()" {
			found = true
		}
	}
	assert.True(t, found, "distinct() expression should be compiled")
}

// ---------------------------------------------------------------------------
// Catch-all: every Graph in every implementation file compiles
// ---------------------------------------------------------------------------

func TestStdlibAllImplementationFilesCompile(t *testing.T) {
	dir := stdlibDir()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".yaml" || !strings.Contains(name, "implementation") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			specs := parseGraphDocs(t, name)
			if len(specs) == 0 {
				t.Skipf("no kind: Graph documents in %s", name)
			}
			for i, spec := range specs {
				compiled, err := compileGraphSpec(spec)
				require.NoError(t, err, "doc %d should compile", i)
				dag := compiled.dag
				assert.NotEmpty(t, dag.TopologicalOrder, "doc %d should have nodes", i)
				t.Logf("  doc %d: %d nodes, %d levels", i, len(dag.Nodes), len(dag.Levels))
			}
		})
	}
}
