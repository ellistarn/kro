# Simplify

Simplify is a static analysis tool for Go codebases. It is the Measure step of a
three-step loop: **Measure, Analyze, Simplify**.

Static analysis produces objective evidence — counts, thresholds, patterns, inventories.
It cannot judge whether complexity is justified. That requires reading the designs. An LLM
reads the tool's output alongside design documents and synthesizes three verdicts:
**alignment** (complexity the designs justify), **divergence** (complexity the designs do
not justify), and **gaps** (design concepts with no code counterpart).

The gap between what the tool finds and what the designs justify is accidental complexity.
The loop converges when all remaining complexity is design-justified.

The budget file is the loop's memory. Each iteration, the LLM records which complexity it
has justified and why. The budget accumulates, so the LLM does not re-derive justifications
on subsequent passes. A stable budget — one where no iteration adds or removes entries —
means the loop has converged.

## The Loop

```
Measure ──→ Analyze ──→ Simplify ──→ Measure ──→ ...
  tool        LLM         LLM          tool
```

**Measure.** Run the tool. It produces a report: inventory, metrics, signals, and budget
attribution. This is evidence, not judgment.

**Analyze.** The LLM reads the report alongside the project's design documents. It
classifies every piece of measured complexity into a category (see Complexity Categories
below). Complexity that maps to a design concept is alignment. Complexity that does not is
divergence. Design concepts with no code counterpart are gaps. The LLM updates the budget
file with newly justified complexity.

**Simplify.** The LLM acts on the divergences — removes unnecessary mechanisms, tightens
types, extracts or inlines code. One structural change per iteration. Compile and test.

**Convergence.** The loop terminates when Analyze produces no divergences. At that point,
every piece of measured complexity traces to a design concept or to language mechanics, and
the budget file is stable.

## Report

The tool produces a single report in two representations: JSON for programmatic consumption
and a text summary for direct LLM consumption. Both contain identical information. The
report has four sections, each providing evidence for the Analyze step.

### Inventory

The inventory is a semantic catalogue of the codebase. It lists every package, every type
with its fields and methods (by name and signature), and every function with its full
signature. This is the code's concept vocabulary.

The Analyze step compares this vocabulary against the design vocabulary to find three
classes of problems: surplus concepts (code entities with no design counterpart), missing
concepts (design entities with no code counterpart), and misaligned concepts (code entities
whose shape contradicts their design intent). The tool provides the vocabulary; the LLM
provides the judgment.

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

Fields and methods include unexported entries when they belong to an exported type — the
inventory captures the full shape of types that cross package boundaries.

### Metrics

Metrics are per-package, per-struct, and per-function measurements. Each metric has a
defined threshold. A value exceeding its threshold is a violation that directs the Analyze
step's attention. See Metric Definitions for precise counting rules.

### Signals

Signals are patterns that indicate structural problems but cannot be expressed as
single-metric threshold violations. They require cross-entity analysis.

**Dangling functions.** Exported functions with zero callers within the analyzed scope.
Either dead code or a public API entry point. The inventory enables the Analyze step to
distinguish — entry points trace to design concepts; dead code does not.

**Dangling types.** Exported types with zero references outside their defining package.
Same interpretation as dangling functions.

**Parameter group co-occurrence.** Sets of parameter types that appear together in more
than 2 function signatures. When the same group of types flows through multiple function
boundaries, it indicates an unnamed concept — a struct that does not yet exist.

### Budget Attribution

When invoked with a budget file, the tool classifies every branch in high-complexity
functions (cyclomatic > 5) into three categories:

- **Attributed** — the branch condition contains a keyword from a budget concept. The
  branch traces to a design-mandated decision.
- **Language-essential** — the branch matches a language-mechanical pattern (`err != nil`,
  `!ok`, nil checks on non-concept values). Go ceremony.
- **Unattributed** — neither attributed nor language-essential. This is the Analyze step's
  primary focus: each unattributed branch is a divergence candidate.

The tool reports per-function breakdowns (attributed count, language-essential count,
unattributed count with source text) and a summary across all analyzed functions. Partial
keyword matches on unattributed branches surface near-misses that may indicate missing
budget keywords.

## Complexity Categories

The Analyze step classifies measured complexity into these categories. The first three are
justified. The last three are divergence signals — targets for the Simplify step.

**Domain.** Design-mandated complexity. A branch that dispatches on node types exists
because the design defines five node types. Identified by budget attribution: the branch
condition matches a concept keyword, and the budget traces that concept to a design
document. Justified.

