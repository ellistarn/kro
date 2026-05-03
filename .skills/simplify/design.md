# Simplify

Simplify is a static analysis tool that produces structured intelligence for LLM-driven refactoring
of Go codebases. It is not a linter. Linters produce pass/fail verdicts. Simplify produces a report
that tells an agent where to look, what to measure, and how to verify improvement.

The division of labor is explicit. The tool measures code structure and detects patterns. An LLM
reads the report alongside design documents and provides judgment — which concepts are surplus,
which are missing, what to fix first. The tool never reads design documents and never suggests fixes.

## Report

The tool produces a single report in two representations: JSON for programmatic consumption, and a
text summary designed for direct LLM consumption. Both representations contain identical information.
An LLM reads the text summary and knows what to do without parsing JSON.

The report has four sections: inventory, metrics, signals, and violations. Each section answers a
different question.

### Inventory

The inventory is a semantic catalogue of the codebase. It lists every package, every exported type
with its fields and methods (by name and signature), and every exported function with its full
signature. This is the concept vocabulary of the code.

An LLM compares the inventory to design documents to identify three classes of problems: surplus
concepts (code entities with no design counterpart), missing concepts (design entities with no code
counterpart), and misaligned concepts (code entities whose shape contradicts their design intent).
The tool provides the vocabulary; the LLM provides the judgment.

#### Inventory Schema

```json
{
  "packages": [
    {
      "path": "github.com/example/project/pkg/compiler",
      "types": [
        {
          "name": "Artifact",
          "kind": "struct",
          "fields": [
            {"name": "Programs", "type": "map[string]cel.Program"},
            {"name": "DAG", "type": "*graph.Graph"}
          ],
          "methods": [
            {"name": "Compile", "signature": "func (a *Artifact) Compile(ctx context.Context) error"},
            {"name": "IsStale", "signature": "func (a *Artifact) IsStale() bool"}
          ]
        }
      ],
      "functions": [
        {"name": "New", "signature": "func New(schema *openapi.Schema) *Compiler"}
      ]
    }
  ]
}
```

Fields and methods include unexported entries only when they belong to an exported type — the
inventory captures the full shape of types that cross package boundaries.

### Metrics

Metrics are per-package, per-struct, and per-function measurements. Each metric has a defined
threshold. A value exceeding its threshold is a violation.

#### Package Metrics

| Metric | Threshold | Definition |
|---|---|---|
| Fan-in | — | Count of packages within the analyzed module that import this package. Informational — no threshold. Higher values mean changes to this package propagate more widely. |
| Fan-out | >7 | Count of packages within the analyzed module that this package imports. Each import is a change-propagation path. |
| Exported symbols | — | Count of exported types, functions, variables, and constants. Informational — no threshold. |
| Untyped crossings | >0 | Count of exported functions or methods whose signature contains `any`, `interface{}`, `map[string]any`, or `*unstructured.Unstructured` (including through pointer indirection). Each is a type-safety gap at the package boundary. |

Fan-in and exported symbols have no threshold because their significance depends on the package's
role. A shared-types package with high fan-in and many exported symbols is correct. The same numbers
on a leaf package indicate a boundary problem. The LLM makes this judgment using design context.

#### Struct Metrics

| Metric | Threshold | Definition |
|---|---|---|
| Method count | >7 | Total methods (exported and unexported) with a receiver of this type. |
| LCOM4 | >1 | Lack of Cohesion of Methods, variant 4. Build an undirected graph where nodes are methods and edges connect methods that (a) access a common field through the receiver, or (b) one calls the other through the receiver. LCOM4 is the number of connected components. A value >1 means the type contains disconnected clusters that could be separate types. |
| Exported mutable fields | >0 | Count of exported fields whose type is a pointer, slice, map, or function. Each is state that external code can mutate without method mediation. Embedded fields use the embedded type name for export checking. |
| Constructor bypass | true | True when every field of the struct is exported, meaning the type's invariants (if any) can be bypassed through struct-literal construction. Only meaningful for types that have constructors with validation — the tool reports the fact; the LLM judges whether the type has invariants worth protecting. |

##### LCOM4 Counting Rules

