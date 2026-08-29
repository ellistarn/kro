# GitHub Provider

An aggregated API server that serves GitHub state as native Kubernetes resources. API group:
`api.github.com`. Two resource types: GithubArtifact (content) and GithubAuthentication (OAuth mechanics).
Ships with a coordination Graph that wires auth status from GithubAuthentications back to GithubArtifacts.

## Resources

### GithubArtifact

Points at a file or directory in a GitHub repository. Returns parsed YAML as structured data.

```yaml
apiVersion: api.github.com/v1alpha1
kind: GithubArtifact
metadata:
  name: string
  namespace: string
spec:
  identity: string    # optional — cluster-wide credential scope, defaults to "default"
  uri: string         # github.com/<owner>/<repo>/<path>@<ref>
status:
  phase: string       # Authenticating | Pending | Ready | Expired
  authURL: string     # verification URL — empty when not authenticating
  authCode: string    # device code — empty when not authenticating
  sha: string         # resolved commit SHA
  shortSha: string    # first 12 characters
  resources: array    # []map[string]any — parsed YAML documents
```

`spec.uri` encodes owner, repo, path, and ref in one string:

```
github.com/<owner>/<repo>/<path>@<ref>
```

`@` separates the ref. Owner and repo are always the first two path segments after `github.com`.
Examples:

```
github.com/ellistarn/myapp/k8s/my-graph.yaml@main
github.com/ellistarn/myapp/k8s/@v1.2.0
github.com/ellistarn/infra/modules/vpc.yaml@abc123
```

`spec.identity` is an optional cluster-wide string key. All GithubArtifacts sharing an identity share
one OAuth token regardless of namespace. Defaults to `default` if omitted. The first GithubArtifact
with a new identity triggers the device flow. Others wait. The token covers any repo the
authenticated user can access.

`status.resources` is always `[]map[string]any` — parsed YAML documents. A single-file path
returns a list of one. A directory returns all documents from all files. A multi-document YAML
file returns one entry per document.

Status fields `phase`, `authURL`, and `authCode` are written by the coordination Graph via
Contribute, not by the aggregated API server directly.

### GithubAuthentication

Triggers an OAuth device flow for an identity. The server handles the OAuth mechanics — creating
a GithubAuthentication initiates the flow, status reflects progress. Cluster-scoped.

```yaml
apiVersion: api.github.com/v1alpha1
kind: GithubAuthentication
metadata:
  name: string        # identity key
spec: {}
status:
  phase: string       # Authenticating | Ready | Expired
  authURL: string     # verification URL
  authCode: string    # device code
  expiresAt: string   # RFC3339 expiry of the device code
```

GithubAuthentication is a server-managed resource. The coordination Graph creates and watches them. Users
don't interact with GithubAuthentications directly — they see auth status on GithubArtifacts via printer
columns.

### Phases

**GithubArtifact:**

| Phase | Meaning |
|---|---|
| `Authenticating` | Waiting for GithubAuthentication to complete. `authURL` and `authCode` set by Graph. |
| `Pending` | Authenticated. Waiting for first successful content fetch. |
| `Ready` | Content fetched and up to date. `status.resources` populated. |
| `Expired` | Token expired and refresh failed. `authURL` and `authCode` set by Graph. |

**GithubAuthentication:**

| Phase | Meaning |
|---|---|
| `Authenticating` | OAuth device flow in progress. |
| `Ready` | Token obtained and stored. |
| `Expired` | Token expired, refresh failed. New device code issued. |

### Printer Columns

| Name | JSON Path | Type |
|---|---|---|
| Phase | `.status.phase` | string |
| AuthURL | `.status.authURL` | string |
| AuthCode | `.status.authCode` | string |
| URI | `.spec.uri` | string |
| SHA | `.status.shortSha` | string |
| Age | `.metadata.creationTimestamp` | date |
| Identity | `.spec.identity` | string |

```
$ kubectl get githubartifacts -A
NAMESPACE    NAME        PHASE            AUTHURL                               AUTHCODE    URI                                              SHA            AGE   IDENTITY
team-alpha   graph       Authenticating   https://github.com/login/device       ABCD-1234   github.com/acme/app/k8s/graph.yaml@main                        5s    default
team-alpha   config      Pending                                                            github.com/acme/app/config.yaml@main                           5s    default
team-beta    shared      Ready                                                              github.com/acme/lib/src/@main                   def789abc123   10m   shared
infra        modules     Expired          https://github.com/login/device       WXYZ-5678   github.com/acme/infra/modules/@main             789def012345   30d   platform
```

