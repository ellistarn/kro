# GraphDefinition

A GraphDefinition defines a Kubernetes CRD and implements it with a graph. Each version has its own
schema and nodes. Users create instances of the generated Kind. The system reconciles resources per
instance.

GraphDefinition supports multiple schema versions on a single CRD. Users migrate between versions
by updating their manifests in Git. The version change swaps the node set, which triggers a Graph
Revision — the same mechanism that handles any implementation change. No conversion webhooks. No
annotation-based pinning.

## Object

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: GraphDefinition
metadata:
  name: webapp
spec:
  kind: WebApp
  group: experimental.kro.run
  versions:
    - name: v1alpha1
      schema:
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

One version. `served` defaults to `true`. `storage` defaults to the last version in the list.
With a single version, neither needs to be set.

## Spec

### kind and group

The Kind and API group for the generated CRD. `kind: WebApp` with `group: experimental.kro.run`
produces a CRD at `webapps.experimental.kro.run`. Immutable after creation.

### versions

A list of version entries. Each entry becomes a version on the generated CRD with its own schema
and nodes. Must have at least one entry.

Version schemas are append-only — to change a schema, add a new version. Nodes within a version
can be updated in-place; the system creates a GraphRevision per affected instance to handle the
transition. Removing a version entry is equivalent to setting `served: false` and then deleting it
once no instances remain.

#### name

The CRD version name (e.g., `v1alpha1`, `v1beta1`, `v1`). This is the `apiVersion` users write in
their manifests.

#### served

Whether the API server accepts requests at this version. Defaults to `true`.

Setting `served: false` on a version with existing instances triggers `MigrationRequired` — the
GraphDefinition goes Pending until all instances at that version have been migrated.

#### storage

Whether this version is the storage version. Defaults to `true` for the last version in the list.
Exactly one version is storage at any time.

When the storage version changes, existing objects in etcd remain in the old format until re-written.
Kubernetes handles this — reads return the stored data regardless of which version is currently
storage. Objects migrate to the new format on their next write.

#### schema

The schema for this version. Contains `spec` (user inputs) and `status` (system outputs).

`spec` fields use SimpleSchema notation: `fieldName: type | constraints`. Each version has its own
schema — versions can add fields, change defaults, or adjust constraints independently.

`status` fields are CEL expressions evaluated by the controller and contributed back to the instance.
Each version can expose different status fields. Status expressions reference node IDs from the
same version's nodes (e.g., `${deployment.status.availableReplicas}` references a node with
`id: deployment`).

#### nodes

The resource templates for this version. Same structure as Graph nodes (`id`, `template`,
`readyWhen`, `includeWhen`, `propagateWhen`, `finalizes`, `forEach`).

`schema` is the variable bound to the specific instance being reconciled. Each version's nodes
reference that version's schema fields — `${schema.spec.image}`, `${schema.metadata.name}`, etc.

Versions can have identical nodes (common for additive schema changes) or completely different
resource topologies. When a user migrates between versions, the node set swap is handled by a
Graph Revision.

## Compilation

The GraphDefinition controller validates each version's schema and nodes together. Schema field
references in nodes are type-checked against the version's schema. Status expressions are validated
against the version's node IDs. Each version is compiled independently.

`Compiled=True` means: all versions' schemas are valid, all node references resolve, all status
expressions reference valid nodes.

From a compiled GraphDefinition, the system creates:

1. **A multi-version CRD** — one version entry per `spec.versions` entry. `None` conversion. Each
   version has its own OpenAPI v3 schema.

2. **A controller Graph (L1)** — generated by the GraphDefinition controller. Watches instances of
   the generated Kind, creates a per-instance Graph (L2) via forEach.

3. **Per-instance Graphs (L2)** — each Graph reconciles the nodes from the instance's version. The
   `schema` variable is bound to the instance. When the instance's version changes (user updates
   manifest), the node set changes, triggering a revision.

### Controller Graph Generation

The generated L1 controller Graph dispatches to different node sets based on each instance's
version. The dispatch is a CEL expression generated from the version schemas' field-structure
discriminators:

```
has(item.spec.healthCheckPath) ? v1beta1_nodes : v1alpha1_nodes
```

For N versions, this is an N-way conditional generated at compilation time.

## Version Detection

With `None` conversion, the API server stores all objects at the storage version's `apiVersion`
regardless of which version was used at creation. This is a fundamental property of `None`
conversion: it changes the `apiVersion` field but does not convert fields. The controller cannot
determine which version an instance was created at from `apiVersion` alone.

