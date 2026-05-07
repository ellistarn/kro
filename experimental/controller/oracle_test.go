// oracle_test.go is the enforcement point for the equivalence property:
//
//   For any input, the optimized algorithm must produce the same propagateResult
//   as the simple sequential algorithm.
//
// Per 005-reconciliation-optimized.md § Constraint:
//   "Optimizations that change observable behavior — even improvements — are
//    specification changes, not optimizations."
//
// Every optimization adds its oracle equivalence test here. The individual test
// files (oracle_bugs_test.go, oracle_random_test.go, etc.) contain the generators
// and bug reproductions. This file is the table of contents.
//
// Equivalence tests (must all pass for the optimization to ship):
//
//   TestPropagateOracle                          — simple 3-node chain (propagate_pbt_test.go)
//   TestScopedPropagation_ColdStart              — scoped == simple on first reconcile (propagate_scoped_test.go)
//   TestScopedPropagation_SingleChange           — scoped == simple when one root changes (propagate_scoped_test.go)
//   TestScopedPropagation_OracleEquivalence      — scoped == simple on multi-node tree (propagate_scoped_test.go)
//   TestForEach_IncrementalEvaluation            — forEach skip == full re-eval (foreach_design_test.go)
//   TestOracleForEach_RandomGraphs               — random forEach inputs, optimization == oracle (oracle_foreach_test.go)
//   TestOracleForEach_ScaleDown                  — item removal, optimization == oracle (oracle_foreach_test.go)
//
// Bug reproduction tests (prove the oracle catches known failure modes):
//
//   TestOracleBug_ReadinessDependentsNeverTriggered  — skip policy diverges (oracle_bugs_test.go)
//   TestOracleBug_StaleSnapshotConcurrentEvaluation  — stale scope diverges (oracle_bugs_test.go)
//   TestOracleBug_ForEachStaleCarryForward           — incorrect SelfContained diverges (oracle_bugs_test.go)
//   TestOracleForEach_SelfContainedViolation         — random detection of SelfContained bug (oracle_foreach_test.go)
//
// Random exploration tests (undirected bug discovery):
//
//   TestOracleRandom_SkipPolicy      — random node skipping, 100% divergence (oracle_random_test.go)
//   TestOracleRandom_OrderPolicy     — random reordering, 95% divergence (oracle_random_test.go)
//   TestOracleRandom_StaleScopePolicy — random stale scope, 100% divergence (oracle_random_test.go)
//   TestOracleRandom_Combined        — all perturbations, 96% divergence (oracle_random_test.go)
//
package graphcontroller
