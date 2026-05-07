package graphcontroller

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Random DAG oracle exploration (Bornholt/ShardStore method)
//
// Generate random DAGs, random evaluation policy perturbations, and compare
// against a sequential oracle. Divergence = discovered bug class.
//
// This is exploratory — tests always pass. They report what they find.
// ---------------------------------------------------------------------------

// rNode is a simplified DAG node for random exploration.
type rNode struct {
	id   string
	deps []string // IDs of nodes this node depends on
}

// rDAG is a random DAG for oracle exploration.
type rDAG struct {
	nodes []rNode
	topo  []int // topological order (indices into nodes)
}

// generateRandomDAG creates a random DAG with n nodes (3-15).
// Edges only go from lower-index to higher-index nodes (guarantees acyclic).
func generateRandomDAG(rng *rand.Rand, n int) *rDAG {
	nodes := make([]rNode, n)
	for i := range nodes {
		nodes[i].id = fmt.Sprintf("n%d", i)
	}

	// Add random edges from lower to higher index (ensures DAG property).
	for i := 1; i < n; i++ {
		// Each node gets 0-3 deps from earlier nodes.
		numDeps := rng.Intn(min(4, i+1))
		if numDeps == 0 && rng.Float64() < 0.7 {
			// Most non-root nodes should have at least one dep.
			numDeps = 1
		}
		chosen := map[int]bool{}
		for d := 0; d < numDeps; d++ {
			dep := rng.Intn(i)
			if !chosen[dep] {
				chosen[dep] = true
				nodes[i].deps = append(nodes[i].deps, nodes[dep].id)
			}
		}
	}

	// Topological order: since edges only go lower->higher, declaration
	// order IS a valid topological order.
	topo := make([]int, n)
	for i := range topo {
		topo[i] = i
	}

	return &rDAG{nodes: nodes, topo: topo}
}

// evalOracle evaluates the DAG in correct topological order with full scope.
// Each node produces: concat of all dep values + "-" + nodeID.
// Root nodes (no deps) produce their ID as the initial value.
func evalOracle(dag *rDAG) map[string]string {
	scope := map[string]string{}
	for _, idx := range dag.topo {
		node := dag.nodes[idx]
		scope[node.id] = evalNode(node, scope)
	}
	return scope
}

// evalNode computes a node's value given the current scope.
func evalNode(node rNode, scope map[string]string) string {
	if len(node.deps) == 0 {
		return node.id
	}
	// Sort deps for determinism.
	sorted := make([]string, len(node.deps))
	copy(sorted, node.deps)
	sort.Strings(sorted)

	parts := make([]string, len(sorted))
	for i, dep := range sorted {
		val, ok := scope[dep]
		if !ok {
			val = "<missing:" + dep + ">"
		}
		parts[i] = val
	}
	return strings.Join(parts, "+") + "-" + node.id
}

// --- Skip policy ---

type skipResult struct {
	diverged           bool
	skippedWithDeps    bool // skipped a node that has dependents
	skippedLeaf        bool // skipped a leaf node only
}

func evalSkipPolicy(dag *rDAG, skipSet map[int]bool) (map[string]string, skipResult) {
	scope := map[string]string{}
	// Build reverse dep map to know who has dependents.
	hasDependents := map[string]bool{}
	for _, node := range dag.nodes {
		for _, dep := range node.deps {
			hasDependents[dep] = true
		}
	}

	skippedWithDeps := false
	skippedLeaf := false

	for _, idx := range dag.topo {
		if skipSet[idx] {
			node := dag.nodes[idx]
			if hasDependents[node.id] {
				skippedWithDeps = true
			} else {
				skippedLeaf = true
			}
			continue
		}
		node := dag.nodes[idx]
		scope[node.id] = evalNode(node, scope)
	}

	oracle := evalOracle(dag)
	diverged := false
	for k, v := range oracle {
		if scope[k] != v {
			diverged = true
			break
		}
	}
	// Also check for missing keys.
	if len(scope) != len(oracle) {
		diverged = true
	}

	return scope, skipResult{
		diverged:        diverged,
		skippedWithDeps: skippedWithDeps,
		skippedLeaf:     skippedLeaf,
	}
}

