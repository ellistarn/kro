# OCI Artifacts

Graphs can be published to OCI registries and pulled into other Graphs. A Graph stored in a registry
is an artifact. An artifact is a Graph spec serialized as YAML, pushed as a single OCI layer. The
artifact server resolves OCI references, pulls content, and serves it as a virtual Kubernetes
resource. The graph controller reads artifacts through `ref:` nodes — no new node types, no engine
changes.

## oci() CEL Function

The `oci()` CEL function takes an OCI reference string and returns a DNS-compliant Kubernetes
resource name. The function base32-encodes the URI (RFC 4648, lowercased, no padding) so the result
contains only `[a-z2-7]` — always valid as a Kubernetes name, always reversible. A typical 80-char
OCI URI encodes to ~128 characters, well under the 253-character name limit.

```yaml
- id: networking
  ref:
    apiVersion: artifacts.kro.run/v1alpha1
    kind: Artifact
    metadata:
      name: ${oci("123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v1.0.0")}
```

The CEL expression is evaluated at compile time. The encoded name is opaque — users do not read or
type it. The `oci()` function accepts any valid OCI reference: tag, digest, or tag-with-digest.

```yaml
# Tag
name: ${oci("123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v1.0.0")}

# Digest
name: ${oci("123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking@sha256:a3f2b7c...")}
```

The function is pure — same input, same output. Two Graphs referencing the same OCI URI produce the
same encoded name. The artifact server decodes the name back to the original URI.

## Artifact Server

The artifact server is an aggregated API server registered under `artifacts.kro.run/v1alpha1`. It
serves a single resource type: `Artifact`. Artifacts are virtual — they do not exist in etcd. Each
GET triggers a resolution against the OCI registry (or returns a cached result).

```
GET /apis/artifacts.kro.run/v1alpha1/artifacts/<base32-encoded-oci-ref>
```

The server decodes the name, resolves the tag to a digest, pulls the manifest and layer, and returns
the content as a Kubernetes-shaped response. Artifacts are cluster-scoped — an OCI reference is
global, not namespaced.

```yaml
apiVersion: artifacts.kro.run/v1alpha1
kind: Artifact
metadata:
  name: <base32-encoded-oci-ref>
  annotations:
    artifacts.kro.run/uri: "123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v1.0.0"
    artifacts.kro.run/digest: "sha256:a3f2b7c..."
status:
  content:
    nodes:
      - id: vpc
        template:
          apiVersion: ec2.services.k8s.aws/v1alpha1
          kind: VPC
          ...
```

`status.content` is the deserialized Graph spec from the OCI layer. The annotation
`artifacts.kro.run/uri` is the original URI before encoding — diagnostic, not functional.

### Caching

The artifact server caches pulled content in memory keyed by digest. Tag resolution is performed on
each GET — tag-to-digest mapping is the only request that hits the registry when the content is
cached. Digest-pinned references skip tag resolution entirely.

Cache entries are evicted by LRU when the cache exceeds a configured size. The default is
unbounded — Graph specs are small (a 50-node Graph is ~30KB) and practical deployments serve
hundreds, not millions, of distinct artifacts.

### Authentication

The artifact server authenticates to ECR using Pod Identity. The server's ServiceAccount is
annotated with an IAM role that has `ecr:GetAuthorizationToken`, `ecr:BatchGetImage`, and
`ecr:GetDownloadUrlForLayer` permissions. The server exchanges the Pod Identity token for an ECR
auth token and uses it for OCI pull requests.

ECR is OCI-compliant — the pull path is standard OCI Distribution (resolve manifest, fetch blob).
Only the authentication differs. The server uses a minimal OCI client: resolve a tag to a manifest
via `GET /v2/<repo>/manifests/<ref>`, fetch a blob via `GET /v2/<repo>/blobs/<digest>`. No
container runtime, no image unpacking, no layer composition.

### Registration

