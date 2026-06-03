# Graph

A Graph is kro's primitive for composing Kubernetes resources. Graphs define a scope of nodes that
can reference other nodes using [CEL expressions](https://cel.dev/). The graph manages the lifecycle
of its nodes, including the dependencies between them. Create a Graph and its nodes are created in
their topological order. Mutate it, and the graph converges on the newly desired state. Delete it
and nodes are pruned in the reverse order.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: my-app
spec:
  nodes:
    - id: deployment
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
                  image: nginx
    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${deployment.metadata.name}-svc
        spec:
          selector: ${deployment.spec.selector.matchLabels}
          ports:
            - port: 80
```

## Spec

### Nodes

`spec.nodes` is a list of nodes. The order of the nodes does not matter — evaluation order is
determined by the dependencies between nodes.

#### id

A string that identifies the node within the Graph's scope. Other nodes can reference this id in CEL
expressions. Must be an alphanumeric string (case-insensitive) and unique within the Graph's scope.

#### type

A node's type is the keyword it declares. Six types exist:

- **`template:`** — Template a Kubernetes resource. The controller creates the resource if it
  doesn't exist, applies changes when the template changes, and deletes the resource on prune.
- **`patch:`** — Patch fields on an existing resource. Each field has exactly one writer —
  ownership conflicts are detected and surfaced before the write proceeds. A patch may apply fields
  to a resource in another graph. On prune, the fields are released from the resource.
- **`ref:`** — Reference a resource outside of this graph and make its fields available to other
  nodes in this graph.
- **`watch:`** — Watch all resources of a GroupKind matching a label selector and make their fields
  available to other nodes in this graph.
- **`def:`** — Define raw data for use by other nodes in this graph. Def nodes do not read or write
  Kubernetes resources.
- **`metric:`** — Emit a prometheus gauge driven by CEL. Value is an explicit CEL expression that
  evaluates to a number. Labels are direct CEL expressions evaluated in scope. Metric names are
  unique across Graphs. Propagation-driven — re-evaluates when upstream dependencies change. Does
  not publish to scope.

```yaml
# def: — reusable naming values, no Kubernetes resource created
- id: naming
  def:
    prefix: ${spec.name + '-' + spec.env}

# template: — creates and manages a Deployment using the definition
- id: deploy
  template:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: ${naming.prefix + '-deploy'}
    spec:
      replicas: 3
      selector:
        matchLabels:
          app: ${naming.prefix}
      template:
        metadata:
          labels:
            app: ${naming.prefix}
        spec:
          containers:
            - name: app
              image: nginx

# ref: — reads an existing WebApp into scope
- id: webapp
  ref:
    apiVersion: kro.run/v1alpha1
    kind: WebApp
    metadata:
      name: my-app

# patch: — writes status fields to the WebApp
- id: webappStatus
  patch:
    apiVersion: kro.run/v1alpha1
    kind: WebApp
    metadata:
      name: ${webapp.metadata.name}
      namespace: ${webapp.metadata.namespace}
    status:
      deploymentReady: ${deploy.status.availableReplicas == deploy.spec.replicas}

# watch: — discovers all Pods matching a selector
- id: appPods
  watch:
    apiVersion: v1
    kind: Pod
    selector:
      app: ${naming.prefix}

# metric: — emits a prometheus gauge with an explicit value
- id: podCount
  metric:
    type: gauge
    name: pod_count
    help: Total number of pods matching the app selector.
    value: ${size(appPods)}

# metric: — with forEach, emits a gauge per dimension
- id: podsByPhase
  forEach:
    phase: ${appPods.map(p, p.status.phase).distinct()}
  metric:
    type: gauge
    name: pods_by_phase
    labels:
      phase: ${phase}
    value: ${size(appPods.filter(p, p.status.phase == phase))}
```

## Dependencies

Dependencies between nodes are defined by CEL expressions. If node B's template contains
`${A.metadata.name}`, B has a dependency on A. Each CEL expression creates an edge in the graph. A
node cannot be evaluated until all of its edges can be evaluated.

### Soft Dependencies

Nodes can have soft dependencies on other nodes by defining CEL expressions with
[Optional Types](https://pkg.go.dev/github.com/google/cel-go/cel#OptionalTypes) (`?`, `.orValue()`).
A node's expression uses the data of another node if its available, but defines alternative values
if it's not.

```yaml
- id: deployment
  template: ...

- id: appStatus
  patch:
    apiVersion: kro.run/v1alpha1
    kind: WebApp
    metadata:
      name: my-app
    status:
      replicas: ${deployment.?status.?availableReplicas.orValue(0)}
      endpoint: ${service.?status.?loadBalancer.?ingress[0].?hostname.orValue('pending')}
```

`appStatus` evaluates immediately. While `deployment` is absent, `.?availableReplicas.orValue(0)`
returns `0`. While `service` has no load balancer, `.?hostname.orValue('pending')` returns
`'pending'`. When both resolve, the patch reflects the live values.

### CEL Functions

Nodes expose functions that enable other nodes to reason about their state.

- **`.ready()`** — true when the node is applied and its `readyWhen` conditions pass.
- **`.updated()`** — true when the node has been evaluated against the latest graph generation.
- **`.dependencies()`** — a list of hard and soft dependencies of the node. Useful to chain
  dependency readiness `readyWhen: [${node.dependencies().all(d, d.ready())}]`.
- **`time.now()`** — the current wall clock as a CEL-native `timestamp`. When it appears in a
  comparison, kro solves for the moment the comparison becomes true and enqueues reconciliation.
- **`.condition(type, status, reason, message)`** — sugar for constructing a Kubernetes status
  condition from observed state. Finds existing condition by type, preserves `lastTransitionTime`
  when status is unchanged, stamps `time.now()` on transition, sets `observedGeneration` from
  `.metadata.generation`. See §Observed State for the underlying pattern.
- **`plural(s)`** — English pluralization. Returns the plural form of the input string. Example:
  `plural("WebApp").lowerAscii()` → `"webapps"`.
- **`simpleSchema.toOpenAPI(schema, resources)`** — Converts a SimpleSchema map and resource list
  into a fully-structured OpenAPI v3 schema object.

## Modifiers

Modifiers modify the behavior of a node. They use CEL expressions that can reference other nodes.

### ForEach

The forEach modifier is a key-value pair that repeats a node for each value in a list CEL
expression. These child nodes are a set of logical nodes that depend on the parent. The forEach key
can be referenced in the node's CEL expressions. Additional node modifiers apply to the child nodes,
not the parent. The parent's state is a rollup of its children. Its `.ready()` is true when all children are ready, and `.updated()` is true when all children are updated.
Each child's identity is derived from the parent's ID and the child's GVK, Namespace, and
Name. Other nodes in the graph see the forEach node as a list of children when referenced in CEL.

```yaml
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

# forEach + def: computed container list embedded in a Deployment
- id: containers
  forEach:
    w: ${spec.workers}
  def:
    name: ${w}
    image: ${spec.appImage}
    args: ["--worker=${w}"]

- id: deployment
  template:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: ${spec.name}
    spec:
      template:
        spec:
          containers: ${containers}
```

### includeWhen

The `includeWhen` modifier is a list of boolean CEL expressions. When all are true, the node is
included in the graph, else it is skipped and its resource becomes a prune candidate. Nodes that
depend on excluded nodes are also excluded.

```yaml
- id: ingress
  includeWhen:
    - ${config.data.enableIngress == "true"}
  template: ...
```

### readyWhen

The `readyWhen` modifier is a list of boolean CEL expressions. When all are true, the node is ready.
If readyWhen is not defined, the node is ready as soon as it is evaluated. readyWhen is a health
signal — it does not gate dependents. Dependents proceed regardless of readyWhen. Use propagateWhen
to gate dependents on readiness.

Each node exposes its readiness through a `.ready()` CEL function as a convenience for other nodes.

```yaml
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas > 0}
  template: ...
