package graphcontroller

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// forEach Oracle Tests — random graph generation
//
// These tests exercise the REAL forEach incremental evaluation optimization
// against a full-re-evaluation oracle. The goal: find cases where
// SelfContained carry-forward produces wrong results.
//
// Bug classes targeted:
//   1. SelfContained incorrectly true — template references external scope
//      but is marked SelfContained; carry-forward returns stale results.
//   2. forEach scale-down — items removed between reconciles; stale hashes
//      from removed items persist and pollute results.
//   3. External scope mutation — items unchanged but external scope changes;
//      optimization carries forward but oracle re-evaluates.
// ---------------------------------------------------------------------------

// forEachTestCase holds a single random forEach scenario.
type forEachTestCase struct {
	// Collection items for cycle 1 and cycle 2.
	items1 []map[string]any
	items2 []map[string]any

	// External scope value referenced by the template (if not self-contained).
	externalVal1 string
	externalVal2 string

	// Whether the template references external scope.
	referencesExternal bool

	// Whether SelfContained is set (may be incorrectly set).
	selfContained bool
}

// generateForEachCase creates a random forEach test case.
func generateForEachCase(rng *rand.Rand, forceExternalRef bool, forceSelfContained bool) forEachTestCase {
	// Generate 1-20 items for cycle 1.
	n1 := 1 + rng.Intn(20)
	items1 := make([]map[string]any, n1)
	for i := range items1 {
		items1[i] = map[string]any{
			"metadata": map[string]any{"name": fmt.Sprintf("item-%d", i)},
			"value":    fmt.Sprintf("v%d", rng.Intn(100)),
		}
	}

	// Cycle 2: mutate some items, possibly add/remove.
	// Start by copying.
	n2 := n1
	// 30% chance of adding items.
	if rng.Float64() < 0.3 {
		add := 1 + rng.Intn(5)
		n2 = n1 + add
	}
	// 20% chance of removing items (but keep at least 1).
	if rng.Float64() < 0.2 && n1 > 1 {
		remove := 1 + rng.Intn(min(5, n1-1))
		n2 = n1 - remove
	}

	items2 := make([]map[string]any, n2)
	for i := range items2 {
		if i < n1 {
			// Copy existing item.
			items2[i] = map[string]any{
				"metadata": map[string]any{"name": fmt.Sprintf("item-%d", i)},
				"value":    items1[i]["value"],
			}
			// 40% chance of mutating value.
			if rng.Float64() < 0.4 {
				items2[i]["value"] = fmt.Sprintf("v%d", rng.Intn(100))
			}
		} else {
			// New item.
			items2[i] = map[string]any{
				"metadata": map[string]any{"name": fmt.Sprintf("item-%d", i)},
				"value":    fmt.Sprintf("v%d", rng.Intn(100)),
			}
		}
	}

	externalVal1 := fmt.Sprintf("ext-%d", rng.Intn(50))
	externalVal2 := externalVal1
	// 60% chance external scope changes.
	if rng.Float64() < 0.6 {
		externalVal2 = fmt.Sprintf("ext-%d", rng.Intn(50))
	}

	referencesExternal := forceExternalRef || rng.Float64() < 0.4
	selfContained := forceSelfContained
	if !forceSelfContained {
		if referencesExternal {
			// 30% chance of incorrectly marking as self-contained.
			selfContained = rng.Float64() < 0.3
		} else {
			// Correctly self-contained.
			selfContained = true
		}
	}

	return forEachTestCase{
		items1:             items1,
		items2:             items2,
		externalVal1:       externalVal1,
		externalVal2:       externalVal2,
		referencesExternal: referencesExternal,
		selfContained:      selfContained,
	}
}

// buildForEachNodes builds the graph spec nodes for a forEach test.
func buildForEachNodes(tc forEachTestCase, items []map[string]any, selfContained bool) []graphpkg.Node {
	var defMap map[string]any
	if tc.referencesExternal {
		defMap = map[string]any{
			"name":   "${item.metadata.name}",
			"value":  "${item.value}",
			"extern": "${config.label}",
		}
	} else {
		defMap = map[string]any{
			"name":  "${item.metadata.name}",
			"value": "${item.value}",
		}
	}

	nodes := []graphpkg.Node{}

	if tc.referencesExternal {
		configNode := graphpkg.Node{ID: "config", Def: map[string]any{
			"label": "placeholder",
		}}
		configNode.SetType(graphpkg.NodeTypeDef)
		nodes = append(nodes, configNode)
	}

	sourceNode := graphpkg.Node{ID: "source", Def: map[string]any{
		"items": toAnySlice(items),
	}}
	sourceNode.SetType(graphpkg.NodeTypeDef)
	nodes = append(nodes, sourceNode)

	workersNode := graphpkg.Node{
		ID: "workers",
		ForEach: &graphpkg.ForEachBinding{
			VarName:       "item",
			Expr:          "${source.items}",
			SelfContained: selfContained,
		},
		Def: defMap,
	}
	workersNode.SetType(graphpkg.NodeTypeDef)
	nodes = append(nodes, workersNode)

	return nodes
}