1. Collect all fields of the struct, including embedded type names.
2. For each method, walk the AST body. A `receiver.FieldName` selector where `FieldName` is in the
   field set counts as a field access. A `receiver.MethodName` selector where `MethodName` is
   another method of the same type counts as a method call.
3. Build an undirected graph. Add an edge between methods *i* and *j* if they share any field
   access, or if one calls the other.
4. Count connected components using union-find.

Methods with no field access and no receiver calls form singleton components. This is intentional —
a method that touches no state has no cohesion relationship with the rest of the type.

#### Function Metrics

| Metric | Threshold | Definition |
|---|---|---|
| Parameter count | >4 | Count of parameters in the function signature. Each named parameter in a multi-name field counts separately (`a, b int` is 2). |
| Cyclomatic complexity | >10 | Start at 1. Add 1 for each: `if`, `for`, `range`, non-default `case`, non-default `comm`, `&&`, `||`. This counts independent execution paths. |
| Cognitive complexity | >15 | Measures nesting-weighted decision density. Unlike cyclomatic complexity, cognitive complexity penalizes nesting depth — a branch inside a branch costs more than two sequential branches. |

##### Cognitive Complexity Counting Rules

For each control structure (`if`, `for`, `range`, `switch`, `type switch`, `select`): add 1 plus
the current nesting depth. `else` and `else if` add 1 each (no nesting penalty — they continue a
chain, not deepen it). Logical operators `&&` and `||` in conditions add 1 each. `goto` adds 1.
Labeled `break` or `continue` adds 1. Function literals increase nesting depth by 1 for their body.

Nesting depth starts at 0 for the function body. Each control structure's body increments depth by 1
for its children.

### Signals

Signals are patterns that indicate structural problems but cannot be expressed as single-metric
threshold violations. They require cross-entity analysis.

#### Dangling Functions

Exported functions with zero callers within the analyzed scope. A dangling function is either dead
code or a public API entry point. The inventory enables an LLM to distinguish — entry points trace
to design concepts; dead code does not.

Detection: for each exported function, search all AST nodes in the analyzed packages for call
expressions or selector expressions that reference the function name on the declaring package's
import identifier.

#### Dangling Types

Exported types with zero references outside their defining package. Same interpretation as dangling
functions — either dead code or an unused public API.

Detection: for each exported type, search all AST nodes in packages other than the defining package
for identifiers or selector expressions matching the type name on the defining package's import
identifier.

#### Parameter Group Co-occurrence

Sets of parameter types that appear together in more than 2 function signatures. When the same group
of types flows through multiple function boundaries, it indicates an unnamed struct — the types are
a concept without a name.

Detection: for each function, extract the multiset of parameter types (excluding `context.Context`
and `error`). Find type-multisets that occur in >2 distinct function signatures. Report the type set
and the functions that share it.

#### Untyped Protocols

String literals used as keys in `map[string]any` access patterns across multiple files. When the
same string literal appears as a map key in different files, the string is acting as a protocol —
a shared agreement without type enforcement.

Detection: find string-literal keys in index expressions on map-typed or `any`-typed values. Group
by literal value. Report literals that appear in >1 file.

### Violations

Violations are all metric threshold exceedances, grouped by refactoring phase. Signals are reported
in a separate section of the output — they are not threshold violations and do not slot into phases.
An LLM reads both sections: violations tell it what exceeds thresholds, signals tell it what
patterns exist across entities.

The phases impose an ordering — structural defense problems are fixed before design alignment, which
is fixed before internal structure. This ordering exists because outer-layer fixes change the
boundaries that inner-layer fixes operate within.

#### Phase 1 — Structural Defense

Violations where wrong-layer modification is currently possible:

- Untyped crossings (threshold: >0)
- Exported mutable fields (threshold: >0)
- Constructor bypass (threshold: true, for types with invariants)
- Fan-out (threshold: >7)

#### Phase 2 — Design Alignment

This phase is not populated by the tool. The tool provides the inventory. An LLM compares the
inventory to design documents and populates this phase with surplus concepts, missing concepts, and
misaligned concepts. The tool's role is to make this comparison possible by providing a complete,
structured vocabulary.

#### Phase 3 — Internal Structure