## Examples

### Apply a Graph from GitHub

A single file in GitHub contains a Graph definition. This Graph watches the file and applies it
to the cluster. Push a change → Graph updates.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: myapp-sync
  namespace: team-alpha
spec:
  nodes:
    - id: source
      template:
        apiVersion: api.github.com/v1alpha1
        kind: GithubArtifact
        metadata:
          name: myapp-graph
        spec:
          uri: github.com/acme/app/k8s/graph.yaml@main
      readyWhen:
        - "source.status.phase == 'Ready'"

    - id: resource
      includeWhen:
        - "size(source.status.resources) > 0"
      template: ${source.status.resources[0]}
```

### Apply a Directory of Manifests from GitHub

A directory in GitHub contains multiple YAML files — Graphs, RGDs, ConfigMaps, whatever. This
Graph watches the directory and applies every document. Push a new file → it appears. Remove a
file → forEach cleans it up.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: myapp-k8s
  namespace: team-alpha
spec:
  nodes:
    - id: source
      template:
        apiVersion: api.github.com/v1alpha1
        kind: GithubArtifact
        metadata:
          name: myapp-k8s
        spec:
          uri: github.com/acme/app/k8s/@main
      readyWhen:
        - "source.status.phase == 'Ready'"

    - id: resources
      forEach:
        r: ${source.status.resources}
      template: ${r}
```

### Apply Everything

Watch all GithubArtifacts in the cluster. Flatten all their resources into one list. Apply CRDs
first, then everything else — Graph dependency ordering handles the sequencing.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: github-sync
  namespace: kro-system
spec:
  nodes:
    - id: artifacts
      template:
        apiVersion: api.github.com/v1alpha1
        kind: GithubArtifact
        selector: {}

    # Tier 1: CRDs — must be established before resources that use them
    - id: crds
      forEach:
        r: >-
          ${artifacts
            .filter(a, a.status.phase == 'Ready')
            .map(a, a.status.resources)
            .flatten()
            .filter(r, r.kind == 'CustomResourceDefinition')}
      template: ${r}
      readyWhen:
        - >-
          crds.status.conditions.exists(c,
            c.type == 'Established' && c.status == 'True')

    # Tier 2: Everything else — depends on CRDs being established
    - id: resources
      forEach:
        r: >-
          ${artifacts
            .filter(a, a.status.phase == 'Ready')
            .map(a, a.status.resources)
            .flatten()
            .filter(r, r.kind != 'CustomResourceDefinition')}
      template: ${r}
```

`crds` readyWhen gates on Established — `resources` won't evaluate until every CRD is ready.
Push a new RGD + Graph in the same commit → CRD created first → Graph applied after. Natural
ordering through Graph dependency semantics, no special sync logic.

## Coordination Graph

Ships with the provider. Watches all GithubArtifacts, deduplicates identity keys, creates one
GithubAuthentication per unique identity, and Contributes auth status back to each GithubArtifact.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: github-auth
  namespace: kro-system
spec:
  nodes:
    # Watch all GithubArtifacts across the cluster
    - id: artifacts
      template:
        apiVersion: api.github.com/v1alpha1
        kind: GithubArtifact
        selector: {}

    # One GithubAuthentication per unique identity
    - id: auths
      forEach:
        identity: ${artifacts.map(a, a.spec.identity).distinct()}
      template:
        apiVersion: api.github.com/v1alpha1
        kind: GithubAuthentication
        metadata:
          name: ${identity}
      readyWhen:
        - "auths.status.phase == 'Ready'"

    # Contribute auth status back to each GithubArtifact
    - id: authStatus
      forEach:
        a: ${artifacts}
      template:
        apiVersion: api.github.com/v1alpha1
        kind: GithubArtifact
        metadata:
          name: ${a.metadata.name}
          namespace: ${a.metadata.namespace}
        status:
          phase: >-
            ${auths.filter(x, x.metadata.name == a.spec.identity).size() > 0
              ? auths.filter(x, x.metadata.name == a.spec.identity)[0].status.phase
              : 'Authenticating'}
          authURL: >-
            ${auths.filter(x, x.metadata.name == a.spec.identity).size() > 0
              ? auths.filter(x, x.metadata.name == a.spec.identity)[0].status.authURL
              : ''}
          authCode: >-
            ${auths.filter(x, x.metadata.name == a.spec.identity).size() > 0
              ? auths.filter(x, x.metadata.name == a.spec.identity)[0].status.authCode
              : ''}
```