**Language.** Go ceremony. Error checks, nil guards on values the type system cannot
exclude, ok-idiom checks on map and type-assertion results. Identified by
language-essential patterns in the budget report. Justified.

**Integration.** External system boundary complexity. Branches that handle Kubernetes API
semantics, serialization edge cases, or library contract requirements. Identified by
external package references in branch conditions. Partially justified — the branches are
necessary given the current boundary, but may be reducible with better boundary
abstractions.

**Type permissiveness.** Branches that exist because the type permits invalid states. Nil
checks on values that should never be nil. Type assertions on `any` values that should
have concrete types. Map-key guards on values that should be struct fields. Identified by
branch conditions that guard against states a tighter type would prevent. Divergence
signal — the fix is to tighten the type, which eliminates the branches.

**Unnecessary mechanisms.** Clusters of branches serving a pattern the design does not
require. A hand-rolled state machine where an enum would suffice. A registry where direct
calls would work. Identified by groups of unattributed branches that share a common
pattern but trace to no design concept. The highest-value simplification target. Divergence
signal.

**Surplus.** Code entities (types, functions, packages) with no design counterpart.
Identified by comparing the inventory against design documents. Not every surplus entity is
wrong — infrastructure code (logging, testing, error handling) earns its place through the
design concepts it serves. But surplus entities that serve no design concept are dead weight.
Divergence signal.

## Metric Definitions

### Package Metrics

| Metric | Threshold | Definition |
|---|---|---|
| Fan-in | — | Count of packages within the analyzed module that import this package. Informational — no threshold. Higher values mean changes propagate more widely. |
| Fan-out | >7 | Count of packages within the analyzed module that this package imports. Each import is a change-propagation path. |
| Exported symbols | — | Count of exported types, functions, variables, and constants. Informational — no threshold. |
| Untyped crossings | >0 | Count of exported functions or methods whose signature contains `any`, `interface{}`, `map[string]any`, or `*unstructured.Unstructured` (including through pointer indirection). Each is a type-safety gap at the package boundary. |

Fan-in and exported symbols have no threshold because their significance depends on the
package's role. A shared-types package with high fan-in and many exported symbols is
correct. The same numbers on a leaf package indicate a boundary problem. The Analyze step
makes this judgment using design context.

### Struct Metrics

| Metric | Threshold | Definition |
|---|---|---|
| Method count | >7 | Total methods (exported and unexported) with a receiver of this type. |
| LCOM4 | >1 | Lack of Cohesion of Methods, variant 4. Build an undirected graph where nodes are methods and edges connect methods that (a) access a common field through the receiver, or (b) one calls the other through the receiver. LCOM4 is the number of connected components. A value >1 means the type contains disconnected clusters that could be separate types. |
| Exported mutable fields | >0 | Count of exported fields whose type is a pointer, slice, map, or function. Each is state that external code can mutate without method mediation. Embedded fields use the embedded type name for export checking. |
| Constructor bypass | true | True when every field of the struct is exported, meaning the type's invariants (if any) can be bypassed through struct-literal construction. Only meaningful for types that have constructors with validation — the tool reports the fact; the Analyze step judges whether the type has invariants worth protecting. |

#### LCOM4 Counting Rules

1. Collect all fields of the struct, including embedded type names.
2. For each method, walk the AST body. A `receiver.FieldName` selector where `FieldName`
   is in the field set counts as a field access. A `receiver.MethodName` selector where
   `MethodName` is another method of the same type counts as a method call.
3. Build an undirected graph. Add an edge between methods *i* and *j* if they share any
   field access, or if one calls the other.
4. Count connected components using union-find.

Methods with no field access and no receiver calls form singleton components. This is
intentional — a method that touches no state has no cohesion relationship with the rest of
the type.

### Function Metrics

| Metric | Threshold | Definition |
|---|---|---|
| Parameter count | >4 | Count of parameters in the function signature. Each named parameter in a multi-name field counts separately (`a, b int` is 2). |
| Cyclomatic complexity | >10 | Start at 1. Add 1 for each: `if`, `for`, `range`, non-default `case`, non-default `comm`, `&&`, `||`. This counts independent execution paths. |
| Cognitive complexity | >15 | Measures nesting-weighted decision density. Unlike cyclomatic complexity, cognitive complexity penalizes nesting depth — a branch inside a branch costs more than two sequential branches. |

