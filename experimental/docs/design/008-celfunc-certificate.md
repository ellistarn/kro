# `certificate.generateParams` CEL function for the experimental Graph compiler

## Problem statement

Many Kubernetes workloads require a private key plus a matching Certificate
Signing Request (CSR) — TLS-terminated services, mutual TLS clients, signing
identities, and cluster-trust integrations. Today, a kro `Graph`
author has no in-graph way to produce these. They must either:

1. Pre-generate keys/CSRs out-of-band (manual steps, drift-prone), or
2. Compose with an external tool such as cert-manager (heavier — adds CRDs,
   issuers, and a separate controller surface)

A small, focused CEL primitive that produces `{privateKey, csr}` PEM strings would let authors declare cert-issuance flows directly inside a `Graph` without inventing a new CRD or pulling in cert-manager.

This proposal introduces a CEL function that generates the cryptographic
parameters required for certificate issuance. The kro Graph is responsible
for lifecycle concerns — idempotency, configuration drift detection, and
time-based rotation — expressed as standard CEL expressions.

## Proposal

Introduce a new CEL function `certificate.generateParams(config)` in the
experimental compiler's custom-function set
(`experimental/controller/compiler/celfuncs.go`). The function is a **pure
generator** — it always produces a fresh private key and CSR. Idempotency,
config-change detection, and time-based rotation are handled entirely in
CEL expressions within the Graph, not inside the function.

#### Overview

* **Signature**

  ```
  certificate.generateParams(
      config map<string, dyn>,
  ) -> map<string, string>   // { "privateKey": "<PEM>", "csr": "<PEM>" }
  ```

* **Behavior**

  * Always generates a fresh private key using the `algorithm` and `size`
    from `config`.
  * Builds a PKCS#10 CSR over the generated key using the subject
    information in `config`.
  * Returns both the private key and the CSR as PEM-encoded strings in a
    CEL map keyed `privateKey` and `csr`.

* **Where state lives**

The function has no Kubernetes client and no opinion on storage. It
returns PEM strings; the Graph author decides where to persist them
(Secret, ConfigMap, custom resource, or any other Kubernetes object).

* **Stateless by design**

  The function produces fresh key material on every invocation (fresh
  randomness). It has no internal state, no caching, and no awareness
  of prior calls. Callers are responsible for deciding when to invoke
  it and where to store the output.

* **Caller-managed lifecycle**

  Idempotency, config-change detection, time-based rotation, and
  multi-cert scaling are caller concerns — not function concerns.
  Example patterns are provided in the author-facing examples section
  below.


#### Design details

##### File and registration

* New function defined in `experimental/controller/compiler/celfuncs.go`
  alongside the other custom CEL functions
  (`celPluralFunction`, `celTimeNowFunction`, `celSimpleSchemaFunction`,
  `celConditionFunction`, etc.).
* Registered into every compilation environment by appending its
  `[]cel.EnvOption` to `customCELFunctions()` (currently around
  `celfuncs.go:364`). This places it on the same surface as `time.now()`
  and `simpleSchema.toOpenAPI()` — visible to the outer environment, the
  refined environment, and the forEach inner-scope environment, and to
  deferred-expression validation.

##### Function declaration

* CEL function name: `certificate.generateParams`.
* Single overload `certificate_generateParams_map`:
  * Args: `cel.DynType` (a CEL map — DynType because the map contains
    mixed value types: strings, integers, and `list<string>`).
  * Return: `cel.DynType` (a CEL map shaped `{privateKey: string,
    csr: string}`).
  * Binding: `cel.UnaryBinding`.

##### Config schema

The `config` map accepts:

| Key         | Type           | Required | Meaning |
|-------------|----------------|----------|---------|
| `commonName`| `string`       | yes      | CSR subject CommonName; also added to the SAN list. |
| `algorithm` | `string`       | yes      | Key algorithm. One of `"RSA"`, `"ECDSA"`, `"Ed25519"` (cert-manager / `x509.PublicKeyAlgorithm` casing). |
| `size`      | `int`          | yes      | Key size / curve parameter. Allowed values depend on `algorithm`. |
| `dnsNames`  | `list<string>` | no       | Additional DNS entries placed in the CSR's SubjectAltName extension. |