// runForEachOracle evaluates the forEach with SelfContained=false (full re-evaluation).
// Returns the scope output ([]any) as a map keyed by item name.
func runForEachOracle(t *testing.T, tc forEachTestCase, items []map[string]any, externalVal string) map[string]map[string]any {
	t.Helper()

	nodes := buildForEachNodes(tc, items, false)
	spec := &graphpkg.GraphSpec{Nodes: nodes}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatalf("oracle compile: %v", err)
	}

	state := newInstanceState(compiled, nil)
	eval := newEvaluator(state)
	eval.effectiveGeneration = 1
	eval.scope["source"] = map[string]any{"items": toAnySlice(items)}
	if tc.referencesExternal {
		eval.scope["config"] = map[string]any{"label": externalVal}
	}

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata":   map[string]any{"name": "test", "namespace": "default"},
	}}
	r := &GraphReconciler{}
	rs := newReconcileScope(graph, nil)

	// Find the workers node index.
	workersIdx := len(nodes) - 1
	_, err = r.cluster().reconcileForEach(context.Background(), rs, spec.Nodes[workersIdx], eval, state)
	if err != nil {
		t.Fatalf("oracle reconcile: %v", err)
	}

	result, ok := eval.scope["workers"].([]any)
	if !ok {
		t.Fatalf("oracle scope not []any")
	}

	return indexByName(result)
}

// runForEachOptimized runs two cycles with the optimization enabled.
// Returns the scope output from cycle 2.
func runForEachOptimized(t *testing.T, tc forEachTestCase) map[string]map[string]any {
	t.Helper()

	nodes := buildForEachNodes(tc, tc.items1, tc.selfContained)
	spec := &graphpkg.GraphSpec{Nodes: nodes}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatalf("optimized compile: %v", err)
	}

	state := newInstanceState(compiled, nil)
	workersIdx := len(nodes) - 1

	// --- Cycle 1: populate hashes ---
	eval1 := newEvaluator(state)
	eval1.effectiveGeneration = 1
	eval1.scope["source"] = map[string]any{"items": toAnySlice(tc.items1)}
	if tc.referencesExternal {
		eval1.scope["config"] = map[string]any{"label": tc.externalVal1}
	}

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata":   map[string]any{"name": "test", "namespace": "default"},
	}}
	r := &GraphReconciler{}
	rs := newReconcileScope(graph, nil)

	_, err = r.cluster().reconcileForEach(context.Background(), rs, spec.Nodes[workersIdx], eval1, state)
	if err != nil {
		t.Fatalf("optimized cycle 1: %v", err)
	}

	// --- Cycle 2: with mutations ---
	eval2 := newEvaluator(state)
	eval2.effectiveGeneration = 1
	eval2.scope["source"] = map[string]any{"items": toAnySlice(tc.items2)}
	if tc.referencesExternal {
		eval2.scope["config"] = map[string]any{"label": tc.externalVal2}
	}

	_, err = r.cluster().reconcileForEach(context.Background(), rs, spec.Nodes[workersIdx], eval2, state)
	if err != nil {
		t.Fatalf("optimized cycle 2: %v", err)
	}

	result, ok := eval2.scope["workers"].([]any)
	if !ok {
		t.Fatalf("optimized scope not []any")
	}

	return indexByName(result)
}

// indexByName converts []any (of map[string]any with "name" field) to a map keyed by name.
func indexByName(items []any) map[string]map[string]any {
	m := make(map[string]map[string]any, len(items))
	for _, item := range items {
		im := item.(map[string]any)
		name := im["name"].(string)
		m[name] = im
	}
	return m
}

// toAnySlice converts []map[string]any to []any.
func toAnySlice(items []map[string]any) []any {
	result := make([]any, len(items))
	for i, item := range items {
		result[i] = item
	}
	return result
}

