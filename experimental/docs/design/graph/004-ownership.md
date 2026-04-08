# Ownership

A Graph manages Kubernetes resources through field managers. Each Graph
instance is a distinct field manager — the API server tracks which fields
belong to which Graph, and field management is the only ownership mechanism.
There are no explicit ownership types and no mode flags. The template content
and the API server's field ownership metadata determine everything.

## Background

001-graph.md defines three resource types — template, externalRef, and
contribution — each with different apply mechanics, tracking rules, and
deletion behavior. Template applies without force and cascade-deletes.
Contribution applies with force and orphans on delete. ExternalRef is
read-only. The controller has separate code paths for each, and the user
declares which type each resource is.

Two of these distinctions are unnecessary. ExternalRef (read a resource into
scope) is a template that specifies no fields beyond identity — the
controller can detect this from the template content. Contribution (write
partial fields to someone else's resource) is a template that specifies
fewer fields — per-field ownership handles this without a flag. The
remaining distinction — which resources to delete on cleanup — is answered
by the API server's field ownership metadata: if no other kro field manager
remains, delete; otherwise, release.

## Template

Every resource entry is a `template`. One field, one mechanism.

```yaml
resources:
  # Full resource — the Graph manages all specified fields.
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

  # Metadata-only — read into scope, no apply.
  - id: config
    template:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: shared-config
    readyWhen:
      - ${config.data.ready == "true"}

  # Partial — writes status fields only. The field manager
  # owns exactly these fields and nothing else.
  - id: status
    template:
      apiVersion: kro.run/v1alpha1
      kind: WebApp
      metadata:
        name: ${schema.metadata.name}
        namespace: ${schema.metadata.namespace}
      status:
        deploymentReady: ${deployment.status.availableReplicas == deployment.spec.replicas}
        address: ${service.status.loadBalancer.ingress[0].hostname}
```

### Metadata-Only

A template that specifies only identity — `apiVersion`, `kind`,
`metadata.name`, `metadata.namespace` — is read-only. The controller GETs
the resource and enters it into scope. No apply, no field manager entry,
no cleanup tracking.

If the target resource does not exist, the resource enters the `DataPending`
state — the same behavior as an unresolvable CEL reference. Downstream
dependents are blocked until the resource appears.

### Substance

A template that specifies anything beyond identity — labels, annotations,
spec, data, status — is applied with force. The fields in the template
are the fields the Graph manages. A partial template manages partial fields.
A complete template manages all fields.

### Transitions

If a metadata-only template gains substance (e.g., adding `data:` fields),
the controller transitions from GET-only to force-apply on the next reconcile.
The resource begins being tracked in the applied-resources annotation.

If a substance template is reduced to metadata-only, the controller treats
the removed fields as a prune — it force-applies the skeleton with the
Graph's field manager, releasing all previously managed fields. The resource
is removed from the applied-resources annotation and returns to GET-only.

When a template contains `.status` fields, the controller splits the apply
into two operations — metadata/spec via the main resource, status via the
status subresource. The status subresource has its own field ownership
entries. The lifecycle algorithm inspects both — a resource is only deleted
when no other `kro.run/*` manager appears in either the main or status
subresource field ownership metadata. A Graph that manages only status fields on a
resource prevents its deletion, same as one managing spec fields.

### Selector

`externalRef` with a label selector discovers a collection of resources and
enters them into scope as an array. This is a collection query — a different
operation from single-resource field management — and is retained as a
separate concept. The ownership model does not cover selectors; their
mechanics are unchanged.

### forEach

forEach stamps its template once per item. Each stamped resource follows the
same rules — metadata-only stamps are read-only, substance stamps are
force-applied. The ownership model applies per-stamp; forEach itself is
unchanged.

## Field Manager

Every Graph instance uses a dedicated field manager:

```
kro.run/<namespace>/<name>
```

This string identifies the Graph in every resource's field ownership
metadata. The convention is permanent — changing it orphans all existing
field manager entries across every resource the Graph has applied to.
Orphaned entries have no automated cleanup path; they persist until manually
removed or force-adopted by a new manager.

The Graph always applies with force, taking ownership of exactly
the fields its template specifies. Other field managers retain fields the
Graph doesn't specify. Two Graphs can manage different fields on the same
resource without interference.

## Lifecycle

One algorithm covers both prune (resource removed from spec) and teardown
(Graph deleted):

1. Apply a skeleton with the Graph's field manager. The skeleton is
   exactly: `apiVersion`, `kind`, `metadata.name`, `metadata.namespace` —
   no labels, no annotations, no spec, no data, no status. Omitted fields
   are released from this manager. The skeleton apply itself leaves a
   minimal field manager entry (metadata identity fields only).
2. Re-GET the resource. Inspect field ownership metadata for apply-operation
   entries whose manager string matches `kro.run/*`, excluding this Graph's
   own entry.
3. If no other `kro.run/*` Apply managers remain — delete the resource.
4. If other `kro.run/*` Apply managers remain — done. The resource survives.

| Trigger | Action | Delete? |
|---------|--------|---------|
| Template removed from spec | Release fields, check field ownership | If sole kro manager |
| Graph instance deleted | Release fields, check field ownership | If sole kro manager |

Teardown unwinds in reverse dependency order. Dependents release before the
resources they depend on.

During normal reconciliation, pruning applies to resources in the
`applied-resources` annotation that are absent from the current spec.

### Finalizer and Recovery

The Graph carries a finalizer that prevents API server removal until
teardown completes. All state needed for teardown is persisted:

- **applied-resources annotation** on the Graph — the cleanup index.
- **field ownership metadata** on each resource — which managers are active.
- **template hash** on each resource — change detection for idempotent
  re-entry.

Controller crash mid-teardown: the finalizer prevents Graph removal. Next
reconcile re-enters teardown. The annotation says what to clean up. Field
ownership metadata says what's left. Releasing already-released fields is a
no-op. If a tracked resource returns 404 on GET, it is already gone — the
controller removes it from the tracking annotation and moves on.

## Adoption and Migration

**Adopt.** Add a template for an existing resource to the Graph spec. The
next reconcile force-applies, taking ownership of the specified fields. The
resource was created by Helm, kubectl, another Graph — this Graph now
co-manages it.

**Abandon.** Remove the template. Prune releases the Graph's fields. If no
other kro manager exists, the resource is deleted. If another exists, the
resource survives.

**Migrate** (Graph A → Graph B). Add the template to Graph B. Graph B
force-applies, taking field ownership. Remove from Graph A. Graph A prunes
— sees Graph B's manager — releases without deleting.

If the order is reversed (remove from A before adding to B), Graph A prunes
and deletes (sole kro manager). Graph B creates on next reconcile. Brief
disruption, convergent recovery.

## Conflict Detection

Force-apply means the apply always succeeds. Conflicts are detected
post-apply by inspecting field ownership metadata.

The controller diffs its managed field set against other `kro.run/*`
managers on the same resource:

- **Disjoint fields** — no condition. Two Graphs manage different fields.
- **Overlapping fields** — warning condition on the Graph's status.
  Last reconcile wins. The warning identifies the contending Graph and the
  overlapping fields.

Conflict detection is informational, not blocking. The system always makes
progress.

## Tracking

The `applied-resources` annotation on the Graph tracks every resource the
Graph has applied to. This is the cleanup index — on prune or teardown, the
controller iterates this list.

All applied resources are tracked. The distinction between "created" and
"contributed to" does not exist — field ownership metadata is the ground
truth for what each Graph manages, and the count of remaining kro managers
determines whether deletion is safe.

The controller sets `internal.kro.run/graph-name` and
`internal.kro.run/graph-namespace` labels on resources it applies. These
labels are for visibility — they let operators query which resources a
Graph manages (`kubectl get all -l internal.kro.run/graph-name=my-app`).
They are not the ownership mechanism. Ownership is determined solely by
field ownership metadata; the labels are a convenience that could be lost
without affecting correctness.

Template hash annotations on resources provide change detection. If the hash
matches the last applied state, the controller skips the patch. This is
a performance optimization; correctness does not depend on it.

Under force-apply, skipping an apply means the controller does not re-assert
field ownership during that reconcile. If another actor force-applies the
same fields between reconciles and the template hash hasn't changed, the
controller will not retake the fields or detect the contention until the
next template change triggers a new apply. This is consistent with the
"converges on spec change" model from 003-performance.md — the controller
corrects drift when it has reason to apply, not continuously. Conflict
detection only runs after an apply, so contention during a hash-stable
period is undetectable until the next apply.

## Not Covered

- **Selector redesign.** Renaming or restructuring `externalRef` with
  selectors. The collection query mechanism is unchanged by this design.
- **Conflict condition schema.** The specific condition type, reason, and
  message format for field contention warnings. Delegated to implementation.
- **Annotation and finalizer key names.** The specific string values for
  `applied-resources`, template hash, and the finalizer. Delegated to
  implementation; the current keys are acceptable.

## Discarded Paths

**ExternalRef as a separate field.** A metadata-only template achieves the
same result — read an existing resource into scope without managing it. One
field type instead of two.

**Explicit contribution flag.** Field managers track ownership per-field.
A partial template manages partial fields. No boolean needed — ownership
granularity is per-field, not per-resource.

**Management labels as the ownership mechanism.** The prior design used
`internal.kro.run/graph-name` labels as the primary ownership signal —
checking labels to determine what to delete. Field ownership metadata is the
ownership mechanism now; labels are set for visibility but not consulted for
lifecycle decisions.

**Non-force apply with blocking conflicts.** 001-graph.md positioned
force-apply as harmful. The revised position: force-apply with post-apply
conflict detection is preferable — blocking conflicts halt reconciliation,
while field ownership inspection surfaces the same information without
blocking. A single force-apply code path eliminates the apply/contribute
split.

**OpenAPI schema comparison for contribution detection.** Requires schema
discovery. Many CRDs have no required fields, making completeness detection
unreliable. Per-field ownership via field managers is more precise and
requires no schema knowledge.

**Runtime existence detection.** Checking whether a resource exists before
first apply makes ownership a function of timing. The same template produces
different lifecycle behavior depending on cluster state the user doesn't
control. Field-manager-based ownership is deterministic.

**Prune without delete.** An earlier version proposed that pruning should
release fields but never delete, preserving resources for potential adoption.
This surprises the common case — a user who removes a Deployment from their
spec expects cleanup. Same algorithm for prune and teardown is predictable.
Migration safety is a sequencing requirement (add to B before removing from
A), not a default behavior.