Allowed `(algorithm, size)` pairs:

| `algorithm` | Allowed `size` | Notes |
|-------------|---------------|-------|
| `"RSA"`     | `2048`, `3072`, `4096` | Bit length of modulus. |
| `"ECDSA"`   | `256`, `384`           | NIST P-256 / P-384 (curve bit size). P-521 omitted — rarely needed, less interoperable. |
| `"Ed25519"` | `256`                  | Ed25519 has a single fixed parameter set. `size: 256` is required for schema consistency; any other value is rejected. |

`algorithm` and `size` are deliberately **required** — there are no
defaults. A function that produces key material on the user's behalf
should not silently choose a security-relevant parameter for them. Every
Graph that calls `certificate.generateParams` must explicitly state which
algorithm and which parameter size it wants.

Other subject and SAN fields (`organization`, `organizationalUnit`,
`country`, `ipAddresses`, `emailAddresses`, `uris`) are intentionally
deferred. The function returns an explicit error if it encounters an
unknown key in `config`, so we can introduce additional keys later
without ambiguity over their prior meaning.

##### Implementation outline

* Convert `configVal` via `conversion.GoNativeType(...)` (the same helper
  used by `celSimpleSchemaFunction` and `celConditionFunction`).
* Validate config:
  * Require `commonName` (non-empty string), `algorithm` (string), and
    `size` (integer). Missing any required field → explicit CEL error.
  * Validate `algorithm ∈ {"RSA", "ECDSA", "Ed25519"}` (exact case).
  * Validate `(algorithm, size)` pair against the allowed matrix above.
  * If `dnsNames` is present, require it to be a `[]any` whose elements
    are all strings.
  * Reject unknown keys in `config` with a CEL error listing accepted
    keys.
* Generate the private key:
  * `algorithm == "RSA"`     → `rsa.GenerateKey(rand.Reader, size)`
  * `algorithm == "ECDSA"`   → `ecdsa.GenerateKey(curveFor(size), rand.Reader)` where `curveFor(256) = elliptic.P256()` and `curveFor(384) = elliptic.P384()`
  * `algorithm == "Ed25519"` → `ed25519.GenerateKey(rand.Reader)`
* Build the CSR template:
  * `Subject: pkix.Name{CommonName: cn}`.
  * `DNSNames: dedup(append([]string{cn}, dnsNames...))` — CommonName
    is added to SAN per current TLS verification practice (RFC 6125 /
    browser/Go behavior); deduplication avoids a redundant entry when
    the caller already lists `cn` in `dnsNames`.
  * Signature algorithm: chosen from the key type
    (`x509.SHA256WithRSA`, `x509.ECDSAWithSHA256` for P-256,
    `x509.ECDSAWithSHA384` for P-384, `x509.PureEd25519` for Ed25519).
* Call `x509.CreateCertificateRequest(rand.Reader, template, priv)`.
* PEM-encode:
  * Private key: PKCS#8 (`x509.MarshalPKCS8PrivateKey`) wrapped in a
    `PRIVATE KEY` block. PKCS#8 is algorithm-agnostic, so the same
    output shape covers RSA, ECDSA, and Ed25519 uniformly.
  * CSR: `CERTIFICATE REQUEST` block.
* Return `types.DefaultTypeAdapter.NativeToValue(map[string]any{
    "privateKey": privPEM, "csr": csrPEM })`.
* All error paths (missing/wrong-typed config, unknown config key, invalid
  `(algorithm, size)` pair, random-source failure) return
  `types.NewErr(...)` so the caller sees a clean CEL evaluation error
  attributable to the call site, as the existing custom functions do.

##### Author-facing example

###### Basic: generate once, passthrough on subsequent reconciles

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: my-app-cert
  namespace: default