The artifact server is deployed as a Deployment and Service in `kro-system`. A single APIService
registers the `artifacts.kro.run` API group:

```yaml
apiVersion: apiregistration.k8s.io/v1
kind: APIService
metadata:
  name: v1alpha1.artifacts.kro.run
spec:
  group: artifacts.kro.run
  version: v1alpha1
  service:
    name: kro-artifact-server
    namespace: kro-system
  groupPriorityMinimum: 100
  versionPriority: 100
```

The artifact server is optional. Clusters that do not use OCI artifacts do not deploy it. The graph
controller has no dependency on the artifact server — it discovers `artifacts.kro.run` through
standard API discovery, same as any other type.

### Watch Behavior

The graph controller registers metadata-only informers for every GVR referenced by `ref:` nodes.
The artifact server supports list and watch on Artifacts. A watch stream returns no events — artifact
content changes are detected by the graph controller's per-node resync timer, which re-GETs the
artifact on each resync interval. When the tag resolves to a new digest, the GET returns different
content, the output-hash changes, and downstream nodes re-evaluate.

For digest-pinned references, resync still fires but the content never changes — the output-hash
matches and dependents are not triggered. The cost is one GET per resync interval per artifact ref.

The artifact server's list response includes all artifacts that have been resolved (cached) during
the server's lifetime. This satisfies the informer's initial list requirement. Artifacts not yet
requested return 404 on GET — the `ref:` node reports `ErrPending` and the graph retries on the
next resync.

## Consumption

A Graph pulls an artifact with `ref:` and templates a child Graph from its content. Two nodes — one
read, one write.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: us-west-2
spec:
  nodes:
    - id: networking
      ref:
        apiVersion: artifacts.kro.run/v1alpha1
        kind: Artifact
        metadata:
          name: ${oci("123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v1.0.0")}

    - id: networkGraph
      template:
        apiVersion: experimental.kro.run/v1alpha1
        kind: Graph
        metadata:
          name: us-west-2-networking
        spec:
          nodes: ${networking.status.content.nodes}
```

The `ref:` node reads the Artifact. The `template:` node creates a child Graph whose `spec.nodes`
is the artifact's content. The child Graph is a regular Graph — reconciled independently by the graph
controller. Nested evaluation rules from [001-graph](001-graph.md#evaluation-boundary) apply: the
child's scope is independent, `$${...}` escaping works as documented.

### Parameterization

Artifacts contain Graphs with CEL expressions. Those expressions are evaluated by the child Graph's
controller against the child's own scope. To parameterize an artifact, the parent prepends a `def:`
node to the artifact's node list when constructing the child Graph:

```yaml
- id: networking
  ref:
    apiVersion: artifacts.kro.run/v1alpha1
    kind: Artifact
    metadata:
      name: ${oci("123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v1.0.0")}

- id: networkGraph
  template:
    apiVersion: experimental.kro.run/v1alpha1
    kind: Graph
    metadata:
      name: us-west-2-networking
    spec:
      nodes: ${[{"id": "params", "def": {"vpcName": "us-west-2-main", "cidr": "10.0.0.0/16"}}]
               + networking.status.content.nodes}
```

The CEL list concatenation prepends a `params` def node. The artifact's nodes resolve
`${params.vpcName}` against the child scope. The parent controls the parameters; the artifact
defines the structure.

The natural pattern uses a Kind — the Kind's schema IS the parameter surface, and the artifact
provides the implementation:

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Kind
metadata:
  name: networking
spec:
  kind: Networking
  group: platform.kro.run
  versions:
    - name: v1alpha1
      schema:
        spec:
          vpcName: string
          cidr: string
        status:
          vpcId: ${vpc.status.vpcID}
  nodes:
    - id: artifact
      ref:
        apiVersion: artifacts.kro.run/v1alpha1
        kind: Artifact
        metadata:
          name: ${oci("123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v1.0.0")}

    - id: graph
      template:
        apiVersion: experimental.kro.run/v1alpha1
        kind: Graph
        metadata:
          name: ${schema.metadata.name}-networking
        spec:
          nodes: ${artifact.status.content.nodes}
```