#### Cognitive Complexity Counting Rules

For each control structure (`if`, `for`, `range`, `switch`, `type switch`, `select`): add
1 plus the current nesting depth. `else` and `else if` add 1 each (no nesting penalty —
they continue a chain, not deepen it). Logical operators `&&` and `||` in conditions add 1
each. `goto` adds 1. Labeled `break` or `continue` adds 1. Function literals increase
nesting depth by 1 for their body.

Nesting depth starts at 0 for the function body. Each control structure's body increments
depth by 1 for its children.

## CLI

```
simplify [flags] <package-pattern>...

Flags:
  -dir      Working directory for package resolution (default: current directory)
  -format   Output format: json, text (default: json)
  -budget   Path to YAML budget file for complexity attribution
```

Package patterns follow `go/packages` conventions: `./...` for all packages under the
current directory, `./pkg/compiler` for a single package, `./pkg/...` for a subtree.

## Module Structure

```
.skills/simplify/
  design.md          — this document
  SKILL.md           — operational checklist for the Analyze and Simplify steps
  go.mod             — standalone Go module
  main.go            — entry point, CLI flags, output dispatch
  analyze.go         — package loading and orchestration
  metrics.go         — metric computations
  inventory.go       — semantic inventory construction
  signals.go         — cross-cutting pattern detection
  budget.go          — budget loading, branch extraction, attribution analysis
  report.go          — output types and text/JSON formatting
  budgets/           — budget files per analysis target
```

The module is standalone. It depends on `golang.org/x/tools/go/packages` for AST loading
and `gopkg.in/yaml.v3` for budget file parsing. Nothing else from the host project. This
isolation means the tool never becomes a dependency of the code it analyzes.

## Analysis Method

The tool uses `go/packages` to load syntax trees and type information. All analysis
operates on the AST. The tool does not use `go/types` for deep cross-reference resolution
— AST-based analysis covers the metrics and most signals without the complexity and
performance cost of full type resolution.

The trade-off is precision in cross-reference signals. Dangling function and dangling type
detection match on names and import identifiers, which can produce false positives when
different packages export identically-named symbols. This is acceptable — the signals are
hints for LLM investigation, not verdicts.

Generated files (containing "Code generated" and "DO NOT EDIT" in the first 3 lines) and
test files (`_test.go` suffix) are excluded from analysis.

## Boundaries

The tool does not read design documents. It produces an inventory and metrics that enable
comparison against designs. The comparison is the Analyze step's responsibility.

The tool does not judge whether complexity is justified. It classifies branches
mechanically — by keyword match (attributed), by pattern match (language-essential), or by
exclusion (unattributed). The LLM decides what each classification means.

The tool does not suggest fixes. Each violation has a metric name, threshold, actual value,
and location. What to do about it is the Simplify step's decision, guided by the SKILL.md.

The tool does not modify code. It produces a report. The SKILL.md provides the fix
vocabulary.

The Analyze and Simplify steps are the LLM's responsibility, guided by the SKILL.md.

## Rejected Alternatives

**golangci-lint plugin.** The plugin system produces pass/fail linting results. It cannot
produce semantic inventories or cross-cutting signals. The structured report format —
inventory alongside metrics alongside signals — is not expressible in the linter output
contract.

**Multiple small tools.** One tool per metric (gocyclo, gocognit, etc.) requires multiple
AST loading passes over the same packages, cannot correlate signals across metrics
(parameter co-occurrence requires seeing all functions together), and produces fragmented
output that an LLM must synthesize from multiple sources. A single tool loads AST once and
produces one coherent report.

**`go/types`-based deep analysis.** Full type resolution enables precise cross-reference
analysis (exact call graphs, accurate dangling detection) but is significantly more complex
to implement and slower to run. AST-based name matching covers metrics exactly and covers
signals with acceptable false-positive rates. `go/types` can be added incrementally for
specific signals where precision matters most — dangling detection benefits the most, since
it currently relies on name matching across import boundaries.

**Automated design-code matching.** Keyword matching in budget files handles the
incremental case — attributing branches to already-justified concepts without re-reading
designs. But the initial classification and the holistic picture (surplus entities, missing
concepts, misaligned shapes) require LLM synthesis against the full design documents.
Automated keyword matching cannot detect that a type's shape contradicts its design intent,
or that a cluster of functions serves a pattern the design never required. The budget
handles the cached case; the LLM handles the hard case.
