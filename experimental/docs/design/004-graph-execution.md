# Graph Execution

How the controller reconciles a Graph. The DAG is the dependency structure between nodes.
Watches bring external state in. The walk converges the DAG toward the active revision's desired
state. Performance is structural — work is proportional to change, not to DAG size.

## The DAG

A directed acyclic graph of nodes. Each node declares exactly one Kubernetes resource. Edges are
dependencies inferred from CEL expression references. A revision materializes a DAG — the static
structure the walk operates on.

### Nodes

A node has:

- **Identity** — node ID within the Graph's scope
- **Template** — desired state declaration with `${...}` CEL expressions referencing other nodes
- **Dependencies** — nodes this node's CEL expressions reference (edges in the DAG)

Template shape determines how the node is processed and cleaned up:

- **Owns** — creates and manages the resource. Deletes on prune.
- **Watch** — reads the resource into scope. No management.
- **Collection Watch** — discovers matching resources by selector. Enters an array into scope.
- **Contribute** — writes partial state to another actor's resource. Releases fields on prune.

### Dependencies

Dependencies are inferred from CEL expression references. If node B's template contains
`${A.metadata.name}`, B depends on A. The dependency graph must be acyclic — cycles are rejected at
compile time.

Declaration order in the spec is irrelevant. The dependency graph determines execution order.
Nodes with no dependency relationship are independent and can be processed in parallel.

## The Walk

Each reconcile walks every node in topological order. At each node: should I evaluate?

A node evaluates when any of these hold:

- A watch event fired for this node
- A dependency was re-evaluated during this walk
- The node's `next-sync` label has passed
- The node is in a transient error state (SystemError, Conflict) — backoff retry with a low cap,
  then wait for `next-sync`
- First reconcile or revision transition (all nodes)

Deterministic errors (4xx) are not retried — same inputs produce the same failure. They resolve via
propagation (upstream input changes), revision transition (user fixes the spec), or `next-sync` (the
consistency floor).

Otherwise — skip. O(1) per skipped node.

After evaluation, the node's output is hashed — only the specific field paths that downstream CEL
expressions reference (determined statically at graph compilation from expression ASTs). The hash
input is the union of all downstream-referenced paths. Absent paths hash to a fixed sentinel value
that is not a valid Kubernetes field value — the transition from absent to present is a change, not
a skip. If the hash changed — or no previous hash exists — dependents evaluate when visited later in
topological order. If not, propagation stops. Changes flow forward through the DAG and stop when they
stop mattering.

Every apply writes a `next-sync` label — a jittered future timestamp (default 5 minutes, jitter
up to the sync interval). `next-sync` is read from the managed resource's label, not generated fresh
each reconcile. During template hash computation, the controller includes the existing `next-sync`
value in the desired state. When `next-sync` has not expired, the desired state includes the same
timestamp as the managed resource — the template hash matches and the write is skipped. When
`next-sync` has expired, the controller generates a new jittered timestamp — the template hash
differs and SSA apply fires, correcting drift as a side effect. This read-back cycle is what makes
`next-sync` both sticky (no apply every reconcile) and triggering (apply on expiration). Jitter is
per-node, baked into the label, so expirations are naturally decorrelated across nodes. This replaces
periodic full-graph resync with amortized per-node resync. The reconciler returns `RequeueAfter` set
to the minimum `next-sync` expiration across all nodes minus current time, ensuring expiration is
detected promptly even when no watch events fire.

The controller uses metadata-only informers — labels are visible, annotations are not. Full object
reads happen only during evaluation (step 5). When an evaluated node needs data from a skipped
dependency, the full object is read from the API server on demand. If absent (deleted externally,
not yet created), the node is Pending.

**Storage model.** All controller-managed fields on managed resources use labels — metadata-only
informers can read labels without fetching the full object. Ownership labels and template hash are
materialized into the revision spec (they feed the hash that drives change detection). Runtime-only
fields (`next-sync`) are injected at apply time. The `internal.kro.run/` prefix marks all
controller-internal fields.

| What | Where | Purpose |
|---|---|---|
| `graph-name`, `graph-generation`, `node-id` | Managed resource labels | Watch routing, ownership |
| `template-hash` | Managed resource label | Skip write if desired state unchanged |
| `next-sync` | Managed resource label | Resync scheduling |
| `resolvedKey`, `appliedKeys` | Revision status (per-node) | Graph topology state |
| Propagation hash | In-memory (per reconcile pass) | Skip downstream evaluation |