The server handles OAuth mechanics (GithubAuthentication). The Graph handles coordination (fan-in identities,
fan-out status). Auth state is visible on both resources — `kubectl get githubauthentication` for the
identity-level view, `kubectl get githubartifacts` for the per-path view.

## Lifecycle

1. **GithubArtifact created** — coordination Graph sees it, creates GithubAuthentication for its identity if
   one doesn't exist. Contributes `Authenticating` phase with auth URL/code.
2. **User authorizes** — GithubAuthentication transitions to Ready. Graph Contributes `Pending` to all
   GithubArtifacts with that identity. Server fetches content.
3. **Ready** — server resolves ref, fetches content, populates `status.resources`.
4. **Ref advances** — branch HEAD changes → SHA updates → resources update. Phase stays `Ready`.
5. **Token expiry** — GithubAuthentication transitions to `Expired` with new device code. Graph Contributes
   `Expired` phase and new auth URL/code to all GithubArtifacts with that identity.
6. **GithubArtifact deleted** — if no other GithubArtifacts share the identity, Graph deletes the
   GithubAuthentication (forEach naturally handles this — identity disappears from distinct list).

### Coordination Graph Operations

The coordination Graph (`github-auth`) is deployed alongside the aggregated API server as part of
the provider package. It is a standard kro Graph — kro's controller owns its lifecycle.

**Failure:** If the coordination Graph's controller encounters an error (CEL evaluation failure,
RBAC issue, API server unreachable), it follows normal Graph error handling — retry with backoff,
surface errors on Graph status. GithubArtifacts remain in their last Contributed phase until the
Graph recovers. Content fetching is unaffected — the server continues serving cached content for
already-authenticated identities.

**Recovery:** On restart, the Graph re-evaluates all nodes. WatchKind re-discovers all
GithubArtifacts. forEach re-derives unique identities. Existing GithubAuthentications are found
(not recreated). Status Contributions resume. No data loss — GithubAuthentication tokens are
server-side, not in the Graph.

**Ownership:** The coordination Graph owns GithubAuthentication resources via Own. Deleting the
coordination Graph deletes all GithubAuthentications (and their tokens). This is intentional —
the coordination Graph IS the auth lifecycle manager.

## Authentication

### OAuth Device Flow

The GitHub provider project registers one OAuth App with GitHub — maintained by the project,
client ID embedded in the binary.

The coordination Graph deduplicates by identity. The server handles token acquisition and refresh
via GithubAuthentication resources. Tokens are stored server-side, never exposed in status.

### GitHub App

Production path:

```
github-provider init --org acme
```

GitHub App Manifest flow (browser) or non-interactive (`--app-id=12345 --private-key-file=key.pem`).
GithubArtifacts skip the device flow and transition to `Ready` immediately.

## Caching

**ETag-based conditional requests.** `If-None-Match` returns 304 without counting against rate limit.

**SHA immutability.** Content at a resolved SHA never changes. Cached indefinitely.

**Branch resolution as polling target.** The one thing that changes. Once HEAD SHA is known, content
is a cache lookup.

### resourceVersion

Derived from a content hash of the resolved resource data. A GithubArtifact's resourceVersion
changes when the resolved SHA changes or the parsed content changes — not on every poll.
Branch resolution polls that return the same HEAD SHA produce no resourceVersion change, no watch
event, no downstream evaluation. This fulfills the 005 commitment that resourceVersion changes when
and only when external data changes.

On crash recovery: the server re-resolves all active refs on startup. Content-derived
resourceVersions produce the same values for the same state — clients reconnecting with a fresh
LIST get a consistent baseline.

### Rate Limit Budget

GitHub: 5,000 requests/hour (authenticated).

- **Request deduplication** — multiple GithubArtifacts pointing at the same `(repo, ref)` produce
  one upstream poll. The cache key is `(owner, repo, ref)`.
- **Conditional requests** — ETags on branch resolution. 304s don't count against rate limit.
- **Adaptive polling** — `X-RateLimit-Remaining` header drives poll interval. Below 20% headroom,
  interval doubles. Below 5%, polling pauses until reset.
- **SHA-keyed content** — file content fetched once per SHA and cached indefinitely. Only branch
  HEAD resolution incurs ongoing polling cost.
