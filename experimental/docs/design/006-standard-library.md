# Standard Library

The standard library is sugar that ships with kro. Each stdlib type is a Graph that enables a
higher-order pattern. The stdlib is self-hosting: each type is implemented by the types below it.

```
Core:    Graph     — the runtime primitive (Go controller)
Stdlib:  Kind      — define a new Kubernetes Kind (CRD + per-instance Graphs)
         Decorator — watch instances of a Kind, create a sub-Graph per instance
         Singleton — declare a resource that should exist exactly once
```

## Kind

A Kind defines a new Kubernetes Kind and implements it. It declares a schema and nodes (the
resources to create per instance). Schemas use kro's simpleSchema syntax —
`fieldName: type | default=value` — that compiles to OpenAPI. A Kind with empty
`nodes: []` creates the CRD but produces no per-instance resources.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Kind
metadata:
  name: webapp
spec:
  schema:
    apiVersion: experimental.kro.run/v1alpha1
    kind: WebApp
    spec:
      image: string | default=nginx
      replicas: integer | default=1
      port: integer | default=80
    status:
      deploymentReady: ${deployment.status.availableReplicas == deployment.spec.replicas}
  nodes:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.metadata.name}
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: ${schema.metadata.name}
          template:
            metadata:
              labels:
                app: ${schema.metadata.name}
            spec:
              containers:
                - name: app
                  image: ${schema.spec.image}
    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.metadata.name}-svc
        spec:
          selector:
            app: ${schema.metadata.name}
          ports:
            - port: ${schema.spec.port}
