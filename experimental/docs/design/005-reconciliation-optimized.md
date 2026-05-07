# Graph Reconciliation — Optimized

Optimized reconciliation layered on top of [005-reconciliation](005-reconciliation.md). The simple algorithm evaluates every node sequentially on every reconcile. The optimized version narrows evaluation to affected nodes and skips work whose result would be the same. Correctness is defined as: same output as the simple algorithm on the same inputs. The simple algorithm is the test oracle.

## Constraint

Every optimization must satisfy: for any input state, the optimized propagation produces the same `propagateResult` (same applied keys, same node states, same scope entries, same summary) as the simple sequential walk. Optimizations that change observable behavior — even improvements — are specification changes, not optimizations, and belong in [005-reconciliation](005-reconciliation.md).

## Tree-Walking Hasher

The apply-hash comparison runs on every node every reconcile (it gates the SSA write). The hasher replaces `json.Marshal` + FNV with direct tree-walking of `map[string]any`: no reflect, no intermediate JSON bytes, keys sorted for determinism. `sync.Pool`'d buffer, single FNV pass.

The hasher is a drop-in replacement for the hashing primitive. No new state, no behavioral change. 2.3x faster per hash (measured on prior implementation). The apply-hash annotation path is unchanged.

## forEach Incremental Evaluation

The simple algorithm evaluates all forEach children every reconcile. For a collection of N items where K changed, the optimized version evaluates only K children.

**Mechanism.** At compile time, the forEach binding's dependency structure determines whether the collection input is the sole variable for each child's template. When it is, an unchanged collection item produces an unchanged template — provable without evaluation.

Per reconcile:
1. Hash each collection item by content.
2. Compare against the previous reconcile's per-item hashes (stored on the parent's instance state).
3. Unchanged items: carry forward the previous applied keys and scope entry. No template evaluation, no SSA apply attempt.
4. Changed items: evaluate normally.

The per-item hash is the only new state. It lives on the parent node's instance state (already per-reconcile) and is recomputed from scratch on cold start.

**Precondition.** The optimization applies only when the forEach binding's input dependencies are fully captured by the collection item. If the child template references scope outside the iterator variable, all children must re-evaluate when that scope changes. The compiler detects this statically and marks the binding `forEachSelfContained: true` when every field path in the child template resolves within the iterator variable.

**Equivalence.** An unchanged item produces the same template, which produces the same apply-hash, which produces the same SSA outcome (skip or no-op write), which produces the same scope entry. Carrying forward the previous result is equivalent to re-evaluating.

## Scoped Propagation

The simple algorithm walks all nodes in topological order. The optimized version walks only nodes reachable from a change.

**Mechanism.** At compile time, build a forward-reachability map: for each node, which other nodes can it eventually affect? This is a static property of the DAG, recomputed only when compilation produces a new DAG.

Per reconcile:
1. Identify triggered nodes — nodes whose inputs changed since last evaluation. A node is triggered when its informer resourceVersion differs from the recorded value, or when it was affected by a compilation change.
2. Compute the affected set: the union of `reachability[node]` for each triggered node, plus the triggered nodes themselves.
3. Walk only nodes in the affected set, in topological order. Unaffected nodes retain their previous state and scope entry.

**Cold start.** On first reconcile (no previous state), all nodes are triggered. The affected set is the full DAG. The optimized version degrades to the simple algorithm.

**Resync.** A periodic full-graph reconcile (controller-runtime requeue at a configured interval) triggers all nodes, ensuring eventual consistency even if a watch event is missed. The resync interval is the consistency floor — the bound on how long any divergence can persist.

**Equivalence.** An unaffected node's hard dependencies are also unaffected (by the forward-reachability definition). An unaffected node with unchanged inputs and unchanged dependency outputs produces the same evaluation result. Retaining the previous state is equivalent to re-evaluating.

**What this replaces.** The prior trigger-scoped walk failed because triggers were mutable runtime state that had to be complete to be correct — and if one was missed, the node was never walked. The reachability map is static and computed from the DAG. Missed triggers cannot cause permanent staleness because the resync interval forces a full walk periodically. If a watch event is missed, the affected node stays stale until the next resync — bounded staleness instead of the simple algorithm's guarantee of evaluating everything every reconcile.

## What is deferred

**Parallel evaluation.** Nodes with no dependency relationship could evaluate concurrently. The correctness invariant (hard dependencies processed first) is easy to enforce with a dependency-counting scheduler. Deferred because: the dominant cost is API writes (gated by apply-hash), sequential evaluation eliminates concurrency bugs by construction, and most graphs have linear dependency chains. Profile first.

**Compilation cache.** Multiple forEach children with identical structure could share a single compiled artifact. Deferred because: compilation runs only on spec change (not per-reconcile), the cost is amortized over revision lifetime, and the prior cache implementation had lifecycle bugs. The simple mechanism (compile once, store on the reconciler, share by reference) may be sufficient without content-addressing.

**Output-hash propagation bounding.** After a node evaluates, hash its scope output and compare to the previous. If unchanged, don't trigger dependents. This narrows the affected set further than input-reachability alone. Deferred because: it requires per-node output state, and scoped propagation already eliminates the bulk of unnecessary work. Add if profiling shows propagation fan-out is the bottleneck.