```

#### Graph Readiness

The Graph is Ready when all of its nodes are ready. This is the mechanism by which external tools —
Helm, ArgoCD, `kubectl wait` — know when a Graph has converged. Tools block on the Graph's `Ready`
condition. Authors control what "converged" means by choosing which nodes carry `readyWhen`.

A Graph deploying a Deployment should declare `readyWhen` on the Deployment so Helm knows when the
rollout is complete. A Graph managing an unbounded collection (a controller watching instances) should
not put `readyWhen` on the forEach — the collection is never "done," and new items can appear at any
time. The Graph's readiness reflects its own convergence, not the convergence of things it manages.

Put `readyWhen` on resources whose convergence defines "done" for the Graph's purpose:

```yaml
# Graph is Ready once the Deployment has available replicas.
# Helm will wait for this before proceeding with the next chart.
spec:
  nodes:
    - id: deployment
      readyWhen:
        - ${deployment.status.availableReplicas > 0}
      template:
        apiVersion: apps/v1
        kind: Deployment
        ...
    - id: service
      template:
        apiVersion: v1
        kind: Service
        ...
```

The Service has no `readyWhen` — its existence is sufficient. The Deployment's `readyWhen` is what
blocks the Graph's `Ready` condition. The author made a choice: "done" means "pods are serving."
If a node's convergence should not block the Graph's readiness, omit readyWhen.

### propagateWhen

The `propagateWhen` modifier is a list of boolean CEL expressions. When unsatisfied, the node and
its dependents are not re-evaluated. When satisfied, the node evaluates normally.

It's common to leverage readyWhen and propagateWhen to control evaluation between nodes.

```yaml
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas > 0}
  template: ...

