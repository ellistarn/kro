# Crypto CEL functions for the experimental Graph compiler

## Problem statement

Many Kubernetes workloads require a private key plus a matching Certificate
Signing Request (CSR). Today, a kro `Graph` author has no in-graph way to
produce these without pre-generating out-of-band or pulling in cert-manager.

This proposal introduces CEL functions that generate the cryptographic
parameters required for certificate issuance — a private key and a matching
CSR as PEM strings. The functions are pure, stateless wrappers around Go's
`crypto` stdlib. The kro Graph is responsible for lifecycle concerns —
persistence, idempotency, drift detection, and rotation — expressed as
standard CEL expressions.

## Proposal

Introduce CEL functions that mirror Go's crypto stdlib — separate functions
for key generation and CSR creation. Each function maps 1:1 to a Go stdlib
call with `crypto/rand.Reader` injected implicitly and PEM encoding applied
to the output.

#### Overview

* **Functions (v1)**

  | CEL function | Go equivalent | Args | Return |
  |---|---|---|---|
  | `rsa.generateKey(bits)` | `rsa.GenerateKey(rand.Reader, bits)` | `bits: int` — 2048, 3072, 4096 | `string` (PKCS#8 PEM) |
  | `ecdsa.generateKey(curve)` | `ecdsa.GenerateKey(curve, rand.Reader)` | `curve: string` — `"P256"`, `"P384"`, `"P521"` | `string` (PKCS#8 PEM) |
  | `ed25519.generateKey()` | `ed25519.GenerateKey(rand.Reader)` | none | `string` (PKCS#8 PEM) |
  | `x509.createCertificateRequest(key, template)` | `x509.CreateCertificateRequest(rand.Reader, tmpl, priv)` | `key: string` (PEM), `template: map` | `string` (CSR PEM) |
  | `crypto.publicKey(key)` | `crypto.Signer.Public()` + `x509.MarshalPKIXPublicKey` | `key: string` (private key PEM) | `string` (PKIX public key PEM) |

* **Behavior**

  * Key generation functions always produce a fresh key (fresh randomness).
  * `x509.createCertificateRequest` parses the PEM key, builds a PKCS#10
    CSR using the template fields, and returns PEM.
  * All functions are stateless — no caching, no awareness of prior calls.

* **Where state lives**

  The functions have no Kubernetes client and no opinion on storage. They
  return PEM strings; the caller decides where to persist them.

* **Caller-managed lifecycle**

  Idempotency, config-change detection, time-based rotation, and
  multi-cert scaling are caller concerns — not function concerns.
  Example patterns are provided in the author-facing examples section.

#### Design details

##### File and registration

* Functions defined in `experimental/controller/compiler/celfuncs.go`
  alongside the other custom CEL functions.
* Registered into every compilation environment by appending their
  `[]cel.EnvOption` to `customCELFunctions()`.

##### Function declarations

* `rsa.generateKey`: overload `rsa_generateKey_int`, args `[cel.IntType]`,
  return `cel.StringType`, binding `cel.UnaryBinding`.
* `ecdsa.generateKey`: overload `ecdsa_generateKey_string`, args
  `[cel.StringType]`, return `cel.StringType`, binding `cel.UnaryBinding`.
* `ed25519.generateKey`: overload `ed25519_generateKey`, args `[]`,
  return `cel.StringType`, binding `cel.FunctionBinding`.
* `x509.createCertificateRequest`: overload
  `x509_createCertificateRequest_string_map`, args
  `[cel.StringType, cel.DynType]`, return `cel.StringType`, binding
  `cel.BinaryBinding`.
* `crypto.publicKey`: overload `crypto_publicKey_string`, args
  `[cel.StringType]`, return `cel.StringType`, binding
  `cel.UnaryBinding`.

##### Key generation — validation

| Function | Valid args | Error on |
|---|---|---|
| `rsa.generateKey(bits)` | 2048, 3072, 4096 | Any other int |
| `ecdsa.generateKey(curve)` | `"P256"`, `"P384"`, `"P521"` | Any other string |
| `ed25519.generateKey()` | (none) | n/a |

##### `x509.createCertificateRequest` — template schema

The `template` map mirrors Go's `x509.CertificateRequest` struct:

| Key | Type | Required | Meaning |
|-----|------|----------|---------|
| `subject` | `map` | yes | Certificate subject (see subject fields below). |
| `dnsNames` | `list<string>` | no | SAN DNS entries. |
| `ipAddresses` | `list<string>` | no | SAN IP entries. Each must be a valid IPv4 or IPv6 string. |

**Subject fields** (`subject` map):

| Key | Type | Required | Meaning |
|-----|------|----------|---------|
| `commonName` | `string` | no | Subject CN. If present and non-empty, also added to SAN DNS names automatically. |
| `organization` | `list<string>` | no | Subject Organization (`pkix.Name.Organization`). Used for Kubernetes RBAC group mapping. |

`subject` map is required (caller must explicitly acknowledge the
subject), but `commonName` within it is optional. If `commonName` is
absent, at least one of `dnsNames` or `ipAddresses` must be provided —
a CSR with no identity is rejected.

Additional fields (`organizationalUnit`, `country`, `emailAddresses`,
`uris`) are deferred. Unknown keys return an explicit error.

##### Implementation outline

Each function:
1. Validates args (type + value range).
2. Calls the corresponding Go stdlib function with `crypto/rand.Reader`.
3. PEM-encodes the result (PKCS#8 for keys, `CERTIFICATE REQUEST` for CSR).
4. Returns the PEM string, or `types.NewErr(...)` on failure.

`x509.createCertificateRequest` additionally:
1. PEM-decodes the key arg → `x509.ParsePKCS8PrivateKey`.
2. Builds `x509.CertificateRequest` template from the map.
3. Sets `Subject` from `subject.commonName` (if present) and
   `subject.organization` (if present).
4. Adds `commonName` to `DNSNames` (deduplicated) if non-empty.
5. Parses `ipAddresses` entries via `net.ParseIP`; invalid → error.
6. Validates that at least one identity is present (CN, dnsNames, or
   ipAddresses). Empty identity → error.
7. Chooses signature algorithm from the parsed key type.

##### Author-facing examples

###### Basic: generate once, passthrough on subsequent reconciles

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: my-app-cert
spec:
  nodes:
    - id: existingSecret
      ref:
        apiVersion: v1
        kind: Secret
        name: my-app-tls

    - id: key
      def: ${existingSecret.?data["tls.key"].hasValue()
            ? existingSecret.data["tls.key"]
            : ecdsa.generateKey("P256")}

    - id: csr
      def: ${existingSecret.?data["tls.csr"].hasValue()
            ? existingSecret.data["tls.csr"]
            : x509.createCertificateRequest(key, {
                "subject": {
                  "commonName": "my-app.default.svc",
                  "organization": ["my-team"]
                },
                "dnsNames": ["my-app", "my-app.default.svc"],
                "ipAddresses": ["10.0.0.5"]
              })}

    - id: keySecret
      template:
        apiVersion: v1
        kind: Secret
        metadata:
          name: my-app-tls
        type: Opaque
        stringData:
          tls.key: ${key}
          tls.csr: ${csr}

    - id: csrRequest
      template:
        apiVersion: certificates.k8s.io/v1
        kind: CertificateSigningRequest
        metadata:
          name: my-app
        spec:
          request: ${base64.encode(csr)}
          signerName: kubernetes.io/kubelet-serving
          usages: ["server auth"]
```

###### With config-change detection and time-based rotation

```yaml
    - id: existingSecret
      ref:
        apiVersion: v1
        kind: Secret
        name: my-app-tls

    - id: configHash
      def: ${base64.encode(hash.sha256("ECDSA|P256|my-app.default.svc"))}

    - id: shouldRegenerate
      def: ${
        !existingSecret.?data["tls.key"].hasValue()
        || existingSecret.?data["configHash"].orValue("") != configHash
        || timestamp(existingSecret.?data["rotateAfter"].orValue("2099-01-01T00:00:00Z")) < time.now()
      }

    - id: key
      def: ${shouldRegenerate
            ? ecdsa.generateKey("P256")
            : existingSecret.data["tls.key"]}

    - id: csr
      def: ${shouldRegenerate
            ? x509.createCertificateRequest(key, {
                "subject": {"commonName": "my-app.default.svc"},
                "dnsNames": ["my-app", "my-app.default.svc"]
              })
            : existingSecret.data["tls.csr"]}

    - id: keySecret
      template:
        apiVersion: v1
        kind: Secret
        metadata:
          name: my-app-tls
        type: Opaque
        stringData:
          tls.key:      ${key}
          tls.csr:      ${csr}
          configHash:   ${configHash}
          rotateAfter:  ${shouldRegenerate
                          ? string(time.now() + duration("720h"))
                          : existingSecret.data["rotateAfter"]}
```

##### Dependencies and footprint

* No new third-party dependencies. Implementation uses only Go standard
  library packages: `crypto/rand`, `crypto/rsa`, `crypto/ecdsa`,
  `crypto/ed25519`, `crypto/elliptic`, `crypto/x509`,
  `crypto/x509/pkix`, `encoding/pem`.
* No changes to the compiler's environment-construction logic beyond
  appending to `customCELFunctions()`.

## Other solutions considered

##### `certificate.generateParams(config)` — single combined function

A single CEL function that takes a config map with `algorithm`, `size`,
`commonName`, `dnsNames` and returns `{privateKey, csr}` as a map.

Rejected: combines two independent operations (key generation and CSR
creation) into one function. This prevents reusing a key for a different
CSR, prevents generating a key without a CSR, and embeds algorithm
selection as a stringly-typed config field (runtime error on typo)
rather than as the function name (compile-time error). Extending to new
algorithms requires modifying the config schema instead of simply adding
a new function. The Go stdlib keeps these operations separate for good
reason.

##### Function takes existing key as input, passthrough internally

A design where the function signature is
`certificate.generateParams(existingKeyPEM, config)` and the function
parses the existing key, validates algorithm/size match, and returns it
unchanged when present.

Rejected: moves idempotency logic into the function (PEM parsing,
algorithm-mismatch validation, multi-branch behavior). This is caller
concern, not function concern. CEL ternaries in the Graph handle it
more simply and visibly.

##### In-process per-reconcile cache, generate on miss

A design where the function uses a closure-scoped cache keyed by
config. The cache is fresh per reconcile.

Rejected: controller processes are ephemeral. After restart or failover
the cache is empty and keys regenerate. Callers would need `includeWhen`
gates to prevent rotation — a footgun at scale.

##### Deterministic key from a seed

Derive private-key bytes from `hash(graphName + nodeID + namespace)`.

Rejected: anyone with read access to the Graph spec can recompute the
private key.

##### Defer to cert-manager

Tell users to install cert-manager and use its CRDs.

Rejected as the *only* option, because cert-manager is a heavyweight
dependency for users who want a small primitive. The experimental Graph
controller's stdlib aims to provide low-altitude composables; full
certificate lifecycle management is a different layer. Nothing in this
proposal precludes a Graph from feeding CSR output into a cert-manager
`CertificateRequest`.

## Scoping

#### What is in scope for this proposal?

* Five CEL functions: `rsa.generateKey`, `ecdsa.generateKey`,
  `ed25519.generateKey`, `x509.createCertificateRequest`,
  `crypto.publicKey`.
* Key sizes: RSA 2048/3072/4096, ECDSA P-256/P-384/P-521, Ed25519.
* PKCS#8 PEM encoding for all private keys, PKIX/SPKI PEM encoding for
  public keys.
* CSR template fields: `subject` (required map, with optional
  `commonName` and `organization`), `dnsNames` (optional),
  `ipAddresses` (optional).
* Error responses for invalid args.
* Unit tests covering each function.

#### What is not in scope?

* Additional algorithms (DSA, X25519, secp256k1, RSA-1024).
* Additional CSR template fields (`organizationalUnit`, `country`,
  `emailAddresses`, `uris`). Can be added as strict extensions.
* Certificate signing or issuance.
* Lifecycle concerns (rotation, drift detection, idempotency) — these
  are caller/Graph responsibilities.
* Promotion to the main controller's `pkg/cel/library`.
* Any new CRD.

## Testing strategy

#### Requirements

* No cluster, no envtest. Pure stdlib crypto + CEL binding; in-process
  unit tests using `cel.NewEnv`.
* No new test fixtures or harnesses.

#### Test plan

* **Key generation tests** — per function/parameter:
  * `rsa.generateKey(2048)`, `rsa.generateKey(3072)`, `rsa.generateKey(4096)`
  * `ecdsa.generateKey("P256")`, `ecdsa.generateKey("P384")`, `ecdsa.generateKey("P521")`
  * `ed25519.generateKey()`

  For each: output parses with `x509.ParsePKCS8PrivateKey`, concrete type
  and parameters match the request.

* **Public key extraction tests**:
  * RSA private key → valid PKIX public key PEM, parseable by
    `x509.ParsePKIXPublicKey`, type is `*rsa.PublicKey`.
  * ECDSA private key → valid PKIX public key PEM, type is
    `*ecdsa.PublicKey`, curve matches.
  * Ed25519 private key → valid PKIX public key PEM, type is
    `ed25519.PublicKey`.
  * Invalid PEM input → CEL error.
  * Non-PKCS#8 PEM (e.g., `RSA PRIVATE KEY` block) → CEL error.

* **CSR creation tests**:
  * Valid key + template with CN → parseable CSR, `Subject.CommonName`
    correct, `DNSNames` correct (including auto-added CN), signature
    verifies.
  * Template without CN but with `dnsNames` → valid CSR, empty CN,
    SANs populated.
  * Template with `ipAddresses` → parsed IPs in CSR's `IPAddresses`.
  * Template with `organization` → `Subject.Organization` populated.
  * Invalid key PEM → CEL error.
  * Missing `subject` → CEL error.
  * No identity (no CN, no dnsNames, no ipAddresses) → CEL error.
  * Invalid IP in `ipAddresses` → CEL error.

* **Validation error tests**:
  * `rsa.generateKey(1024)` → error.
  * `ecdsa.generateKey("secp256k1")` → error.
  * Unknown template keys → error.

* **Freshness**: two calls to the same key-gen function produce different
  keys (confirms fresh randomness).

* **Linting**: `make -C experimental presubmit` stays green.

## Discussion and notes

* **No memoization.** CEL's program cache caches compiled bytecode, not
  evaluation results. Every `Eval()` call executes function bindings
  fresh. There is no result-level caching in `cel-go`. Key generation
  functions produce fresh randomness on every call — the Graph's CEL
  ternary is what prevents them from being reached on steady-state
  reconciles. Key-gen expressions should be marked as volatile (like
  `time.now()`) so the skip-apply optimization doesn't suppress
  Secret updates when they fire.

* **Why mirror Go stdlib.** The function names map 1:1 to Go packages
  and functions. Go developers read the Graph and immediately know what's
  happening. Algorithm selection is structural (function name) rather
  than stringly-typed (config field) — typos in function names are
  compile-time CEL errors, not runtime surprises.

* **Separation of concerns.** Key generation and CSR creation are
  orthogonal. Splitting them means: reuse a key for multiple CSRs,
  generate a key without a CSR (e.g., for JWT signing), extend to new
  operations (`x509.parseCertificate`, `x509.createCertificate`) without
  redesigning the original function.

* **Why PKCS#8.** Algorithm-agnostic PEM envelope. Same
  `-----BEGIN PRIVATE KEY-----` block for RSA, ECDSA, and Ed25519. Consumers
  don't need conditional parsing logic.

* **Why include CommonName in SAN.** Modern TLS clients require hostname
  in SubjectAltName. Auto-adding it ensures CSRs work with current
  verifiers without extra caller effort.

* **`crypto.publicKey` naming.** Go has no single stdlib function for
  "private key PEM → public key PEM." It's a 3-step pattern:
  `crypto.Signer.Public()` + `x509.MarshalPKIXPublicKey` + PEM encode.
  We name it under `crypto` because that's the package defining the
  `Signer` interface with `.Public()`. It's a convenience wrapper, not
  a 1:1 mirror — acceptable because no single stdlib function exists.

* **Extensibility.** Future functions slot in naturally:
  * `x509.parseCertificate(pem)` — inspect a signed cert.
  * `x509.createCertificate(key, template)` — self-sign for testing.
  * `tls.x509KeyPair(cert, key)` — validate a pair matches.
  * Additional template fields (`emailAddresses`, `uris`, etc.)

  Each is a new registration or field addition, not a schema change to
  existing functions.