```

`schema` is the per-instance scope variable — the resource itself. `${schema.spec.image}` resolves
to the instance's `.spec.image` field. Status expressions reference other nodes and are contributed
back to the instance.

**Status writeback mechanism.** The Kind controller synthesizes the per-instance Graph's node list as
`[schema ref] + user nodes + [status patch]`. The `schema` ref node GETs the instance into scope.
The `status` patch node targets the instance's status subresource with the evaluated CEL expressions
from `spec.schema.status` (which defaults to `{}`). The DAG naturally schedules the status patch
last — its expressions reference node IDs from the user's nodes, creating implicit dependencies. The
existing SSA split-apply logic (see [Ownership](003-ownership.md)) handles the separate `/status`
endpoint.

**Status ownership: only expression fields are written.** The status patch writes only the
`spec.schema.status` fields whose value is a CEL expression. The classifier is precise: a status
field is treated as a kro-owned expression **iff its value contains the substring `${`**. Bare-type
fields (e.g. `ready: boolean`) contain no `${`, so they contribute to the generated CRD status schema
but are **excluded** from the status write — kro claims no SSA ownership of them. (Edge case of the
substring rule: a string field whose literal default contains `${`, e.g. `string | default=${x}`,
is classified as an expression. This same predicate is used on both the write side and the schema
side; see the contract note below.)

This lets a Kind serve two roles:

- **Reconciling Kind** — status fields are CEL expressions; kro evaluates and owns them.
- **Schema-authoring Kind** — status fields are bare types (or status is empty); kro generates a
  typed, validated status schema but writes nothing, leaving the status for an external controller to
  fill via SSA without 409 contention.

Because kro never writes the bare-type fields, an external SSA writer is their sole field-manager —
no conflict. The boundary is the `${`: converting a bare-type field to an expression makes kro a
co-owner of that field. When no expression fields remain, the patch omits the `status` key entirely
(writing only the GC finalizer), so kro registers no status field-manager at all.

**Classifier contract and mixed-status limitation.** The `${`-substring predicate is implemented in
two places that MUST agree: the write-side filter in the stdlib `kindInstancePatch` CEL
(`charts/stdlib/templates/kind.yaml`) and the schema generator's expression check
(`controller/compiler/celfuncs.go`). `TestStatusFieldClassifier_PredicateParity` guards against drift.
One limitation today: the schema generator emits a typed status schema only when **no** status field
is an expression. So a Kind that *mixes* expression and bare-type status fields gets correct
write-side ownership (only expression fields written) but no OpenAPI typing on its bare-type fields
(the whole status block falls back to `x-kubernetes-preserve-unknown-fields`). Per-field typing in
the mixed case is a follow-up; `TestStdlibKindMixedStatus_Regression` pins the current behavior and
must be updated when the generator learns to partition fields.



A Kind may declare `readyWhen` and `propagateWhen` at the spec level. Both are per-instance — they
evaluate in the same scope as the Kind's nodes.

- **`readyWhen`** defines when each instance is considered healthy. Produces `.ready()` per-instance.
  The Kind resource's own status conditions (Compiled, Ready) are hoisted from its controller Graph —
  they reflect whether the Kind's structural work is healthy (CRD established, watching instances),
  decoupled from individual instance convergence.
- **`propagateWhen`** controls rollout pace across instances. It is a per-instance input gate with
  sibling visibility — the expression references the instances collection and uses `.ready()` and
  `.updated()` per-item. Semantics are identical to
  [forEach propagateWhen](005-reconciliation.md#propagation-control).

Both are optional. Without them, all instances receive updates simultaneously and readiness rolls up
from node states.

## Decorator

A Decorator watches instances of an existing Kind and creates a sub-Graph per instance. It does not
define a new Kind — it watches an existing one and creates resources per instance. Collection watch
only — for a single-resource dereference, use a plain Graph with a `ref:` node.

Each sub-Graph gets an auto-prepended `item` ref node that fetches the specific instance, plus
the user's nodes. User nodes reference `${item.metadata.name}`, `${item.spec.foo}`, etc.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Decorator
metadata:
  name: namespace-policies
spec:
  watch:
    apiVersion: v1
    kind: Namespace
    selector: {}
  nodes:
    - id: policy
      template:
        apiVersion: networking.k8s.io/v1
        kind: NetworkPolicy
        metadata:
          name: default-deny
          namespace: ${item.metadata.name}
        spec:
          podSelector: {}
          policyTypes:
            - Ingress
```

Deletion is not ordered — resources are garbage-collected through normal Kubernetes ownership.

## Singleton

A Singleton declares a single resource that should exist exactly once. When multiple Singletons
target the same resource (same GVK + namespace + name in their template), priority resolution
determines the claim holder. Same-priority Singletons targeting the same resource must have
identical templates — divergent templates at the same priority is a user error.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Singleton
metadata:
  name: cluster-admin-binding
spec:
  priority: 100
  template:
    apiVersion: rbac.authorization.k8s.io/v1
    kind: ClusterRoleBinding
    metadata:
      name: platform-admin
    subjects:
      - kind: Group
        name: platform-admins
        apiGroup: rbac.authorization.k8s.io
    roleRef:
      kind: ClusterRole
      name: cluster-admin
      apiGroup: rbac.authorization.k8s.io
```

One Singleton, one template, one resource. If you need a group of resources to be a singleton, the
template is a Graph.

### Priority Resolution

A Singleton wins when no other Singleton targeting the same resource has higher priority. Ties at
the same priority are broken by earliest `metadata.creationTimestamp` (first come, first served).
Resource identity is the template's apiVersion + kind + namespace + name. Cluster-scoped resources
use empty string for the namespace component.

### Architecture — Fan-In Graph

The Singleton is implemented as a single long-lived Graph (not a Kind). This Graph:

1. Creates the Singleton CRD
2. Watches all Singleton CRs
3. Computes claims (one per Singleton) and winners (one per unique target identity)
4. Applies target resources via forEach template with `lifecycle.apply: Force`
5. Patches status back to each Singleton CR

The target resource is owned by this singleton controller Graph — not by any per-instance
sub-Graph. When a Singleton CR is deleted, the Graph re-evaluates: if other claimants remain for
that identity, the target persists with the new winner's template (force-applied in place via SSA).
Only when ALL Singletons for an identity are gone does the forEach shrink and the target get pruned.

This eliminates the delete/recreate race that would exist if per-instance Graphs owned the target:
the resource UID is preserved across holder transitions (zero-downtime failover).

## ResourceGraphDefinition

The stdlib includes a backwards-compatibility shim that implements the upstream kro
ResourceGraphDefinition API. RGDs are translated into the same infrastructure as Kind — CRD
creation, instance watching, per-instance Graphs — but accept the upstream `spec.schema` +
`spec.resources` shape instead of Kind's `spec.schema` + `spec.nodes`.

### Validation

An RGD is `Active` or `Inactive`. `Inactive` means the system detected a problem and will not
create instances. The `Ready` condition carries the reason.

**Resource IDs** must be lower camelCase (`^[a-z][a-zA-Z0-9]*$`), unique, and not reserved words
(`spec`, `status`, `metadata`, `instance`, `self`, `kro`, etc.).

**Template resources** must be valid Kubernetes objects: `apiVersion`, `kind`, and `metadata` are
required. Literal `apiVersion` values must follow Kubernetes conventions (`v1`, `v1alpha1`,
`apps/v1`). CEL expressions in these fields are evaluated at instance time.

**Status fields** must be CEL expressions (`${...}`). Plain values are rejected.

**ForEach iterators** must not collide with reserved words or resource IDs.

**Expressions** must be valid CEL, reference existing nodes, and produce values assignable to the
destination field's type. Cycles in the dependency graph are rejected.

## Constraints

**Namespace scoping.** Collection watches (`watch:` nodes) follow k8s list/watch semantics: absent
`metadata.namespace` means all namespaces. Resources (`template:`, `patch:`, `ref:`) default to the
Graph's own namespace when `metadata.namespace` is absent.

**Deletion lifecycle.** Each per-instance Graph has an ownerReference pointing to the instance and
a `patch:` node that places a finalizer on the instance. When the instance is deleted, the finalizer
holds it in Terminating; the controller detects the owner's `deletionTimestamp` and self-deletes the
Graph; teardown prunes the patch node, releasing the finalizer.