spec:
  nodes:
    - id: existingSecret
      ref:
        apiVersion: v1
        kind: Secret
        name: my-app-tls

    - id: certParams
      def: ${existingSecret.?data["tls.key"].hasValue()
            ? {"privateKey": existingSecret.data["tls.key"],
               "csr":        existingSecret.data["tls.csr"]}
            : certificate.generateParams({
                "commonName": "my-app.default.svc",
                "algorithm":  "ECDSA",
                "size":       256
              })}

    - id: keySecret
      template:
        apiVersion: v1
        kind: Secret
        metadata:
          name: my-app-tls
        type: Opaque
        stringData:
          tls.key: ${certParams.privateKey}
          tls.csr: ${certParams.csr}

    - id: csr
      template:
        apiVersion: certificates.k8s.io/v1
        kind: CertificateSigningRequest
        metadata:
          name: my-app
        spec:
          request: ${base64.encode(certParams.csr)}
          signerName: kubernetes.io/kubelet-serving
          usages: ["server auth"]
```

###### With config-change detection and time-based rotation

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: my-app-cert
  namespace: default
spec:
  nodes:
    - id: existingSecret
      ref:
        apiVersion: v1
        kind: Secret
        name: my-app-tls

    - id: config
      def: ${{
        "commonName": "my-app.default.svc",
        "algorithm":  "ECDSA",
        "size":       256,
        "dnsNames":   ["my-app", "my-app.default.svc"]
      }}

    - id: configHash
      def: ${base64.encode(hash.sha256(
              config.commonName + "|" + config.algorithm + "|" + string(config.size)
            ))}

    - id: shouldRegenerate
      def: ${
        !existingSecret.?data["tls.key"].hasValue()
        || existingSecret.?data["configHash"].orValue("") != configHash
        || timestamp(existingSecret.?data["rotateAfter"].orValue("2099-01-01T00:00:00Z")) < time.now()
      }

    - id: certParams
      def: ${shouldRegenerate
            ? certificate.generateParams(config)
            : {"privateKey": existingSecret.data["tls.key"],
               "csr":        existingSecret.data["tls.csr"]}}

    - id: keySecret
      template:
        apiVersion: v1
        kind: Secret
        metadata:
          name: my-app-tls
        type: Opaque
        stringData:
          tls.key:      ${certParams.privateKey}
          tls.csr:      ${certParams.csr}
          configHash:   ${configHash}
          rotateAfter:  ${shouldRegenerate
                          ? string(time.now() + duration("720h"))
                          : existingSecret.data["rotateAfter"]}

    - id: csr
      template:
        apiVersion: certificates.k8s.io/v1
        kind: CertificateSigningRequest
        metadata:
          name: my-app
        spec:
          request: ${base64.encode(certParams.csr)}
          signerName: kubernetes.io/kubelet-serving
          usages: ["server auth"]
```

Regeneration triggers:
1. Secret absent (first deploy).
2. `configHash` mismatch (Graph author changed CN, algorithm, or size).
3. `rotateAfter` has passed (time-based rotation).

In-place rotation is safe when the signing CA is unchanged — clients
trust the CA, not specific leaf certs.

##### Dependencies and footprint

* No new third-party dependencies. Implementation uses only Go standard
  library packages: `crypto/rand`, `crypto/rsa`, `crypto/ecdsa`,
  `crypto/ed25519`, `crypto/elliptic`, `crypto/x509`,
  `crypto/x509/pkix`, `encoding/pem`.

## Other solutions considered


##### Function takes existing key as input, passthrough internally

A design where the function signature is
`certificate.generateParams(existingKeyPEM, config)` and the function
parses the existing key, validates algorithm/size match, and returns it
unchanged when present.

Rejected in favor of the simpler pure-generator design. Moving
idempotency logic into CEL ternaries in the Graph makes the function
trivial (no PEM parsing, no algorithm-mismatch validation, no
multi-branch behavior matrix). The ternary pattern is visible in the
Graph YAML, not hidden inside Go code. Testing becomes straightforward
(generate-only path). The function does one thing well.