// --- Order policy ---

func evalOrderPolicy(dag *rDAG, order []int) map[string]string {
	scope := map[string]string{}
	for _, idx := range order {
		node := dag.nodes[idx]
		scope[node.id] = evalNode(node, scope)
	}
	return scope
}

// --- Stale scope policy ---

func evalStaleScopePolicy(dag *rDAG, staleSet map[int]bool) map[string]string {
	// Take a pre-evaluation snapshot (empty).
	staleScope := map[string]string{}
	scope := map[string]string{}

	for _, idx := range dag.topo {
		node := dag.nodes[idx]
		if staleSet[idx] {
			// Evaluate against stale (empty) scope.
			scope[node.id] = evalNode(node, staleScope)
		} else {
			scope[node.id] = evalNode(node, scope)
		}
	}
	return scope
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestOracleRandom_SkipPolicy(t *testing.T) {
	const seed = 42
	const numTrials = 200
	rng := rand.New(rand.NewSource(seed))

	var totalDiverged int
	var skippedWithDepsCount int
	var skippedLeafOnlyCount int

	for trial := 0; trial < numTrials; trial++ {
		n := 3 + rng.Intn(13) // 3-15 nodes
		dag := generateRandomDAG(rng, n)

		// Generate random skip set (each non-root node: 30% chance of skip).
		skipSet := map[int]bool{}
		for i := 1; i < n; i++ {
			if rng.Float64() < 0.3 {
				skipSet[i] = true
			}
		}
		if len(skipSet) == 0 {
			// Force at least one skip.
			skipSet[1+rng.Intn(n-1)] = true
		}

		_, result := evalSkipPolicy(dag, skipSet)
		if result.diverged {
			totalDiverged++
			if result.skippedWithDeps {
				skippedWithDepsCount++
			}
			if result.skippedLeaf && !result.skippedWithDeps {
				skippedLeafOnlyCount++
			}
		}
	}

	t.Logf("Skip policy: %d/%d perturbations diverged (seed=%d)", totalDiverged, numTrials, seed)
	t.Logf("  - Skipping node with dependents: %d cases", skippedWithDepsCount)
	t.Logf("  - Skipping leaf node only: %d cases (diverged because node itself missing from result)", skippedLeafOnlyCount)
}

func TestOracleRandom_OrderPolicy(t *testing.T) {
	const seed = 42
	const numTrials = 200
	rng := rand.New(rand.NewSource(seed))

	var totalDiverged int
	var nonTopoStaleReads int

	for trial := 0; trial < numTrials; trial++ {
		n := 3 + rng.Intn(13)
		dag := generateRandomDAG(rng, n)
		oracle := evalOracle(dag)

		// Generate random permutation of node indices.
		order := make([]int, n)
		for i := range order {
			order[i] = i
		}
		rng.Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] })

		result := evalOrderPolicy(dag, order)
		if !mapsEqual(oracle, result) {
			totalDiverged++
			// Check if this is because a node was evaluated before its deps.
			nonTopoStaleReads++
		}
	}

	t.Logf("Order policy: %d/%d perturbations diverged (seed=%d)", totalDiverged, numTrials, seed)
	t.Logf("  - Non-topological order caused stale reads: %d cases", nonTopoStaleReads)
}