The alternative — a conversion webhook — would preserve version identity but adds infrastructure
cost (webhook deployment, TLS certs, availability dependency). This design scopes out conversion
webhooks. The trade-off is that version detection relies on field-structure analysis.

The controller detects the version by which fields are present:

- **Additive changes** (v1beta1 adds `healthCheckPath`): CRD defaulting sets `healthCheckPath` on
  v1beta1 instances at admission. v1alpha1 instances don't have it. Present → v1beta1. Absent →
  v1alpha1.

- **Field renames** (`port` → `servicePort`): different field names are unambiguous. Per-version
  CRD schemas enforce that only the version's fields are present at admission.

- **Migration**: `kubectl apply` at the new version triggers schema validation and defaulting. New
  fields appear, old fields are pruned by apply's three-way merge. The controller detects the
  change and swaps node sets.

- **Ambiguous cases** (identical schemas): if two versions have the same spec fields with the same
  defaults, their instances are indistinguishable. Two versions with identical schemas should be
  collapsed into one — the change is implementation-only and belongs in the nodes, not a new
  version. If ambiguity does occur, the controller defaults to the latest version.

## Versions

### Multi-Version CRD

The generated CRD uses `None` conversion — the API server does not convert between versions. Each
version has its own OpenAPI v3 schema. Per-version schemas enforce that instances are always fully
one version's shape — no mixed state.

With `None` conversion, `kubectl get` might show a cosmetically wrong `apiVersion` — the label says
`v1beta1` but the fields are `v1alpha1`. The data is internally consistent. Users interact via
`kubectl apply` from their manifests, not `kubectl get`.

### User Migration

1. User has `apiVersion: v1alpha1` in their manifest in Git
2. Platform engineer adds `v1beta1` to the GraphDefinition
3. CRD now serves both versions — user's v1alpha1 manifest still applies
4. User updates their manifest: `apiVersion: v1beta1`, updates fields
5. `kubectl apply` validates against v1beta1's schema, applies defaults
6. Controller detects field structure change, swaps to v1beta1's nodes
7. Graph Revision transitions resources safely

### Migration as a Revision

Version migration reuses the Graph Revision mechanism. When a user switches from v1alpha1 to
v1beta1:

1. `kubectl apply` at v1beta1 — new fields appear via defaulting, old fields pruned
2. Controller detects the version change and swaps the per-instance Graph's nodes
3. The per-instance Graph creates a new GraphRevision
4. The revision diffs: same `id` → update, new `id` → create, removed `id` → prune
5. Resources transition safely in dependency order
6. Status contribution switches to the new version's status schema — the instance reports
   v1beta1's status fields after migration completes

### Additive Changes

Same field names across versions, new optional fields:

```
v1alpha1: { image, replicas, port }
v1beta1:  { image, replicas, port, healthCheckPath }
```

`None` conversion handles this cleanly. v1alpha1 instances read as v1beta1 show valid fields, just
missing `healthCheckPath`. For additive changes, nodes between versions are often identical or
nearly so.

### Field Renames

Each version's nodes reference that version's fields. v1alpha1 nodes use `${schema.spec.port}`,
v1beta1 nodes use `${schema.spec.servicePort}`. The right nodes are used for the right version.
Per-version CRD schemas prevent mixed state at admission.

### Version Retirement

```yaml
versions:
  - name: v1alpha1
    served: false
  - name: v1beta1
    schema: [...]
    nodes: [...]
```

Controller checks: instances with v1alpha1's field structure? If yes → `MigrationRequired`. If
no → CRD updated to stop serving v1alpha1.

### Type Changes

Not supported in-place. A field type change requires a new GraphDefinition (new CRD).

## Versioning Lifecycle

### Initial Version

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: GraphDefinition
metadata:
  name: webapp
spec:
  kind: WebApp
  group: experimental.kro.run
  versions:
    - name: v1alpha1
      schema:
        spec:
          image: string | default=nginx
          replicas: integer | default=1
          port: integer | default=80
        status:
          deploymentReady: ${deployment.status.availableReplicas == deployment.spec.replicas}
      nodes:
        - id: deployment
          template: ...
        - id: service
          template: ...
```

One version. No `served` or `storage` needed — defaults handle it. Controller creates a CRD and
starts reconciling instances.

### Add a Version

```yaml
versions:
  - name: v1alpha1
    schema:
      spec: { image: ..., replicas: ..., port: ... }
      status: { deploymentReady: ... }
    nodes: [...]

  - name: v1beta1
    schema:
      spec: { image: ..., replicas: ..., port: ..., healthCheckPath: ... }
      status: { deploymentReady: ..., endpoint: ... }
    nodes:
      - id: deployment
        template: ...    # enhanced — livenessProbe using healthCheckPath
      - id: service
        template: ...