### Wind

Forward topological walk. Every dependency is visited before its dependents — steps read
current-reconcile state for dependencies, which is always available.

1. **Skip check** — not triggered and no dependency re-evaluated → skip. Retains previous plan
   state and applied set entries.
2. **Dependency check** — any dependency Excluded → Excluded (definitive absence propagates;
   the node is structurally non-viable). Any dependency in a blocked state → inherit Blocked.
   Excluded takes precedence over Blocked: if a node has both, it is Excluded — the blocked
   dependency's resolution cannot make the node viable while the Excluded dependency is absent.
3. **propagateWhen** — any dependency's propagateWhen unsatisfied → gate. Template not re-evaluated.
   Previous plan state retained (or Pending if never evaluated). Gate takes precedence over
   triggers. When the gate opens on a later reconcile, the node evaluates with current cluster
   state — changes during the gate period are visible at that point.
4. **includeWhen** — false → Excluded.
5. **Dispatch:**
   - Watch: GET full object. Data enters scope. Pending if absent.
   - Collection Watch: GET matching objects. Array enters scope.
   - forEach: evaluate collection, diff items, process changed children (see [forEach](#foreach)).
   - Owns: evaluate template, inject `next-sync`, hash desired state, compare against
     `template-hash` label on managed resource. Match → skip write. Differs → SSA apply.
   - Contribute: same as Owns but force-apply. Auto-splits status subresource.
6. **readyWhen** — Ready or NotReady. Data is in scope regardless.
7. **Propagation check** — hash the specific field paths dependents reference (union of downstream CEL
   access chains) + propagateWhen state, compare against the previous reconcile's in-memory
   propagation hash. Differs → mark re-evaluated. Matches → propagation stops. On first pass after
   restart, no previous value exists — all nodes evaluate through step 5.

Node's data enters scope after processing. Two hashing boundaries: template hash (step 5) skips the
write, propagation hash (step 7) skips downstream evaluation.

### Plan States

Each evaluated node lands in exactly one state:

| State       | Meaning                        | Dependents | Resolution                |
|-------------|--------------------------------|------------|---------------------------|
| Ready       | Applied, readyWhen satisfied   | Proceed    | —                         |
| NotReady    | Applied, readyWhen unsatisfied | Proceed    | Converges via watch       |
| Pending     | Data not yet available         | Blocked    | Upstream resolves         |
| Excluded    | includeWhen false              | Excluded   | includeWhen inputs change |
| Blocked     | Dependency in error state      | Blocked    | Dependency resolves       |
| Conflict    | Field ownership contested      | Blocked    | Backoff retry, then `next-sync` |
| Error       | Client request failed (4xx)    | Blocked    | Propagation, revision, or `next-sync` |
| SystemError | Server/infra failure (5xx)     | Blocked    | Backoff retry, then `next-sync` |

Ready and NotReady are both "applied and in scope." readyWhen is a health signal — it does not gate
dependents. Blocked states propagate as Blocked (uncertain absence — previous applied keys retained,
not safe to prune). Excluded propagates as Excluded (definitive absence — safe to prune).

### Prune

After wind, diff the current key set against the applied set. Absent resources are prune candidates
if their absence is definitive (Excluded, removed from revision). Uncertain absence (Pending,
Error, SystemError) blocks pruning — the resource might reappear once the blocker resolves.

Conflict is deliberately excluded from the prune gate: a 409 is positive evidence that the resource
exists (another actor owns contested fields). Prune is the designed recovery mechanism for
conflicts during revision transitions — the old revision's resource is removed from the applied
set, and the new revision's template creates it fresh without the contested field ownership. This
implements "a template change clears the conflict state" from the ownership design.

Prune in reverse dependency order. Owns → delete. Contribute → release fields via skeleton apply.
Watch/Collection Watch → no action. If reverse ordering is unavailable, prune is blocked — never
degrade to unordered deletion. If a prune candidate has `finalizes` references, finalization runs
first (see [Teardown](#teardown)).

### Applied Set

The applied set (GVK + namespace + name per resource) lives in the GraphRevision status, keyed by
node ID. It reflects what was actually written — errored nodes don't contribute. Committed as a
single status update after wind completes.

The walk evaluates a subset of nodes but the applied set must be complete. Merge: evaluated nodes
replace their entries, skipped nodes retain previous entries. Excluded nodes' previous keys are
removed (Excluded is an evaluation outcome, not a skip). Revision transitions treat all nodes as
triggered — stale entries from a changed DAG structure must be cleaned up.

Prune candidates = union of superseded revisions' applied sets − active revision's applied set.

forEach children use the same managed resource labels and annotations as static nodes (`next-sync`,
`template-hash`). The parent's entry in revision status holds all children's `appliedKeys` — children
do not get individual revision status entries (scaling: a forEach with 1000 items would bloat the
revision status otherwise). Propagation hashes for children are held in-memory like all other nodes —
children can have dependents without storage model constraints. Since children are dynamic cardinality,
in-memory propagation hashes avoid cleanup logic for children that no longer exist.

## forEach

A forEach node is a parent that stamps child nodes — one per item in a collection. Each child is an
independent node in the DAG with its own template, hash, and dependency edges.

```
                                forEach parent
                                ┌──────────┐
    ${namespaces} ─────────────▶│ policies │
                                │ (parent) │
                                └────┬─────┘
                          ┌──────────┼──────────┐
                          ▼          ▼          ▼
                     ┌─────────┐ ┌─────────┐ ┌─────────┐
                     │ pol/ns-a│ │ pol/ns-b│ │ pol/ns-c│
                     │ (child) │ │ (child) │ │ (child) │
                     └─────────┘ └─────────┘ └─────────┘
```

**The parent node** evaluates the collection expression and holds each item's last-known state
in-memory, keyed by the item's identity (stable name or UID, not array position). On restart, no
cached state exists — the full collection is re-evaluated and all children are processed (consistent
with the "first reconcile" trigger). The parent completes when all children have been processed —
applied and in scope. Downstream nodes that depend on the forEach proceed once the parent completes.
They do not wait for every child to satisfy readyWhen — data availability is the gate, not health.

**Child nodes** bind the iterator variable to their item and evaluate the template independently.
Each child is identified by the collection item it's bound to. If the collection returns items in a
different order, the children are the same — same identities, same templates, no churn.

**On reconcile,** the parent diffs the current collection against the previous state:

1. Re-evaluate the collection expression
2. Diff item identities against previous identities — detect adds, removes, changes
3. Changed items: update state, re-evaluate that child's template
4. Unchanged items: skip template evaluation

forEach evaluates only changed items, not the entire collection. The identity diff is negligible.
Template evaluations — the real cost — only run for items whose data actually changed. New items
create child nodes. Removed items are pruned. If the collection expression cannot evaluate (upstream
is Pending), the intended item set is unknown — prune is blocked and existing children persist until
the next successful evaluation.

### forEach Strategies

forEach stamps a single template per item. If you need multiple resources per item, two options:

**Sibling forEach blocks** — two forEach entries with the same collection expression. Dependencies
between them are expressed in the outer DAG. This is the default. Use it when each item needs a
small number of resources with straightforward dependencies between them.

```yaml
- id: quotas
  forEach:
    ns: ${namespaces}
  template:
    apiVersion: v1
    kind: ResourceQuota
    metadata:
      name: default
      namespace: ${ns.metadata.name}
    spec:
      hard:
        pods: "10"

- id: policies
  forEach:
    ns: ${namespaces}
  template:
    apiVersion: networking.k8s.io/v1
    kind: NetworkPolicy
    metadata:
      name: default-deny
      namespace: ${ns.metadata.name}
    spec:
      podSelector: {}
```

**Nested Graphs** — forEach stamps a child Graph per item. Each child Graph is independently
reconciled by its own watches. Use nested Graphs when per-item isolation matters — a failure in one
item shouldn't affect others' reconcile cadence — or when each item's subgraph is complex
enough that you'd write a separate Graph for it by hand.

```yaml
- id: perNamespace
  forEach:
    ns: ${namespaces}
  template:
    apiVersion: experimental.kro.run/v1alpha1
    kind: Graph
    metadata:
      name: ${ns.metadata.name}-resources
    spec:
      nodes:
        - id: nsRef
          template:
            apiVersion: v1
            kind: Namespace
            metadata:
              name: ${ns.metadata.name}
        - id: quota
          template: ...
        - id: policy
          template: ...
```

Flat forEach is simpler — fewer objects, one reconcile loop. Nested Graphs cost more objects but
decouple per-item reconciliation entirely. Start with flat forEach. Move to nested when you need
per-item isolation or the per-item subgraph outgrows a single template.

## Concrete Example

A Graph that creates a Deployment from a watched ConfigMap, a Service for the Deployment, and a
NetworkPolicy per Namespace:

```yaml
spec:
  nodes:
    - id: config
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: app-config

    - id: namespaces
      template:
        apiVersion: v1
        kind: Namespace
        selector: {}

    - id: deploy
      readyWhen:
        - ${deploy.status.availableReplicas > 0}
      propagateWhen:
        - ${deploy.status.updatedReplicas == deploy.spec.replicas}
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: my-app
        spec:
          replicas: 3
          selector:
            matchLabels:
              app: my-app
          template:
            metadata:
              labels:
                app: my-app
            spec:
              containers:
                - name: app
                  image: ${config.data.image}

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${deploy.metadata.name}-svc
        spec:
          selector: ${deploy.spec.selector.matchLabels}
          ports:
            - port: 80

    - id: policies
      forEach:
        ns: ${namespaces}
      template:
        apiVersion: networking.k8s.io/v1
        kind: NetworkPolicy
        metadata:
          name: default-deny
          namespace: ${ns.metadata.name}
        spec:
          podSelector: {}
          policyTypes:
            - Ingress
```

This produces the following DAG:

```
Level 0:  config (Watch)      namespaces (Collection Watch)
              │                       │
Level 1:  deploy (Owns)       policies (forEach parent)
              │                  ┌────┼────┐
Level 2:  service (Owns)     pol/a  pol/b  pol/c  (forEach children)
```

Two independent branches. A change to config propagates through the left branch — deploy and service
are evaluated, the right branch skips. A change to a Namespace propagates through the right branch —
policies and children are evaluated, the left branch skips.

## Revision Transitions

A revision materializes a DAG. There is always exactly one target DAG — the latest revision's. The
controller winds toward it.

When the Graph's spec changes, a new revision is materialized. The controller abandons any
in-progress convergence and begins converging toward the new one. Same walk, same prune — but a
revision change treats all nodes as triggered. The DAG structure changed; the complete key set must
be recomputed. Each node receives a fresh jittered `next-sync` timestamp after its first apply under
the new revision.

If revision N is propagating when N+1 arrives, N is abandoned. Resources that N partially created
either match N+1's templates (kept) or don't (pruned). Resources that N never created either appear
in N+1's DAG or don't (nothing to prune). There is no concurrent convergence across revisions.

### Prune Ordering from Superseded Revisions

Pruning requires reverse dependency ordering — if B depends on A and both are being pruned, B must be
deleted before A. The ordering comes from the most recent revision that defined each pruned resource.

A superseded revision is retained until its unique resources are pruned. This is its only purpose —
providing prune ordering, template shape (Owns vs Contribute), and finalizes metadata for each
pruned resource. The old revision's `finalizes` declarations govern the prune of its resources — if a
new revision changes or drops `finalizes`, the old revision's metadata still applies to resources
being pruned from it. Fast spec
mutations cause superseded revisions to accumulate. This is bounded by the mutation rate — a natural
bottleneck users manage by not mutating faster than the controller converges.

## Worked Example

Tracing the concrete example through a sequence of events: a config change, a forEach scale-up, and
a revision transition.

**Initial state:** fully converged. `data.image: nginx:1.25`. Namespaces a, b, c.

---

**Event 1: ConfigMap updated** — `data.image` changed to `nginx:1.26`.

Watch event fires for node `config`. Walk iterates all nodes; changes propagate through the
left branch:

| Node       | Triggered? | Dep re-evaluated? | Action                                                                    | Result   |
|------------|------------|--------------------|---------------------------------------------------------------------------|----------|
| config     | Yes        | —                  | GET full object → propagation hash differs → re-evaluated                        | Ready    |
| namespaces | No         | No                 | Skip                                                                      | —        |
| deploy     | No         | Yes (config)       | Evaluate template (config in scope) → hash differs → SSA apply            | NotReady |
| service    | No         | Yes (deploy)       | propagateWhen unsatisfied (rollout in progress) → retain previous state   | Ready    |
| policies   | No         | No                 | Skip                                                                      | —        |
| pol/ns-\*  | No         | No                 | Skip                                                                      | —        |

1 apply. Service gated by propagateWhen. Right branch untouched — no trigger, no propagation.

When the rollout completes, a Deployment status watch fires for `deploy`:

| Node       | Triggered? | Dep re-evaluated? | Action                                                                         | Result |
|------------|------------|--------------------|--------------------------------------------------------------------------------|--------|
| config     | No         | No                 | Skip                                                                           | —      |
| namespaces | No         | No                 | Skip                                                                           | —      |
| deploy     | Yes        | No                 | GET full object → propagateWhen now satisfied → propagation (gate changed) | Ready  |
| service    | No         | Yes (deploy)       | Evaluate template (deploy in scope) → hash matches → skip write           | Ready  |
| policies   | No         | No                 | Skip                                                                           | —      |
| pol/ns-\*  | No         | No                 | Skip                                                                           | —      |

0 applies. deploy re-evaluated because status changed; propagateWhen now satisfied. service
evaluates but template output unchanged — write skipped.

---

**Event 2: Namespace added** — Namespace `d` created.

Watch event fires for node `namespaces`. Changes propagate through the right branch:

| Node                         | Triggered? | Dep re-evaluated?  | Action                                     | Result |
|------------------------------|------------|--------------------|--------------------------------------------|--------|
| config                       | No         | No                 | Skip                                       | —      |
| namespaces                   | Yes        | —                  | GET matching objects → 4 namespaces → propagation hash differs | Ready  |
| deploy                       | No         | No                 | Skip                                       | —      |
| service                      | No         | No                 | Skip                                       | —      |
| policies                     | No         | Yes (namespaces)   | Diff items: new key `d`. a, b, c unchanged | Ready  |
| pol/ns-d                     | —          | —                  | New child → evaluate → SSA apply           | Ready  |
| pol/ns-a, pol/ns-b, pol/ns-c | —          | —                  | Unchanged → skip                           | Ready  |

1 apply. Left branch untouched. Three existing children skipped.

---

**Event 3: Spec change** — `monitoring` added (depends on `deploy`), `service` removed. New revision.

All nodes treated as triggered (revision transition — DAG structure changed). Propagation hashes are
computed but do not gate evaluation — every node passes the skip check because all are triggered:

| Node       | Action                                                                                | Result |
|------------|---------------------------------------------------------------------------------------|--------|
| config     | GET full object → propagation hash unchanged → propagation stops                      | Ready  |
| namespaces | GET matching objects → propagation hash unchanged → propagation stops                 | Ready  |
| deploy     | Evaluate template → template hash unchanged → skip write; propagation hash unchanged  | Ready  |
| monitoring | New node → evaluate template → SSA apply                                              | Ready  |
| policies   | Evaluate collection → item set unchanged → propagation stops                          | Ready  |
| pol/ns-\*  | Item unchanged → skip template evaluation                                             | Ready  |

Active revision's applied set: {deploy, monitoring, pol/ns-a, pol/ns-b, pol/ns-c, pol/ns-d}.
Previous revision's applied set included `service`. Prune: delete service.

1 apply + 1 delete.

## Teardown

When a Graph is deleted, the controller unwinds in reverse topological order through the active
revision's DAG. If the revision was deleted (ownerReference cascade race), the controller regenerates
it from the current spec. Teardown is blocked until ordering is available — never degrade to
unordered deletion.

Owns nodes delete the resource. Contribute nodes release fields via skeleton apply. Watch and
Collection Watch take no action. If resources persist (child Graphs with finalizers, external
finalizers), requeue. Once all managed resources are processed, remove the Graph's finalizer.

### Finalization

When a resource is a prune candidate and another node declares `finalizes` pointing at it, the
deletion is gated on the finalizer resource completing. `finalizes` introduces two behaviors that
do not emerge from the DAG:

- **Creates during prune** — the finalizer resource does not exist during normal operation. It
  materializes when the target becomes a prune candidate.
- **Inverts deletion ordering** — normally, dependents are deleted before dependencies. `finalizes`
  inverts this for the target/finalizer pair: the target is deleted before the finalizer resource.

The sequence within a prune walk:

1. The prune walk encounters the target. The controller creates the finalizer resource — the target
   is still fully operational, no `metadata.deletionTimestamp`. This matters: setting
   `deletionTimestamp` can trigger the target's own controller to start destroying underlying
   infrastructure before the finalizer resource has a chance to act. The finalizer resource's key is
   added to the applied set.
2. The finalizer resource reaches readyWhen. If multiple finalizer nodes target the same
   resource, dependencies among them determine ordering — all must be Ready before proceeding.
3. The controller issues DELETE on the target.
4. The prune walk continues. The finalizer resources are in the applied set but not in the desired
   state — they are prune candidates. The walk picks them up and deletes them in reverse dependency
   order.

Finalization state is fully recoverable from the Graph spec, applied set, and cluster state — no
additional state machine is needed. If the controller crashes at any point, the next reconcile
re-derives position: the applied set identifies which finalizer resources were created, the cluster
reveals whether they exist and satisfy readyWhen, and the spec provides the `finalizes` relationships.
SSA idempotency covers re-creation.

The prune ordering must account for finalizer resource dependencies beyond the target. If a finalizer
resource's template references a ConfigMap, that ConfigMap must not be deleted until finalization
completes — even if the ConfigMap is in a different branch of the normal DAG. Resources referenced by
an in-flight finalizer resource are deferred until finalization completes.

Side effects from completed finalizer resources are not rolled back on partial failure. If one
finalizer reaches Ready but a sibling fails, the completed finalizer's effects persist. Finalization
is not transactional.

If the target resource does not exist in the cluster (creation failed, already deleted externally),
there is nothing to finalize. The controller skips finalization and proceeds with cleanup. The
Graph's status surfaces this: `FinalizerSkipped` with a message naming the resource.

If the target exists but the finalizer resource cannot be created — dependency failure, admission
webhook rejection, quota exhaustion, or invalid rendered template — the target's deletion is blocked.
This is `TeardownBlocked`, not a skip — the target has data that the user intended to finalize.

If the finalizer resource is created but never reaches readyWhen (permanent failure), the target's
deletion is blocked. This is also `TeardownBlocked`. The condition message distinguishes between
creation failure and readyWhen failure. To unblock, update the Graph spec to remove or fix the
finalizer resource. The revision transition prunes the orphaned finalizer resource and deletes the
target without finalization.

## Why Not

**Periodic full-graph resync.** Informer resyncs trigger all nodes simultaneously — correlated,
expensive. `next-sync` labels with per-node jitter amortize resync across reconciles, with drift
correction as a side effect.

**Bespoke object caching for partial evaluation.** Caching full objects per node between reconciles
enables skipping nodes entirely, but introduces coherence obligations — the cache can go stale, and
the controller is responsible for invalidation. Dirty propagation through a full topological walk
achieves the same performance (work proportional to change) without maintaining object state.
Informer stores provide the read path using standard Kubernetes watch machinery.

**BFS-scoped walks from triggers.** Walking only the downstream subgraph of a triggered node avoids
iterating over unrelated branches. But iterating is O(1) per skipped node (a boolean check), so the
savings are negligible. The cost is architectural complexity — maintaining walk scopes, restoring
previous state for out-of-scope nodes, and reasoning about the correctness of partial applied set
merges. A full iteration with O(1) skip is simpler and effectively equivalent.

**Full-object change checks.** `metadata.resourceVersion` changes on every update. Full-object
hashing always differs. Field-path-scoped hashing is the correct mechanism — only changes to the
specific paths that dependents reference trigger propagation.

**forEach as a monolithic node.** Re-evaluating all children when one item changes is wasted work
proportional to the collection size. Parent-with-children evaluates only changed items.

**Incremental applied set updates.** Per-resource writes during wind mean partial failure corrupts
the set. Atomic commit after complete wind is safe.

Also rejected: index-based forEach identity (reordering causes churn), forEach as a subgraph stamper
(sibling blocks and nested Graphs compose better), separate rollforward algorithm (one walk handles
all cases), unordered deletion (stuck finalizers), continuous drift restoration (N-write steady-state
tax).