Violations where the interior of a correct boundary is disorganized:

- LCOM4 (threshold: >1)
- Cognitive complexity (threshold: >15)
- Cyclomatic complexity (threshold: >10)
- Method count (threshold: >7)
- Parameter count (threshold: >4)

Within each phase, violations are ordered by severity — the metric value furthest from its threshold
appears first.

## CLI

```
simplify [flags] <package-pattern>...

Flags:
  -dir        Working directory for package resolution (default: current directory)
  -format     Output format: json, text (default: json)
  -threshold  Path to custom threshold config file (default: built-in thresholds)
```

Package patterns follow `go/packages` conventions: `./...` for all packages under the current
directory, `./pkg/compiler` for a single package, `./pkg/...` for a subtree.

The threshold config file is JSON mapping metric names to integer thresholds:

```json
{
  "fanOut": 7,
  "untypedCrossings": 0,
  "methods": 7,
  "lcom4": 1,
  "exportedMutableFields": 0,
  "params": 4,
  "cyclomatic": 10,
  "cognitive": 15
}
```

Omitted keys use built-in defaults. Unknown keys are rejected.

## Module Structure

```
.skills/simplify/
  design.md          — this document
  SKILL.md           — operational checklist that references the tool
  go.mod             — standalone Go module
  main.go            — entry point, CLI flags, output dispatch
  analyze.go         — package loading and orchestration
  metrics.go         — metric computations
  inventory.go       — semantic inventory construction
  signals.go         — cross-cutting pattern detection
  report.go          — output types and text/JSON formatting
```

The tool is split across focused files: entry point, analysis orchestration, metric computation,
inventory construction, signal detection, and output formatting. Each file owns one responsibility.

The module is standalone. It depends on `golang.org/x/tools/go/packages` for AST loading and
nothing else from the host project. This isolation means the tool never becomes a dependency of the
code it analyzes.

## Analysis Method

The tool uses `go/packages` to load syntax trees and type information. All analysis operates on the
AST. The tool does not use `go/types` for deep cross-reference resolution — AST-based analysis
covers the metrics and most signals without the complexity and performance cost of full type
resolution.

The trade-off is precision in cross-reference signals. Dangling function and dangling type detection
match on names and import identifiers, which can produce false positives when different packages
export identically-named symbols. This is acceptable — the signals are hints for LLM investigation,
not verdicts.

Generated files (containing "Code generated" and "DO NOT EDIT" in the first 3 lines) and test files
(`_test.go` suffix) are excluded from analysis.

## Boundaries

The tool does not modify code. It produces a report. The SKILL.md provides the fix vocabulary.

The tool does not read design documents. It produces an inventory that enables an LLM to compare
code against designs. The LLM provides judgment about surplus, missing, and misaligned concepts.

The tool does not suggest fixes. Each violation has a metric name, threshold, actual value, and
location. What to do about it is the LLM's decision, guided by the SKILL.md's fix vocabulary and
the project's design documents.

The tool does not track changes over time. Each invocation produces a snapshot. Temporal comparison
(did this metric improve?) is done by the LLM comparing two snapshots.

The tool does not analyze test code. Tests are consumers of the API, not part of the API's
structure.

## Rejected Alternatives

**golangci-lint plugin.** The plugin system produces pass/fail linting results. It cannot produce
semantic inventories or cross-cutting signals. The structured report format — inventory alongside
metrics alongside signals — is not expressible in the linter output contract.

**Multiple small tools.** One tool per metric (gocyclo, gocognit, etc.) requires multiple AST
loading passes over the same packages, cannot correlate signals across metrics (parameter
co-occurrence requires seeing all functions together), and produces fragmented output that an LLM
must synthesize from multiple sources. A single tool loads AST once and produces one coherent report.

**`go/types`-based deep analysis.** Full type resolution enables precise cross-reference analysis
(exact call graphs, accurate dangling detection) but is significantly more complex to implement and
slower to run. AST-based name matching covers metrics exactly and covers signals with acceptable
false-positive rates. `go/types` can be added incrementally for specific signals where precision
matters most — dangling detection benefits the most, since it currently relies on name matching
across import boundaries.