```

v1beta1 is last → it's storage. Both versions served. Existing v1alpha1 instances continue with
v1alpha1's nodes. Users migrate at their own pace.

### Implementation-Only Change

Platform engineer updates v1beta1's Deployment template (adds resource limits). No schema change.

Controller updates per-instance Graphs for v1beta1 instances with the new nodes. Each creates a
GraphRevision. Resources transition in dependency order. v1alpha1 instances unaffected.

### Version Retirement

Platform engineer sets `served: false` on v1alpha1. Controller checks for remaining v1alpha1
instances → `MigrationRequired` or proceeds to update the CRD.

## Status

**`Compiled`** — spec is valid.

| Reason             | Meaning                                        |
| ------------------ | ---------------------------------------------- |
| `Compiled`         | All versions' schemas and nodes validate       |
| `ExpressionError`  | CEL expression invalid                         |
| `SchemaError`      | Schema definition malformed                    |
| `BindingError`     | Node references schema field that doesn't exist|

**`Ready`** — desired state is enacted.

| Reason              | Status    | Meaning                                         |
| ------------------- | --------- | ----------------------------------------------- |
| `Ready`             | `True`    | CRD matches spec, all instance Graphs converged |
| `MigrationRequired` | `Unknown` | Retiring a version with existing instances       |
| `Converging`        | `Unknown` | CRD updated, instance Graphs transitioning       |
| `Error`             | `False`   | CRD apply failed or instance Graph errors        |

## Relationship to ResourceGraphDefinition

GraphDefinition is the experimental successor to ResourceGraphDefinition (RGD).

| Concern            | RGD                                  | GraphDefinition                                |
| ------------------ | ------------------------------------ | ---------------------------------------------- |
| Schema versioning  | Single version, flat `schema` field  | Multi-version, `versions` list                 |
| Implementation     | `resources`                          | `nodes` (same structure)                       |
| Scope              | Cluster-scoped                       | Namespace-scoped                               |
| CRD conversion     | N/A (single version)                 | `None` strategy, per-version schemas           |
| Migration          | Implicit (all instances update)      | Explicit (users update manifests → revision)   |

A single-version GraphDefinition degenerates to an RGD: one entry in `versions` with its own
`schema` and `nodes`. The migration from RGD to GraphDefinition is a Kind change — different Kinds,
not different versions of the same Kind.

## Scoped Out

**Conversion webhooks.** Users provide correct shapes via manifests. `None` conversion. Version
detection by field structure. No webhook infrastructure.

**Automatic instance migration.** Users update manifests (or Crow automates). The system detects
and reports migration state but does not move instances between versions.

**GraphDefinition revisions.** Not needed. Per-instance Graph revisions track implementation state.
The CRD is the schema state record. All state is discoverable.

**Type changes within a version.** Requires a new GraphDefinition (new CRD).

**Rollout strategies.** Graph Revisions handle safe resource transitions. Propagation control
(KREP-006) adds gates. These are Graph-level concerns.

## Rejected Alternatives

**Conversion webhooks for version identity.** A conversion webhook would let the API server track
which version an instance was created at and convert fields on read/write. This preserves version
identity without field-structure analysis. Rejected because it adds infrastructure cost (webhook
deployment, TLS certs, availability dependency) for a problem that field-structure detection solves
without additional infrastructure.

**Separate Graph objects per version.** Each version references an external Graph object by name
(`graphRef`). Schema and implementation are in separate Kubernetes objects with separate lifecycles.
Rejected because it adds object management overhead and indirection without meaningful benefit —
the platform engineer already edits the GraphDefinition, and inline nodes compile in a single phase
without requiring external variable support in the Graph compilation model.

**Explicit `schemaVersion` field for version detection.** A `schemaVersion` field injected into each
version's CRD schema via defaulting would let the controller identify which version an instance was
created at. Rejected because CRD defaults are applied at admission on CREATE but do not override
existing values on UPDATE — when a user migrates by changing `apiVersion` and applying, the existing
`schemaVersion` value persists. Field-structure detection works without this limitation.

**Shared nodes across versions.** A single `nodes` block shared by all versions, with the controller
normalizing each instance to the storage version's schema before evaluating nodes. Rejected because
it conflates interface and implementation — versions with different schemas produce different
resources, so the node set should vary with the version. It also requires controller-side schema
normalization, which adds complexity.