##### Single-token key vocabulary (`keyType: "ecdsa-p256"`)

Use a single string field that combines algorithm and parameter, like
`keyType: "rsa-2048" | "rsa-3072" | "ecdsa-p256" | "ecdsa-p384" | "ed25519"`.

Rejected in favor of two separate fields (`algorithm` + `size`).
Cert-manager — the closest piece of prior art in the same ecosystem —
uses `algorithm` (`RSA` | `ECDSA` | `Ed25519`) and `size` (an integer)
as separate fields. Go's stdlib `x509.PublicKeyAlgorithm` enum uses the
same casing. Aligning with the existing convention reduces friction
for anyone who has used cert-manager and lets us evolve the two axes
independently.

##### Defaults for `algorithm` and `size`

Default the algorithm and size to a sensible choice so the caller can
omit them.

Rejected. A function that produces key material is making a
security-relevant decision. A silent default hides that decision from
the Graph author. Required fields force every caller to write the
algorithm choice into their YAML where it is reviewable and
version-controlled.

## Scoping

#### What is in scope for this proposal?

* New CEL function `certificate.generateParams(config)` registered
  globally for the experimental compiler.
* Three key algorithms with the following parameter sizes:
  * `RSA` — 2048, 3072, 4096
  * `ECDSA` — P-256, P-384
  * `Ed25519` — 256 (the only valid value)
* PKCS#8 PEM encoding for the private key (algorithm-agnostic envelope —
  same `-----BEGIN PRIVATE KEY-----` block header for all three
  algorithms). Standard `CERTIFICATE REQUEST` PEM block for the CSR.
* Four config fields: `commonName`, `algorithm`, `size` (all required),
  and `dnsNames` (optional).
* Error responses for malformed input: missing required fields,
  wrong-typed fields, invalid `(algorithm, size)` pair, non-string
  `commonName`, non-list `dnsNames`, non-string `dnsNames` element,
  unknown config keys.
* Unit tests covering each algorithm/size combination and the full error
  matrix.
* Documented Graph patterns for:
  * Basic idempotency via CEL ternary.
  * Config-change detection via `configHash`.
  * Time-based rotation via `rotateAfter` + `time.now()`.

#### What is not in scope?

* Algorithms outside the supported set (DSA, X25519, secp256k1, P-521,
  RSA-1024). They can be added later as strict extensions because the
  validation already rejects unknown values explicitly.
* Additional subject fields (`organization`, `organizationalUnit`,
  `country`, etc.) and additional SAN types (`ipAddresses`,
  `emailAddresses`, `uris`). The function rejects unknown keys today so
  these can be added later as a strict extension.
* Preference on consumer strategies on cert rotation or drift detection. 
* Promotion path to the main controller's CEL library

## Testing strategy

#### Requirements

* No cluster, no envtest. The function is pure stdlib crypto plus a
  CEL binding; everything can be exercised with in-process unit tests
  using a `cel.NewEnv` constructed from the function's
  `[]cel.EnvOption`. 
* No new test fixtures or harnesses.

#### Test plan