- id: service
  propagateWhen:
    - ${deployment.ready()}
  template:
    apiVersion: v1
    kind: Service
    metadata:
      name: ${deployment.metadata.name}-svc
    spec:
      selector: ${deployment.spec.selector.matchLabels}
```

It's common to combine forEach with propagateWhen to control how quickly changes happen in parallel
within a graph. Each child's propagateWhen counts it siblings to determine whether or not it should
propagate. The forEach evaluates serially and retains the stable order of inputs to avoid races.

```yaml
# Exponential rollout — budget doubles each wave
- id: deployments
  forEach:
    app: ${apps}
  propagateWhen:
    - >-
      ${deployments.filter(d, d.updated() && !d.ready()).size()
       < max(1, deployments.filter(d, d.updated() && d.ready()).size())}
  template: ...
```

```yaml
# Linear rollout — 2 at a time
- id: deployments
  forEach:
    app: ${apps}
  propagateWhen:
    - ${deployments.filter(d, d.updated() && !d.ready()).size() < 2}
  template: ...
```

### finalizes

The finalizes modifier causes a node to enter the graph when its target node becomes a prune
candidate, and does not exist otherwise. The target cannot be pruned until its finalizer is
evaluated. Finalizers trigger regardless of why the target is being pruned -- graph deletion, graph
mutation, includeWhen, or forEach.

It's common to combine `finalizes` with `readyWhen` to coordinate graceful removal of resources.

```yaml
- id: snapshot
  finalizes: pvc
  template:
    apiVersion: snapshot.storage.k8s.io/v1
    kind: VolumeSnapshot
    metadata:
      name: ${pvc.metadata.name}-final
    spec:
      source:
        persistentVolumeClaimName: ${pvc.metadata.name}
  readyWhen:
    - ${snapshot.status.readyToUse == true}
