// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/compiler"
	"sigs.k8s.io/krocodile/pkg/runtime"
	"sigs.k8s.io/krocodile/pkg/watchrouter"
)

// Simple is the v1 executor: walk nodes in topological order, SSA-apply
// each Template, record observed state on the runtime so dependents see
// the live cluster values, and on Delete tear them down in reverse.
//
// Ignored nodes (includeWhen=false, or contagiously via an ignored
// upstream) are skipped entirely — no resolve, no apply, no scope
// publication. ReadyWhen checks gate the loop: an unsatisfied readyWhen
// returns ErrNotReady so the reconciler requeues.
type Simple struct {
	Client client.Client
}

// NewSimple constructs a Simple executor bound to the given client.
func NewSimple(c client.Client) *Simple {
	return &Simple{Client: c}
}

var _ Interface = (*Simple)(nil)

// Apply walks rt in topological order. For each node it checks ignore
// status (contagious), resolves the desired state, registers a watch on
// the resulting GVR/Name/Namespace via w, applies to cluster (or just
// renders for Def), records observed state on the node, and finally
// checks readyWhen — surfacing an unsatisfied result as ErrNotReady so
// the reconciler requeues without backoff.
//
// Per-template watches are registered BEFORE SSA apply. Doing it before
// closes the window where an external actor could mutate the object
// between apply and watch registration — the informer cache picks up
// the next change either way, but the watch must exist first or the
// event gets dropped.
//
// Soft errors (ErrDataPending from Resolve, ErrWaitingForReadiness from
// CheckReadiness) do NOT abort the walk. The reconciler relies on every
// reachable node getting its watch declared so drift detection stays
// authoritative — bailing early on a not-ready upstream node would
// leave downstream nodes' watches missing, and the next reconcile
// would lose drift events on them. Soft errors are remembered and the
// first one is returned at the end wrapped in ErrNotReady. Hard errors
// (apply failure, type errors, etc.) still abort immediately.
func (s *Simple) Apply(ctx context.Context, rt *runtime.Runtime, w watchrouter.Watcher) (ApplyResult, error) {
	var result ApplyResult
	var firstSoft error
	recordSoft := func(err error) {
		if firstSoft == nil {
			firstSoft = err
		}
	}

	for _, n := range rt.Nodes() {
		ignored, err := n.IsIgnored()
		if err != nil {
			if isSoftRuntimeErr(err) {
				// includeWhen referenced data the cluster hasn't
				// produced yet. We can't decide whether to apply,
				// so identities are unknown — caller preserves the
				// previous entries for this NodeID.
				result.Unresolved = append(result.Unresolved, n.ID())
				recordSoft(fmt.Errorf("apply %q: includeWhen: %w (%w)", n.ID(), err, ErrNotReady))
				continue
			}
			return result, fmt.Errorf("apply %q: %w", n.ID(), err)
		}
		if ignored {
			// Intentionally skipped — not Unresolved. The caller
			// will treat any previous entries for this NodeID as
			// prune candidates.
			continue
		}

		desired, err := n.Resolve()
		if err != nil {
			if isSoftRuntimeErr(err) {
				result.Unresolved = append(result.Unresolved, n.ID())
				recordSoft(fmt.Errorf("apply %q: resolve: %w (%w)", n.ID(), err, ErrNotReady))
				continue
			}
			return result, fmt.Errorf("apply %q: resolve: %w", n.ID(), err)
		}
		switch n.Kind() {
		case compiler.NodeKindDef:
			// Def nodes have no cluster I/O — no managed-resource entries.
			n.SetObserved(desired, desired)
			publishScope(rt, n, n.Observed())
		case compiler.NodeKindTemplate:
			for _, obj := range desired {
				s.defaultNamespace(rt, n, obj)
				if err := s.watchTemplate(w, n, obj); err != nil {
					return result, fmt.Errorf("apply %q: register watch: %w", n.ID(), err)
				}
				if err := s.ssaApply(ctx, obj); err != nil {
					return result, fmt.Errorf("apply %q: %w", n.ID(), err)
				}
				// SSA returned UID + server-managed fields on obj.
				// Record the identity AFTER apply succeeded so we
				// never advertise tracking for resources that didn't
				// actually land.
				result.Applied = append(result.Applied, managedResourceFrom(n, obj))
			}
			n.SetObserved(desired, desired)
			publishScope(rt, n, n.Observed())
		case compiler.NodeKindRef, compiler.NodeKindWatch:
			return result, fmt.Errorf("apply %q (%s): %w", n.ID(), n.Kind(), ErrUnsupported)
		default:
			return result, fmt.Errorf("apply %q: unknown kind %v", n.ID(), n.Kind())
		}

		// readyWhen is checked after observed state is recorded.
		// Soft → ErrNotReady (already tracked in Applied); continue
		// so downstream watches still register. Hard → abort.
		if err := n.CheckReadiness(); err != nil {
			if isSoftRuntimeErr(err) {
				recordSoft(fmt.Errorf("apply %q: %w (%w)", n.ID(), err, ErrNotReady))
				continue
			}
			return result, fmt.Errorf("apply %q: %w", n.ID(), err)
		}
	}
	return result, firstSoft
}