// compareResults checks whether optimized and oracle results diverge.
// Returns true if they diverge, plus a description.
func compareResults(oracle, optimized map[string]map[string]any) (bool, string) {
	if len(oracle) != len(optimized) {
		return true, fmt.Sprintf("length mismatch: oracle=%d, optimized=%d", len(oracle), len(optimized))
	}
	for name, oItem := range oracle {
		optItem, exists := optimized[name]
		if !exists {
			return true, fmt.Sprintf("item %q in oracle but not in optimized", name)
		}
		// Compare value field.
		if oItem["value"] != optItem["value"] {
			return true, fmt.Sprintf("item %q value: oracle=%v, optimized=%v", name, oItem["value"], optItem["value"])
		}
		// Compare extern field if present.
		oExt, oHas := oItem["extern"]
		optExt, optHas := optItem["extern"]
		if oHas != optHas {
			return true, fmt.Sprintf("item %q extern presence: oracle=%v, optimized=%v", name, oHas, optHas)
		}
		if oHas && oExt != optExt {
			return true, fmt.Sprintf("item %q extern: oracle=%v, optimized=%v", name, oExt, optExt)
		}
	}
	return false, ""
}

// TestOracleForEach_RandomGraphs generates random forEach scenarios and compares
// the incremental optimization against a full-re-evaluation oracle.
func TestOracleForEach_RandomGraphs(t *testing.T) {
	const seed = 42
	const numTrials = 300
	rng := rand.New(rand.NewSource(seed))

	var totalDiverged int
	var externalRefDiverged int
	var scaleDownDiverged int
	var correctSelfContainedDiverged int

	type divergence struct {
		trial       int
		description string
		tc          forEachTestCase
	}
	var divergences []divergence

	for trial := 0; trial < numTrials; trial++ {
		tc := generateForEachCase(rng, false, false)

		// Run oracle: full re-evaluation on cycle 2 input.
		oracle := runForEachOracle(t, tc, tc.items2, tc.externalVal2)

		// Run optimized: cycle 1 then cycle 2 with carry-forward.
		optimized := runForEachOptimized(t, tc)

		diverged, desc := compareResults(oracle, optimized)
		if diverged {
			totalDiverged++
			if tc.referencesExternal && tc.selfContained {
				externalRefDiverged++
			}
			if len(tc.items2) < len(tc.items1) {
				scaleDownDiverged++
			}
			if !tc.referencesExternal && tc.selfContained {
				correctSelfContainedDiverged++
			}
			if len(divergences) < 10 {
				divergences = append(divergences, divergence{
					trial:       trial,
					description: desc,
					tc:          tc,
				})
			}
		}
	}

	t.Logf("=== forEach Oracle Results (seed=%d, trials=%d) ===", seed, numTrials)
	t.Logf("Total divergences: %d/%d", totalDiverged, numTrials)
	t.Logf("  - SelfContained=true but references external scope: %d", externalRefDiverged)
	t.Logf("  - Scale-down (items removed): %d", scaleDownDiverged)
	t.Logf("  - Correctly self-contained (no external ref): %d", correctSelfContainedDiverged)
	t.Logf("")

	if totalDiverged > 0 {
		t.Logf("Bug class discovery:")
		if externalRefDiverged > 0 {
			t.Logf("  BUG: SelfContained incorrectly applied — template references external scope")
			t.Logf("       but optimization carries forward stale results when external scope changes.")
			t.Logf("       This is the 'stale carry-forward on external mutation' bug.")
		}
		if scaleDownDiverged > 0 {
			t.Logf("  BUG: Scale-down divergence — items removed from collection between reconciles")
			t.Logf("       may leave stale state or produce wrong results.")
		}
		if correctSelfContainedDiverged > 0 {
			t.Logf("  UNEXPECTED: Divergence with correctly self-contained bindings — ")
			t.Logf("       possible hash collision or carry-forward logic error.")
		}
		t.Logf("")
		t.Logf("First divergences:")
		for _, d := range divergences {
			t.Logf("  trial %d: %s (external=%v, selfContained=%v, items1=%d, items2=%d)",
				d.trial, d.description, d.tc.referencesExternal, d.tc.selfContained,
				len(d.tc.items1), len(d.tc.items2))
		}
	} else {
		t.Logf("No divergences found. The optimization is correct for all generated cases.")
		t.Logf("(This does NOT mean the optimization is bug-free — just that the random")
		t.Logf("generator did not find a failing case in %d trials.)", numTrials)
	}
}