* **Unit tests** in a new file
  `experimental/controller/compiler/celfuncs_test.go` (or extending
  whichever existing test file in that package is closest in style):
  * **Fresh-generate, per algorithm/size pair** — six paths:
    * `RSA/2048`, `RSA/3072`, `RSA/4096`
    * `ECDSA/256`, `ECDSA/384`
    * `Ed25519/256`

    For each: `certificate.generateParams({commonName, algorithm,
    size})` returns a `privateKey` parseable by
    `x509.ParsePKCS8PrivateKey` whose concrete type matches the
    requested algorithm and whose parameters (modulus bit length /
    curve) match the requested size. The returned `csr` parses with
    `x509.ParseCertificateRequest`, has `Subject.CommonName` equal to
    the requested CN, signature verifies against the public key, and
    its public key matches the returned `privateKey`.
  * **`dnsNames` propagation**: verify all entries land in the CSR's
    SAN extension and `commonName` is also present in SAN exactly
    once (deduplication when the caller already lists `cn` in
    `dnsNames`).
  * **Validation errors** — each must return a CEL error, not panic:
    * Missing required field: each of `commonName`, `algorithm`, `size`
      omitted in turn.
    * Wrong-typed required field: `commonName` as int, `algorithm` as
      int, `size` as string.
    * Empty `commonName` (string but blank).
    * `algorithm` outside the accepted set (e.g., `"DSA"`, `"rsa"`
      lowercase).
    * Invalid `(algorithm, size)` pair: `RSA/256`, `RSA/1024`,
      `ECDSA/2048`, `ECDSA/521`, `Ed25519/512`.
    * `dnsNames` containing a non-string element.
    * Unknown key in `config` (e.g., `keyType`, `bits`).
  * **Two calls produce different keys**: invoke the function twice
    with the same config; assert the two `privateKey` values differ
    (confirms fresh randomness, not deterministic derivation).
* **Linting**: `make -C experimental presubmit` (vet + unit tests +
  `go mod tidy`) must remain green.

## Discussion and notes

* **Pure generator, idempotency in CEL.** The function always generates
  fresh key material. Whether to call it is decided by the Graph's CEL
  ternary expression, which checks the existing Secret. This separation
  keeps the function trivial (~50 lines of Go), pushes lifecycle logic
  into visible, reviewable YAML, and avoids hiding behavior inside the
  implementation.

* **Config-change detection via hash.** Storing a `configHash` in the
  Secret and comparing it on each reconcile lets the Graph auto-detect
  when the author changes algorithm, size, or commonName. This mirrors
  how cert-manager detects spec changes on its `Certificate` resource
  and triggers re-issuance — but here the logic is explicit in the
  Graph rather than hidden in a controller.

* **Time-based rotation via `rotateAfter`.** Storing a `rotateAfter`
  timestamp in the Secret and comparing against `time.now()` gives
  time-triggered rotation without any external cron or timer.
  `time.now()` is captured once per reconcile (consistent), and the
  controller's periodic resync ensures reconciles happen often enough
  to detect expiry within hours — well within any reasonable rotation
  window (typically 30–90 days).

* **In-place swap is safe when the CA is unchanged.** TLS clients trust
  the CA, not individual leaf certs. After rotation, the new cert is
  signed by the same CA → clients accept it. Established TLS sessions
  use symmetric keys negotiated during the original handshake and are
  unaffected by a cert swap. Session resumption failures fall back to
  a full handshake gracefully. The only unsafe rotation is CA rotation
  (different root) — that requires a multi-phase trust-bundle update,
  which is out of scope for this proposal.

* **Why include CommonName in SAN.** Modern TLS clients ignore the
  `Subject.CommonName` for hostname verification and require the
  hostname to be present in SubjectAltName. Putting `commonName` into
  both ensures the produced CSR is acceptable to current verifiers
  without additional work from the Graph author.

* **Why PKCS#8 for the private key.** PKCS#1 (`RSA PRIVATE KEY`)
  only fits RSA, and SEC 1 (`EC PRIVATE KEY`) only fits ECDSA. PKCS#8
  (`PRIVATE KEY`) is algorithm-agnostic — the same envelope holds RSA,
  ECDSA, and Ed25519, with the algorithm OID encoded inside. This lets
  the function emit the same PEM block header regardless of which
  algorithm the caller selected.

* **No defaults for `algorithm` and `size`.** A function that produces
  key material is making a security-relevant decision. Required fields
  force every caller to write the algorithm choice into their YAML
  where it is reviewable and version-controlled.

* **Promotion path to the main controller's CEL library.** If this
  function proves broadly useful, a follow-up proposal can move it
  into `pkg/cel/library/certificate.go` and register it in
  `pkg/cel/environment.go` `BaseDeclarations()`, where it would
  become available to the production `ResourceGraphDefinition`
  controller as well. That's a separate decision; this proposal does
  not commit to it.