```

## Observed State

Before evaluating a node's expressions, kro GETs the target resource from the API server. The
result — the full live object including `metadata` and `status` — enters scope under the node's
`id`. This is the observed state: what exists before the node acts. When the resource does not yet
exist (first create), the scope entry is an empty map — expressions use optional chaining (`.?`,
`.orValue()`) to provide defaults. After apply, the response replaces the scope entry — downstream
nodes see the post-apply state, not the pre-apply observation.

Self-reference follows naturally: a node can reference its own `id` in its expressions to read its
observed state. `deployment.metadata.generation` in the `deployment` node's template reads the live
generation from the GET. `deployment.status.conditions` reads the existing conditions. This lets a
node compare observed with desired and compute transitions.

### Status Conditions

Status conditions are a pattern built on observed state. A node reads its target's existing
conditions and generation, computes new conditions, and writes them back. The `observedGeneration`
field reports the generation of the object being written to — confirming the controller has processed
that generation.

**Single template — self-reference:**

```yaml
- id: myapp
  template:
    apiVersion: example.com/v1
    kind: MyApp
    metadata:
      name: my-app
    spec:
      image: nginx
    status:
      conditions: ${[
        {
          "type": "Ready",
          "status": myapp.?status.?availableReplicas.orValue(0) > 0 ? "True" : "False",
          "observedGeneration": myapp.?metadata.?generation.orValue(0),
          "lastTransitionTime": myapp.?status.?conditions.orValue([]).exists(c, c.type == "Ready")
            && myapp.status.conditions.filter(c, c.type == "Ready")[0].status
               == (myapp.?status.?availableReplicas.orValue(0) > 0 ? "True" : "False")
            ? myapp.status.conditions.filter(c, c.type == "Ready")[0].lastTransitionTime
            : time.now()
        }
      ]}
```

On first create (GET 404), `myapp` is an empty map. Optional chaining resolves: `availableReplicas`
defaults to `0`, `generation` to `0`, `conditions` to `[]`. The condition gets `time.now()` as its
initial timestamp. On subsequent reconciles, the GET succeeds and self-reference reads the live
object. A non-404 GET failure (5xx, network timeout) is a transient error — the node becomes
SystemError and retries with backoff. The controller does not evaluate the template when observed
state is unknown.

**Decorator — ref + patch:**

```yaml
- id: webapp
  ref:
    apiVersion: example.com/v1
    kind: WebApp
    metadata:
      name: my-app

- id: deployment
  template:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: ${webapp.metadata.name}
    spec:
      replicas: 3
      # ...

- id: webappStatus
  patch:
    apiVersion: example.com/v1
    kind: WebApp
    metadata:
      name: ${webapp.metadata.name}
      namespace: ${webapp.metadata.namespace}
    status:
      conditions: ${[
        {
          "type": "Ready",
          "status": deployment.ready() ? "True" : "False",
          "observedGeneration": webapp.metadata.generation,
          "lastTransitionTime": webapp.status.conditions.exists(c, c.type == "Ready")
            && webapp.status.conditions.filter(c, c.type == "Ready")[0].status
               == (deployment.ready() ? "True" : "False")
            ? webapp.status.conditions.filter(c, c.type == "Ready")[0].lastTransitionTime
            : time.now()
        }
      ]}
```

The ref provides observed state of the target resource. The patch reads generation and conditions
through `webapp` — the ref's scope entry — rather than through self-reference (`webappStatus`),
because the ref already GETs the same resource. Both paths yield identical data; the ref decouples
the read (observation) from the write (patch), making the dependency graph explicit.

**`.condition()` sugar:** `.condition(type, status, reason, message)` is a convenience function that
encapsulates the pattern above — find existing condition by type, preserve `lastTransitionTime` when
status is unchanged, stamp `time.now()` on transition, set `observedGeneration` from
`.metadata.generation`. Handles first-create (empty map) internally. Called on any scope entry:

```yaml
status:
  conditions: ${[
    myapp.condition('Ready',
      myapp.?status.?availableReplicas.orValue(0) > 0 ? 'True' : 'False',
      'Available', 'Replicas available')
  ]}
```

### Reading Conditions

The same primitive enables reading conditions for downstream decisions. Filter by
`observedGeneration` to ensure you only use conditions that reflect the current spec:

```yaml
- id: production
  propagateWhen:
    - >-
      ${staging.status.conditions.exists(c,
        c.type == 'Available'
        && c.observedGeneration == staging.metadata.generation
        && c.status == 'True')}