func TestOracleRandom_StaleScopePolicy(t *testing.T) {
	const seed = 42
	const numTrials = 200
	rng := rand.New(rand.NewSource(seed))

	var totalDiverged int
	var staleWithDepsCount int

	for trial := 0; trial < numTrials; trial++ {
		n := 3 + rng.Intn(13)
		dag := generateRandomDAG(rng, n)
		oracle := evalOracle(dag)

		// Pick random nodes (non-root, with deps) to evaluate with stale scope.
		staleSet := map[int]bool{}
		for i := 1; i < n; i++ {
			if len(dag.nodes[i].deps) > 0 && rng.Float64() < 0.4 {
				staleSet[i] = true
			}
		}
		if len(staleSet) == 0 {
			// Force at least one stale node.
			for i := 1; i < n; i++ {
				if len(dag.nodes[i].deps) > 0 {
					staleSet[i] = true
					break
				}
			}
		}

		result := evalStaleScopePolicy(dag, staleSet)
		if !mapsEqual(oracle, result) {
			totalDiverged++
			staleWithDepsCount++
		}
	}

	t.Logf("Stale scope: %d/%d perturbations diverged (seed=%d)", totalDiverged, numTrials, seed)
	t.Logf("  - Stale scope for node with deps: %d cases", staleWithDepsCount)
}

func TestOracleRandom_Combined(t *testing.T) {
	const seed = 42
	const numTrials = 200
	rng := rand.New(rand.NewSource(seed))

	var totalDiverged int
	var skipDiverged, orderDiverged, staleDiverged int

	for trial := 0; trial < numTrials; trial++ {
		n := 3 + rng.Intn(13)
		dag := generateRandomDAG(rng, n)
		oracle := evalOracle(dag)

		// Combined perturbation: skip + reorder + stale scope.
		skipSet := map[int]bool{}
		staleSet := map[int]bool{}

		for i := 1; i < n; i++ {
			r := rng.Float64()
			if r < 0.15 {
				skipSet[i] = true
			} else if r < 0.30 && len(dag.nodes[i].deps) > 0 {
				staleSet[i] = true
			}
		}

		// Random order.
		order := make([]int, 0, n)
		for i := 0; i < n; i++ {
			if !skipSet[i] {
				order = append(order, i)
			}
		}
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

		// Evaluate with combined perturbations.
		staleScope := map[string]string{}
		scope := map[string]string{}
		for _, idx := range order {
			node := dag.nodes[idx]
			if staleSet[idx] {
				scope[node.id] = evalNode(node, staleScope)
			} else {
				scope[node.id] = evalNode(node, scope)
			}
		}

		if !mapsEqual(oracle, scope) {
			totalDiverged++
			// Attribute to primary cause (rough heuristic).
			if len(skipSet) > 0 {
				skipDiverged++
			}
			// Check if order is non-topological for non-skipped nodes.
			isNonTopo := false
			posMap := map[int]int{}
			for pos, idx := range order {
				posMap[idx] = pos
			}
			for _, idx := range order {
				for _, dep := range dag.nodes[idx].deps {
					depIdx := -1
					for j, nd := range dag.nodes {
						if nd.id == dep {
							depIdx = j
							break
						}
					}
					if depIdx >= 0 && !skipSet[depIdx] {
						if posMap[depIdx] > posMap[idx] {
							isNonTopo = true
						}
					}
				}
			}
			if isNonTopo {
				orderDiverged++
			}
			if len(staleSet) > 0 {
				staleDiverged++
			}
		}
	}

	t.Logf("Combined policy: %d/%d perturbations diverged (seed=%d)", totalDiverged, numTrials, seed)
	t.Logf("  - Perturbations involving skip: %d", skipDiverged)
	t.Logf("  - Perturbations involving non-topological order: %d", orderDiverged)
	t.Logf("  - Perturbations involving stale scope: %d", staleDiverged)
	t.Logf("")
	t.Logf("Bug class discovery summary:")
	t.Logf("  Skip policies find the 'ReadinessDependents never triggered' pattern: skipping a node with dependents causes downstream corruption")
	t.Logf("  Stale scope policies find the 'stale snapshot' pattern: evaluating with pre-update scope causes wrong results")
	t.Logf("  Order policies find the 'concurrent dispatch' pattern: non-topological order means evaluating before deps are ready")
}