The Kind creates a CRD with `spec.vpcName` and `spec.cidr`. Each instance gets a child Graph whose
nodes come from the artifact. The artifact's expressions reference `${schema.spec.vpcName}` — the
Kind's per-instance scope variable, injected automatically by the Kind machinery
([006-standard-library](006-standard-library.md#kind)).

### Version Selection

CEL string concatenation builds the OCI reference dynamically. A Kind can expose the artifact
version as a spec field:

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Kind
metadata:
  name: region
spec:
  kind: Region
  group: platform.kro.run
  versions:
    - name: v1alpha1
      schema:
        spec:
          name: string
          registry: string | default=123456789.dkr.ecr.us-west-2.amazonaws.com
          networkingVersion: string | default=v2.1.0
          computeVersion: string | default=v1.0.0
        status:
          ready: >-
            ${networkGraph.ready().orValue(false)
             && computeGraph.ready().orValue(false)}
  nodes:
    - id: networkArtifact
      ref:
        apiVersion: artifacts.kro.run/v1alpha1
        kind: Artifact
        metadata:
          name: ${oci(schema.spec.registry + "/graphs/networking:" + schema.spec.networkingVersion)}

    - id: networkGraph
      template:
        apiVersion: experimental.kro.run/v1alpha1
        kind: Graph
        metadata:
          name: ${schema.metadata.name}-networking
        spec:
          nodes: ${networkArtifact.status.content.nodes}

    - id: computeArtifact
      ref:
        apiVersion: artifacts.kro.run/v1alpha1
        kind: Artifact
        metadata:
          name: ${oci(schema.spec.registry + "/graphs/compute:" + schema.spec.computeVersion)}

    - id: computeGraph
      template:
        apiVersion: experimental.kro.run/v1alpha1
        kind: Graph
        metadata:
          name: ${schema.metadata.name}-compute
        spec:
          nodes: ${computeArtifact.status.content.nodes}
```

Each Region instance specifies its artifact versions. Updating `networkingVersion` on one Region
triggers re-resolution — the artifact server pulls the new tag, the ref node sees the content
change, and the child Graph spec updates.

## Publishing

An artifact is a Graph spec serialized as YAML, pushed as a single-layer OCI artifact. Standard OCI
tooling works:

```bash
crane push networking.yaml \
  123456789.dkr.ecr.us-west-2.amazonaws.com/graphs/networking:v1.0.0
```

The content of `networking.yaml` is a Graph spec — the same YAML that would appear under
`spec.nodes` in a Graph object:

```yaml
nodes:
  - id: vpc
    template:
      apiVersion: ec2.services.k8s.aws/v1alpha1
      kind: VPC
      metadata:
        name: ${params.vpcName}
      spec:
        cidrBlocks:
          - ${params.cidr}
  - id: subnets
    forEach:
      az: ${params.availabilityZones}
    template:
      apiVersion: ec2.services.k8s.aws/v1alpha1
      kind: Subnet
      metadata:
        name: ${params.vpcName}-${az.name}
      spec:
        vpcId: ${vpc.status.vpcID}
        availabilityZone: ${az.name}
        cidrBlock: ${az.cidr}
```

The media type is `application/vnd.kro.graph.v1+yaml`. The artifact server accepts both YAML and
JSON.

### Signing

OCI artifacts support signing through cosign and Notation. Signatures are stored as referrer
manifests — they do not modify the artifact itself. A signed artifact has the same digest and content
as an unsigned one. Signature verification is a future concern for the artifact server. The artifact
format (single YAML layer) is compatible with both signing frameworks.

## Recursive Composition

Artifacts enable recursive Graph composition. A Graph pulls an artifact. That artifact's content is
itself a Graph that can pull other artifacts. Each level is a separate Graph object reconciled
independently.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: acme-corp
spec:
  nodes:
    - id: config
      ref:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: acme-config

    - id: regions
      forEach:
        r: ${config.data.regions}
      template:
        apiVersion: platform.kro.run/v1alpha1
        kind: Region
        metadata:
          name: ${r.name}
        spec:
          name: ${r.name}
          networkingVersion: ${r.networkingVersion}
          computeVersion: ${r.computeVersion}
```

One Graph creates N Regions. Each Region Kind pulls versioned artifacts from OCI and unfurls them as
child Graphs. Each child Graph independently reconciles its resources. Updates to an artifact version
on one Region propagate to that Region only — O(1) per instance.

The content model enforces this recursion. Artifacts contain Graph specs — not arbitrary Kubernetes
objects. Pulling an artifact always produces a Graph. A single Deployment is a one-node Graph. An
entire platform is a Graph of Graphs. The primitive is uniform.

## OCI Client

The artifact server uses a minimal OCI client. ECR is OCI Distribution-compliant — the client
implements two HTTP endpoints:

1. **Resolve manifest**: `GET /v2/<repository>/manifests/<reference>` — returns the manifest JSON.
   For tags, the response includes `Docker-Content-Digest` with the resolved digest.
2. **Fetch blob**: `GET /v2/<repository>/blobs/<digest>` — returns the layer content.

Authentication uses the ECR `GetAuthorizationToken` API via the AWS SDK, exchanging Pod Identity
credentials for a registry auth token. The token is cached for its lifetime (12 hours for ECR).

The client is ~200 lines. No dependency on go-containerregistry, oras, or other OCI libraries. The
scope is narrow: resolve a tag, fetch one blob, return bytes.

## Scoped Out

- **Non-ECR registries.** Auth is ECR-specific (Pod Identity + `GetAuthorizationToken`). The OCI
  pull path is generic. Supporting other registries requires pluggable auth — a future extension
  point.
- **Signature verification.** The format supports it. The server does not verify signatures today.
- **Git-based artifact sources.** A Git aggregated API server could serve artifacts from Git
  repositories using the same `ref:` + child Graph pattern. The graph controller does not
  distinguish between artifact sources — any API that serves the Artifact schema works.
- **Content types other than Graph specs.** Artifacts are always Graph specs. Arbitrary Kubernetes
  objects are not supported. A single resource is a one-node Graph.
- **Disk caching.** The artifact server caches in memory. Disk-based caching (local OCI layout,
  compressed layers) is a future optimization for large deployments.
- **Webhook/push-based invalidation.** Tag re-resolution happens on each GET. Event-driven
  invalidation (ECR EventBridge → webhook) is a future optimization.

## Why Not

**New node type.** An `artifact:` keyword would encode OCI semantics into the graph engine. The
graph engine has no concept of external registries — `ref:` reads any Kubernetes resource. The
artifact server makes OCI content look like a Kubernetes resource. The graph engine stays generic.

**CRDs for configuration.** A Repository CRD or Artifact CRD would store the URI-to-name mapping in
etcd. The `oci()` CEL function eliminates this indirection — the URI is in the expression, the name
is computed, the server decodes it. Zero configuration objects.

**Content in etcd.** Storing artifact content in a CRD status field is simpler but persists external
data in etcd. The aggregated API server avoids etcd storage entirely — content lives in the server's
memory cache, sourced from the OCI registry.

**Controller-side OCI pulls.** Embedding the OCI client in the graph controller mixes concerns and
duplicates work across controller replicas. The artifact server centralizes pulls, caching, and
credential management.

**Dynamic API groups per repository.** Each repository could register its own API group
(`my-repo.artifacts.kro.run`), making the tag the resource name directly. This eliminates base32
encoding but requires dynamic APIService registration — more infrastructure complexity for a cosmetic
improvement. The encoded name is never hand-authored.