```

This gates `production` until the staging controller has reconciled the current generation and
reports Available. Stale conditions from a previous generation don't match.

## Time

`time.now()` returns the current wall clock as a CEL-native `timestamp`. All duration arithmetic
uses standard CEL operators (`timestamp - timestamp → duration`, `timestamp + duration →
timestamp`). When `time.now()` appears in a comparison, kro solves for the moment that comparison
becomes true and enqueues reconciliation for exactly that time. The comparison operator is what gives
kro something to solve — it works wherever the comparison appears: gate expressions, ternaries,
value expressions.

Adding a duration constraint to the generation filter from §Reading Conditions:

```yaml
# Wait 2 hours after staging reports Available for the current generation.
- id: production
  propagateWhen:
    - >-
      ${staging.status.conditions.exists(c,
        c.type == 'Available'
        && c.observedGeneration == staging.metadata.generation
        && c.status == 'True'
        && time.now() - timestamp(c.lastTransitionTime) >= duration('2h'))}
  template:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: my-app
      namespace: us-east-1
    spec:
      replicas: 10
      # ...
```

Kro solves `time.now() - lastTransitionTime >= 2h` → enqueue at `lastTransitionTime + 2h`. The
timestamp is read from the observed state — the staging controller already set it. Two hours after
staging becomes Available for the current generation, one reconciliation fires and `production`
proceeds.

Without a comparison, `time.now()` is a raw value — nothing to solve, no enqueue. Writing
`time.now()` directly onto a resource produces a new value on every reconciliation. The condition
patterns above settle naturally because `lastTransitionTime` is preserved when status is unchanged.
Raw `time.now()` in a write path without a settling mechanism is the user's responsibility to manage.

### Why Not

**`tick(d)` as an explicit scheduling function.** Exposes scheduling mechanics to the user —
polling intervals to tune, side effects to reason about. A comparison involving `time.now()` gives
kro enough information to solve for the exact enqueue time.

**Automatic condition management by the runtime.** Users should decide what conditions exist, what
they mean, and when they transition. `time.now()` provides the clock, observed state provides
existing timestamps, `patch:` or `template:` writes the result — no implicit conditions created by
the system.

**A separate `observed` variable.** Unnecessary — the scope entry *is* the observed state. The GET
that precedes evaluation populates it. Adding a parallel variable duplicates data under a different
name.

## Nested Graphs

A node can define a Graph as its template to create a nested graph. The nested graph is a Kubernetes
object applied like any other template node. This relationship causes the nested graph to evaluate
independently from the parent graph. Like any other node, the parent can define fields of a nested
graph via CEL expressions, which are templated when the object is applied to the Kubernetes API.
However, the child graph executes independently from the parent -- the child's nodes cannot
reference the parent's, and vice versa.

When a graph is evaluated, each CEL expression `${...}` is evaluated into a concrete value. However,
child graphs need to define their own expressions separate from the parent graph's scope. When a
`${...}` expression's body contains inner `${...}` patterns, the outer `${}` wrapper is a deferral
boundary -- the current scope strips the outer `${}`, writing the body (including its inner `${...}`
expressions) as a literal string to the Kubernetes API in the child Graph's spec. The child graph
evaluates those inner expressions independently at its own runtime. A `${...}` whose body contains
no inner `${}` is evaluated immediately as CEL in the current scope.

It's common to combine nested graphs and expression nesting with `watch`, `forEach`, `ref`, to create a
nested scope that evaluates in isolation. Below, the parent graph intentionally does not directly
reference parent's forEach `ns`, except by name, as any change to `ns` would cause the nested graph
to be mutated. Instead, the nested graph is configured to directly reference the `ns` itself within
its own scope.

```yaml
# Parent Graph — watches all Namespaces, creates a child Graph per Namespace
- id: namespaces
  watch:
    apiVersion: v1
    kind: Namespace
    selector: {}

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
          ref:
            apiVersion: v1
            kind: Namespace
            metadata:
              name: ${ns.metadata.name}
        - id: policy
          template:
            apiVersion: networking.k8s.io/v1
            kind: NetworkPolicy
            metadata:
              name: default-deny
              namespace: ${${nsRef.metadata.name}}
            spec:
              podSelector: {}
              policyTypes:
                - Ingress