// managedResourceFrom builds a ManagedResource pointer from a node and
// its post-apply unstructured object. The UID is captured from the SSA
// response so the Reconciler can use it as a delete precondition later.
func managedResourceFrom(n *runtime.Node, obj *unstructured.Unstructured) expv1alpha1.ManagedResource {
	gvk := obj.GroupVersionKind()
	return expv1alpha1.ManagedResource{
		NodeID:     n.ID(),
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
		UID:        string(obj.GetUID()),
	}
}

// isSoftRuntimeErr classifies a runtime-package error as a retryable
// "cluster hasn't converged yet" signal versus a hard error. Both
// ErrDataPending and ErrWaitingForReadiness mean "try again later" —
// the executor records them and continues so the rest of the graph
// still gets its watches declared.
func isSoftRuntimeErr(err error) bool {
	return errors.Is(err, runtime.ErrDataPending) || errors.Is(err, runtime.ErrWaitingForReadiness)
}

// publishScope writes the supplied objects back to the runtime scope.
// Collection nodes get a []any so downstream CEL list functions work;
// singletons get the lone object map.
func publishScope(rt *runtime.Runtime, n *runtime.Node, objs []*unstructured.Unstructured) {
	if n.IsCollection() {
		list := make([]any, 0, len(objs))
		for _, obj := range objs {
			list = append(list, obj.Object)
		}
		rt.Set(n.ID(), list)
		return
	}
	if len(objs) == 0 {
		return
	}
	rt.Set(n.ID(), objs[0].Object)
}

// Delete removes resources in reverse of the supplied slice order so
// dependents go before dependencies. Identity comes from the persisted
// ManagedResources list — no re-resolve of the current spec, so a
// Graph whose templates were renamed or whose forEach shrunk between
// apply and delete still gets every prior resource removed.
//
// UID precondition guards against deleting an impostor that some other
// actor created after the resource we tracked was removed out of band.
// NotFound and "already deleted by something else" are tolerated.
func (s *Simple) Delete(ctx context.Context, resources []expv1alpha1.ManagedResource) error {
	for i := len(resources) - 1; i >= 0; i-- {
		r := resources[i]
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(r.APIVersion)
		obj.SetKind(r.Kind)
		obj.SetNamespace(r.Namespace)
		obj.SetName(r.Name)

		opts := []client.DeleteOption{}
		if r.UID != "" {
			uid := types.UID(r.UID)
			opts = append(opts, &client.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid},
			})
		}

		if err := s.Client.Delete(ctx, obj, opts...); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			// UID-precondition mismatch surfaces as Conflict — the
			// resource we tracked is gone and a different object now
			// occupies its identity. Not our problem; skip.
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("delete %s/%s %s: %w", r.APIVersion, r.Kind, refName(r), err)
		}
	}
	return nil
}

func refName(r expv1alpha1.ManagedResource) string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}


// watchTemplate registers a scalar watch on the resolved Template
// object so the dynamic controller re-enqueues the Graph when the
// resource changes out from under us. Cluster-scoped resources are
// watched with Namespace="". A nil watcher is treated as a Noop.
func (s *Simple) watchTemplate(w watchrouter.Watcher, n *runtime.Node, obj *unstructured.Unstructured) error {
	if w == nil {
		return nil
	}
	return w.Watch(watchrouter.WatchRequest{
		NodeID:    n.ID(),
		GVR:       n.GVR(),
		Name:      obj.GetName(),
		Namespace: obj.GetNamespace(),
	})
}

// defaultNamespace fills in metadata.namespace from the Graph for
// namespaced GVRs when the template left it empty. Cluster-scoped GVRs
// are untouched.
func (s *Simple) defaultNamespace(rt *runtime.Runtime, n *runtime.Node, obj *unstructured.Unstructured) {
	if !n.Namespaced() || obj.GetNamespace() != "" {
		return
	}
	if ns := rt.Graph().GetNamespace(); ns != "" {
		obj.SetNamespace(ns)
	}
}

// ssaApply server-side applies obj with the krocodile FieldManager. We
// force ownership so re-applies after a hand-edit converge back.
func (s *Simple) ssaApply(ctx context.Context, obj *unstructured.Unstructured) error {
	return s.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(FieldManager), client.ForceOwnership)
}
