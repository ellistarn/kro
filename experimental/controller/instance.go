// instance.go holds per-Graph mutable reconcile-time state.
//
// In the stateless reconciler model, instanceState is ephemeral —
// created fresh every reconcile from compilation output. No cross-cycle
// state is preserved. The InstanceMap exists solely to support the
// onNewType callback (requeue all graphs when a new CRD is observed).
package graphcontroller

import (
	"sync"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
)

// instanceState holds the compilation artifacts for a single reconcile cycle.
// Created fresh every compileRevision call. The only cross-cycle state is
// forEachItemHashes, which is carried forward from the previous instance
// to enable forEach incremental evaluation.
type instanceState struct {
	compilation compiledArtifacts

	// forEachItemHashes stores per-item content hashes from the most recent
	// reconcile cycle, keyed by node ID then item identity. Used by the
	// forEach incremental evaluation optimization to skip unchanged items.
	// Nil on cold start; populated after the first successful forEach
	// evaluation for self-contained bindings.
	forEachItemHashes map[string]map[string]uint64

	// forEachPreviousScope stores the per-item scope entries from the most
	// recent successful reconcile, keyed by node ID then item identity.
	// Carried forward for unchanged items in self-contained forEach bindings.
	forEachPreviousScope map[string]map[string]any

	// forEachPreviousKeys stores the per-item applied keys from the most
	// recent successful reconcile, keyed by node ID then item identity.
	// Carried forward for unchanged items in self-contained forEach bindings.
	forEachPreviousKeys map[string]map[string][]Applied
}

// compiledArtifacts holds the output of a single compilation.
type compiledArtifacts struct {
	compiled *compiler.CompiledGraph
	dag      *dagpkg.DAG
}

// newInstanceState creates a fresh instanceState.
func newInstanceState(compiled *compiler.CompiledGraph, dag *dagpkg.DAG) *instanceState {
	return &instanceState{
		compilation: compiledArtifacts{
			compiled: compiled,
			dag:      dag,
		},
	}
}

// ---------------------------------------------------------------------------
// instanceMap — concurrent-safe wrapper around per-instance state.
// Exists to support the onNewType callback (requeue all cached graphs).
// ---------------------------------------------------------------------------

// InstanceMap is a concurrent-safe map of per-instance state keyed by
// namespace/revision-name.
type InstanceMap struct {
	mu        sync.RWMutex
	instances map[string]*instanceState
}

func newInstanceMap() *InstanceMap {
	return &InstanceMap{
		instances: make(map[string]*instanceState),
	}
}

func (m *InstanceMap) get(key string) *instanceState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances[key]
}

func (m *InstanceMap) set(key string, s *instanceState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[key] = s
}

func (m *InstanceMap) remove(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.instances, key)
}

// CacheSizes returns the number of cached instance states.
func (m *InstanceMap) CacheSizes() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.instances)
}

// instanceKeys returns a snapshot of all instance cache keys.
// Used by the onNewType callback to requeue all cached graphs
// when a new type becomes watchable.
func (m *InstanceMap) instanceKeys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.instances))
	for k := range m.instances {
		keys = append(keys, k)
	}
	return keys
}