// TestOracleForEach_SelfContainedViolation specifically tests the case where
// SelfContained is incorrectly set to true but the template references external
// scope. Mutates ONLY the external scope (not collection items) to expose
// stale carry-forward.
//
// This is the "can we find the SelfContained violation bug without being told
// about it" test.
func TestOracleForEach_SelfContainedViolation(t *testing.T) {
	const seed = 42
	const numTrials = 200
	rng := rand.New(rand.NewSource(seed))

	var totalDiverged int
	var divergenceDetails []string

	for trial := 0; trial < numTrials; trial++ {
		// Generate items that DON'T change between cycles.
		numItems := 1 + rng.Intn(15)
		items := make([]map[string]any, numItems)
		for i := range items {
			items[i] = map[string]any{
				"metadata": map[string]any{"name": fmt.Sprintf("item-%d", i)},
				"value":    fmt.Sprintf("v%d", rng.Intn(100)),
			}
		}

		externalVal1 := fmt.Sprintf("ext-%d", rng.Intn(50))
		// Always change external value.
		externalVal2 := fmt.Sprintf("ext-%d", 50+rng.Intn(50))

		tc := forEachTestCase{
			items1:             items,
			items2:             items, // SAME items — only external scope changes.
			externalVal1:       externalVal1,
			externalVal2:       externalVal2,
			referencesExternal: true,
			selfContained:      true, // INCORRECTLY set — template references config.label.
		}

		// Oracle: full re-evaluation with new external scope.
		oracle := runForEachOracle(t, tc, tc.items2, tc.externalVal2)

		// Optimized: carry-forward (items unchanged, so all items will be skipped).
		optimized := runForEachOptimized(t, tc)

		diverged, desc := compareResults(oracle, optimized)
		if diverged {
			totalDiverged++
			if len(divergenceDetails) < 5 {
				divergenceDetails = append(divergenceDetails, fmt.Sprintf(
					"trial %d: %s (extVal1=%s, extVal2=%s, items=%d)",
					trial, desc, externalVal1, externalVal2, numItems))
			}
		}
	}

	t.Logf("=== SelfContained Violation Test (seed=%d, trials=%d) ===", seed, numTrials)
	t.Logf("Divergences: %d/%d", totalDiverged, numTrials)
	t.Logf("")

	if totalDiverged > 0 {
		t.Logf("CONFIRMED BUG: SelfContained=true with external scope reference")
		t.Logf("  When collection items are unchanged but external scope mutates,")
		t.Logf("  the optimization incorrectly carries forward stale results.")
		t.Logf("  The oracle re-evaluates and produces the correct (new) external value.")
		t.Logf("")
		t.Logf("  Divergence rate: %d/%d (%.1f%%)", totalDiverged, numTrials,
			float64(totalDiverged)*100/float64(numTrials))
		t.Logf("")
		t.Logf("  Examples:")
		for _, d := range divergenceDetails {
			t.Logf("    %s", d)
		}
		t.Logf("")
		t.Logf("  Root cause: reconcileForEach checks only item hashes to decide")
		t.Logf("  whether to skip. If SelfContained is incorrectly true, external")
		t.Logf("  scope changes are invisible to the hash check, producing stale output.")
		t.Logf("")
		t.Logf("  Fix: compiler must never set SelfContained=true when the template")
		t.Logf("  references variables outside the iterator. If it does, the optimization")
		t.Logf("  must also hash the full external scope (or at least referenced variables).")
	} else {
		t.Logf("No divergences found. This is unexpected — the bug should manifest")
		t.Logf("when external scope changes but items don't. Investigate test setup.")
	}
}

// TestOracleForEach_ScaleDown tests specifically the case where items are removed
// between reconciles. Verifies that the optimization doesn't leak stale results
// from removed items.
func TestOracleForEach_ScaleDown(t *testing.T) {
	const seed = 42
	const numTrials = 200
	rng := rand.New(rand.NewSource(seed))

	var totalDiverged int

	for trial := 0; trial < numTrials; trial++ {
		// Generate 5-20 items for cycle 1.
		n1 := 5 + rng.Intn(16)
		items1 := make([]map[string]any, n1)
		for i := range items1 {
			items1[i] = map[string]any{
				"metadata": map[string]any{"name": fmt.Sprintf("item-%d", i)},
				"value":    fmt.Sprintf("v%d", rng.Intn(100)),
			}
		}

		// Remove 1-N items for cycle 2 (keep at least 1).
		removeCount := 1 + rng.Intn(min(n1-1, 10))
		n2 := n1 - removeCount
		items2 := make([]map[string]any, n2)
		copy(items2, items1[:n2])

		tc := forEachTestCase{
			items1:             items1,
			items2:             items2,
			externalVal1:       "x",
			externalVal2:       "x",
			referencesExternal: false,
			selfContained:      true,
		}

		oracle := runForEachOracle(t, tc, tc.items2, tc.externalVal2)
		optimized := runForEachOptimized(t, tc)

		diverged, _ := compareResults(oracle, optimized)
		if diverged {
			totalDiverged++
		}
	}

	t.Logf("=== Scale-Down Test (seed=%d, trials=%d) ===", seed, numTrials)
	t.Logf("Divergences: %d/%d", totalDiverged, numTrials)
	if totalDiverged > 0 {
		t.Logf("  BUG: Scale-down produces divergent results.")
		t.Logf("  Stale hashes/scope from removed items may leak into output.")
	} else {
		t.Logf("  Scale-down is handled correctly — removed items don't leak.")
	}
}
