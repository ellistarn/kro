# Aggregated APIs

External systems hold state that Kubernetes resources depend on. This design extends kro's
integration boundary to external systems via the Kubernetes API Aggregation Layer, making external
state available as native Kubernetes resources that Graphs consume through existing Watch semantics.

## Commitments

### API Aggregation, Not CRD Sync

External state is served through `k8s.io/apiserver` aggregated API servers, not CRD controllers
that sync external state into Kubernetes objects. A CRD sync controller introduces a copy that can
drift from the source — drift between copy and source is the class of bug this design exists to
eliminate.

### No New kro Mechanism

kro consumes aggregated API resources through existing Watch, Own, and Contribute references.
A Graph with a Watch reference to an aggregated API resource is identical to a Watch on any other
Kubernetes resource. No new node types, no new reference types, no changes to the Graph controller.

### Dynamic Template from Expression

A node's `template` can be a standalone CEL expression that produces the entire resource:

```yaml
template: ${source.status.resources[0]}
```

The expression returns a `map[string]any` containing `apiVersion`, `kind`, `metadata`, and all
other fields. The controller resolves GVK from the expression result at evaluation time, not compile
time. This is required for consuming aggregated API resources whose type varies — the same GitPath
may return a Graph, a ConfigMap, or a CRD depending on what the file contains.

This depends on two existing Graph semantics: standalone `${expr}` preserves the CEL return type
(004-graph-execution), and identity fields (apiVersion, kind, metadata.name, metadata.namespace)
can be CEL expressions resolved at evaluation time (004-graph-execution, forEach identity
rendering). If the current implementation does not support a standalone expression as the entire
template value, this is a prerequisite for aggregated API consumption.

### Watch Fan-Out

Every provider must deduplicate upstream requests. N Graphs watching the same external resource
produce one upstream poll, with results distributed to all Kubernetes watchers. Without this, the
external API's rate limit becomes a function of Graph count rather than unique-resource count, and
providers hit rate limits at small scale.

### resourceVersion from Source State

resourceVersion for aggregated resources must change when and only when the external data changes.
A resourceVersion that increments on every poll (even when data is unchanged) causes spurious watch
events. kro's propagation-hash catches these downstream — the propagation hash won't change if the
data didn't change — but the unnecessary watch events still trigger node evaluation up to the hash
check. resourceVersion derived from a content hash or external-state identifier avoids this: no data
change, no watch event, no evaluation.

On crash recovery: clients reconnect with a fresh LIST to establish a baseline. The provider must
ensure the LIST response and subsequent WATCH events are consistent. Content-derived
resourceVersions satisfy this naturally — the same state produces the same version regardless of
whether the server restarted.

### Availability Under External Failure

When the external system is unreachable, the provider must return errors, not hang. The APIService
health check depends on the provider responding. A hanging provider causes the kube-apiserver to
mark the APIService unavailable, which breaks discovery for all resources in the API group —
including resources that could be served from cache.

## Provider Decision Space

These areas require provider-specific decisions. The design does not prescribe answers — each
depends on the external system's model.

**Scope: global vs namespaced.** Resources can be cluster-scoped or namespace-scoped. Cluster-scoped
is simpler and works well when the external system has its own authorization model (IAM policies,
org permissions). Namespace-scoped enables per-team credentials. Global is likely the right default.

**Authentication.** Depends on what the external system supports. Workload identity (EKS Pod
Identity, GKE Workload Identity) is zero-config and preferred when available. OAuth via resource
status composes with Graphs — a `readyWhen` gate on auth completion, printer columns surface the
auth URL. Static API keys are acceptable when nothing better exists.

**Caching strategy.** Whether the cache is the authoritative Kubernetes representation (eventually
consistent with the external system) or a read-through proxy (consistent but higher latency).
Poll intervals, webhook support, conditional request support.

**Rate limit budgeting.** How the external API's rate limit is distributed across active resources.
Adaptive polling, request deduplication, conditional requests.

## AggregateAPI as a Graph

Deployment sugar. A Graph that encapsulates the operational machinery — Deployment, PDB, Service,
APIService. Not core to the design — providers can be deployed however you want. This is a
convenience.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: github-provider
  namespace: kro-system
spec:
  nodes:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: github-apiserver
          namespace: kro-system
        spec:
          replicas: 2
          selector:
            matchLabels:
              app: github-apiserver
          template:
            metadata:
              labels:
                app: github-apiserver
            spec:
              containers:
                - name: apiserver
                  image: ghcr.io/kro/github-apiserver:latest
      readyWhen:
        - ${deployment.status.readyReplicas == deployment.spec.replicas}

    - id: pdb
      template:
        apiVersion: policy/v1
        kind: PodDisruptionBudget
        metadata:
          name: github-apiserver
          namespace: kro-system
        spec:
          minAvailable: 1
          selector:
            matchLabels:
              app: github-apiserver

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: github-apiserver
          namespace: kro-system
        spec:
          selector:
            app: github-apiserver
          ports:
            - port: 443
              targetPort: 8443

    - id: apiservice
      template:
        apiVersion: apiregistration.k8s.io/v1
        kind: APIService
        metadata:
          name: v1alpha1.api.github.com
        spec:
          service:
            name: github-apiserver
            namespace: kro-system
          group: api.github.com
          version: v1alpha1
          groupPriorityMinimum: 100
          versionPriority: 100
      readyWhen:
        - >-
          ${apiservice.status.conditions.exists(c,
            c.type == 'Available' && c.status == 'True')}
```