```

## Status

### Conditions

Two conditions on orthogonal axes. Standard Kubernetes condition fields; `lastTransitionTime`
preserved when status unchanged; `observedGeneration` advances independently per condition.

**`Compiled`** — is the spec valid? Set once when the spec is processed, permanent until the spec
changes. Message: `"<N> nodes"` on success; verbatim compiler error on failure.

| Reason             | Meaning                    |
| ------------------ | -------------------------- |
| `Compiled`         | Spec is valid              |
| `ExpressionError`  | CEL expression is invalid  |
| `DependencyError`  | Circular dependency        |
| `DeclarationError` | Malformed node declaration |

**`Ready`** — has the graph converged? `True` means all nodes satisfied. `Unknown` means the
controller is still making progress. `False` means something requires human intervention. Authors
control what gates this condition by choosing which nodes carry `readyWhen` (see § readyWhen >
Graph Readiness).

The message is a summary line of non-zero state counts followed by up to 10 indented detail lines
for every non-ready node, sorted by node ID. Nodes with error reasons use the format
`<nodeID> (<state>): <reason>`; converging nodes (not ready, pending, blocked) use
`<nodeID> (<state>)`. Messages are deterministic for the same underlying state, avoiding spurious
status writes.

| Reason        | Status    | Meaning                                |
| ------------- | --------- | -------------------------------------- |
| `Ready`       | `True`    | All resources applied and ready         |
| `NotReady`    | `Unknown` | readyWhen not met                      |
| `Pending`     | `Unknown` | Waiting for upstream data              |
| `Blocked`     | `Unknown` | Dependency in error state              |
| `NotCompiled` | `False`   | Rollup of Compiled=False               |
| `Conflict`    | `False`   | SSA field ownership contested          |
| `Error`       | `False`   | Client request failed (4xx)            |
| `SystemError` | `False`   | Server or infrastructure failure (5xx) |

```yaml
message: "47 ready"
message: |-
  43 ready, 4 pending
    deploy (pending)
    ingress (pending)
    secret (pending)
    svc (pending)
message: |-
  1 ready, 1 blocked, 1 error, 1 system error
    authService (error): Forbidden
    downstream (blocked)
    paymentDb (system error): ServerError
```

Alarm on `False` or `Unknown` persisting beyond a reasonable convergence window. Compiled rolls into
Ready as `NotCompiled`, so a single Ready alarm covers both failure domains.

### Topological Order

`status.topologicalOrder` is a map with the key `nodes` holding the Graph's own DAG in topological
order. When the Graph pre-compiles a forEach child Graph, the child's topology is stored as a
sibling key named after the forEach node:

```yaml
topologicalOrder:
  nodes: ["a", "b", "c"]
  b: ["x", "y", "z"]
```

The child's topology is available before any child Graph CR exists.

The Graph's status contains only controller-managed fields. There are no user-defined status fields
on the Graph itself. User-defined status (e.g., `deploymentReady`, `address`) lives on custom
resource types and is written via `patch:` nodes targeting the custom resource's status subresource.

## Owner References

The controller does not cascade-delete — teardown is ordered by the DAG. A Graph with
`metadata.ownerReferences` inherits standard K8s cascade: when any owner is Terminating, the
controller self-deletes the Graph. The resulting teardown runs the full ordered path — `finalizes`
sequences fire, prune walks the reverse DAG.

To hold the owner in Terminating during teardown, combine ownerReferences with a `patch:` node that
places a finalizer on the owner:

```yaml
metadata:
  ownerReferences:
    - apiVersion: kro.run/v1alpha1
      kind: WebApp
      name: my-instance
      uid: ...
spec:
  nodes:
    - id: lifecycle
      patch:
        apiVersion: kro.run/v1alpha1
        kind: WebApp
        metadata:
          name: my-instance
          finalizers:
            - experimental.kro.run/graph
    - id: deployment
      template: ...
```

The ownerReference triggers self-deletion. The patch holds the owner until teardown prunes it — SSA
releases the finalizer, the owner completes deletion.

## Why Not

**`immutable()` as a CEL function.** Write constraints (preventing field mutation after creation)
are a policy concern, not a value expression. CEL expressions produce values; they do not constrain
what values are acceptable.
