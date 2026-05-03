package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// --- Output types ---

type Report struct {
	Dir        string           `json:"dir"`
	Timestamp  string           `json:"timestamp"`
	Packages   []PackageMetrics `json:"packages"`
	Signals    Signals          `json:"signals"`
	Violations PhasedViolations `json:"violations"`
	Budget     *BudgetReport    `json:"budget,omitempty"`
}

// --- Budget attribution types ---

type BudgetReport struct {
	Functions []FunctionAttribution `json:"functions"`
	Summary   BudgetSummary         `json:"summary"`
}

type FunctionAttribution struct {
	Name              string               `json:"name"`
	File              string               `json:"file"`
	Line              int                  `json:"line"`
	Receiver          string               `json:"receiver,omitempty"`
	Cyclomatic        int                  `json:"cyclomatic"`
	Attributed        []ConceptAttribution `json:"attributed"`
	LanguageEssential []LanguageBranch     `json:"languageEssential"`
	Unattributed      []UnattributedBranch `json:"unattributed"`
	Summary           AttributionSummary   `json:"summary"`
}

type ConceptAttribution struct {
	Concept  string   `json:"concept"`
	Count    int      `json:"count"`
	Keywords []string `json:"keywords"`
	Lines    []int    `json:"lines"`
}

type LanguageBranch struct {
	Pattern string `json:"pattern"`
	Count   int    `json:"count"`
	Lines   []int  `json:"lines"`
}

type UnattributedBranch struct {
	Line           int      `json:"line"`
	Text           string   `json:"text"`
	PartialMatches []string `json:"partialMatches,omitempty"`
}

type AttributionSummary struct {
	Attributed        int     `json:"attributed"`
	LanguageEssential int     `json:"languageEssential"`
	Unattributed      int     `json:"unattributed"`
	AttributedRatio   float64 `json:"attributedRatio"`
	UnattributedRatio float64 `json:"unattributedRatio"`
}

type BudgetSummary struct {
	FunctionsAnalyzed   int    `json:"functionsAnalyzed"`
	FullyAttributed     int    `json:"fullyAttributed"`
	HighestUnattributed string `json:"highestUnattributed"`
	TotalAttributed     int    `json:"totalAttributed"`
	TotalLanguage       int    `json:"totalLanguage"`
	TotalUnattributed   int    `json:"totalUnattributed"`
}

type PackageMetrics struct {
	Path             string           `json:"path"`
	FanIn            int              `json:"fanIn"`
	FanOut           int              `json:"fanOut"`
	ExportedSymbols  int              `json:"exportedSymbols"`
	UntypedCrossings int              `json:"untypedCrossings"`
	Structs          []StructMetrics  `json:"structs"`
	Functions        []FunctionMetrics `json:"functions"`
	Inventory        *PackageInventory `json:"inventory,omitempty"`
}

type StructMetrics struct {
	Name                  string `json:"name"`
	Exported              bool   `json:"exported"`
	File                  string `json:"file"`
	Line                  int    `json:"line"`
	Methods               int    `json:"methods"`
	ExportedMutableFields int    `json:"exportedMutableFields"`
	ConstructorBypass     bool   `json:"constructorBypass"`
	LCOM4                 int    `json:"lcom4"`
}

type FunctionMetrics struct {
	Name       string `json:"name"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Receiver   string `json:"receiver"`
	Params     int    `json:"params"`
	Returns    int    `json:"returns"`
	Cyclomatic int    `json:"cyclomatic"`
	Cognitive  int    `json:"cognitive"`
}

// --- Inventory types ---

type PackageInventory struct {
	Path      string     `json:"path"`
	Types     []TypeInfo `json:"types,omitempty"`
	Functions []FuncInfo `json:"functions,omitempty"`
}

type TypeInfo struct {
	Name     string       `json:"name"`
	Kind     string       `json:"kind"`
	Exported bool         `json:"exported"`
	File     string       `json:"file"`
	Line     int          `json:"line"`
	Fields   []FieldInfo  `json:"fields,omitempty"`
	Methods  []MethodInfo `json:"methods,omitempty"`
}

type FieldInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Exported bool   `json:"exported"`
}

type MethodInfo struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`
	Exported  bool   `json:"exported"`
}

type FuncInfo struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`
	Exported  bool   `json:"exported"`
	File      string `json:"file"`
	Line      int    `json:"line"`
}

// --- Signals types ---

type Signals struct {
	DanglingFunctions []DanglingSymbol `json:"danglingFunctions,omitempty"`
	DanglingTypes     []DanglingSymbol `json:"danglingTypes,omitempty"`
	ParameterGroups   []ParameterGroup `json:"parameterGroups,omitempty"`
}

type DanglingSymbol struct {
	Name    string `json:"name"`
	Package string `json:"package"`
	File    string `json:"file"`
	Line    int    `json:"line"`
}

type ParameterGroup struct {
	Types       []string `json:"types"`
	Occurrences int      `json:"occurrences"`
	Functions   []string `json:"functions"`
}

// --- Phased violations ---

type Violation struct {
	Metric    string `json:"metric"`
	Threshold int    `json:"threshold"`
	Actual    int    `json:"actual"`
	Package   string `json:"package"`
	Name      string `json:"name"`
	File      string `json:"file"`
	Line      int    `json:"line"`
}

type PhasedViolations struct {
	StructuralDefense []Violation `json:"structuralDefense"`
	InternalStructure []Violation `json:"internalStructure"`
}

// --- Violation computation ---

func computeViolations(pkgs []PackageMetrics) PhasedViolations {
	var pv PhasedViolations

	for _, pkg := range pkgs {
		if pkg.FanOut > 7 {
			pv.StructuralDefense = append(pv.StructuralDefense, Violation{
				Metric:    "fanOut",
				Threshold: 7,
				Actual:    pkg.FanOut,
				Package:   pkg.Path,
				Name:      pkg.Path,
			})
		}
		if pkg.UntypedCrossings > 0 {
			pv.StructuralDefense = append(pv.StructuralDefense, Violation{
				Metric:    "untypedCrossings",
				Threshold: 0,
				Actual:    pkg.UntypedCrossings,
				Package:   pkg.Path,
				Name:      pkg.Path,
			})
		}

		for _, s := range pkg.Structs {
			if s.ExportedMutableFields > 0 {
				pv.StructuralDefense = append(pv.StructuralDefense, Violation{
					Metric:    "exportedMutableFields",
					Threshold: 0,
					Actual:    s.ExportedMutableFields,
					Package:   pkg.Path,
					Name:      s.Name,
					File:      s.File,
					Line:      s.Line,
				})
			}
			if s.ConstructorBypass {
				pv.StructuralDefense = append(pv.StructuralDefense, Violation{
					Metric:    "constructorBypass",
					Threshold: 0,
					Actual:    1,
					Package:   pkg.Path,
					Name:      s.Name,
					File:      s.File,
					Line:      s.Line,
				})
			}
			if s.Methods > 7 {
				pv.InternalStructure = append(pv.InternalStructure, Violation{
					Metric:    "methods",
					Threshold: 7,
					Actual:    s.Methods,
					Package:   pkg.Path,
					Name:      s.Name,
					File:      s.File,
					Line:      s.Line,
				})
			}
			if s.LCOM4 > 1 {
				pv.InternalStructure = append(pv.InternalStructure, Violation{
					Metric:    "lcom4",
					Threshold: 1,
					Actual:    s.LCOM4,
					Package:   pkg.Path,
					Name:      s.Name,
					File:      s.File,
					Line:      s.Line,
				})
			}
		}

		for _, f := range pkg.Functions {
			name := f.Name
			if f.Receiver != "" {
				name = f.Receiver + "." + f.Name
			}
			if f.Params > 4 {
				pv.InternalStructure = append(pv.InternalStructure, Violation{
					Metric:    "params",
					Threshold: 4,
					Actual:    f.Params,
					Package:   pkg.Path,
					Name:      name,
					File:      f.File,
					Line:      f.Line,
				})
			}
			if f.Cyclomatic > 10 {
				pv.InternalStructure = append(pv.InternalStructure, Violation{
					Metric:    "cyclomatic",
					Threshold: 10,
					Actual:    f.Cyclomatic,
					Package:   pkg.Path,
					Name:      name,
					File:      f.File,
					Line:      f.Line,
				})
			}
			if f.Cognitive > 15 {
				pv.InternalStructure = append(pv.InternalStructure, Violation{
					Metric:    "cognitive",
					Threshold: 15,
					Actual:    f.Cognitive,
					Package:   pkg.Path,
					Name:      name,
					File:      f.File,
					Line:      f.Line,
				})
			}
		}
	}

	return pv
}

// --- Text formatter ---

func formatText(w io.Writer, r *Report) {
	totalViolations := len(r.Violations.StructuralDefense) + len(r.Violations.InternalStructure)
	fmt.Fprintf(w, "=== Simplify Report ===\n")
	fmt.Fprintf(w, "Dir: %s\n", r.Dir)
	fmt.Fprintf(w, "Packages: %d\n", len(r.Packages))
	fmt.Fprintf(w, "Total violations: %d\n", totalViolations)
	fmt.Fprintln(w)

	for _, pkg := range r.Packages {
		formatPackageText(w, &pkg)
	}

	// Signals.
	formatSignalsText(w, &r.Signals)

	// Budget attribution.
	if r.Budget != nil {
		formatBudgetText(w, r.Budget)
	}

	// Violations by phase.
	formatViolationsText(w, &r.Violations)
}

func formatPackageText(w io.Writer, pkg *PackageMetrics) {
	warn := ""
	if pkg.UntypedCrossings > 0 {
		warn = fmt.Sprintf(" ⚠ untyped crossings: %d", pkg.UntypedCrossings)
	}
	fmt.Fprintf(w, "=== Package: %s ===\n", pkg.Path)
	fmt.Fprintf(w, "Fan-in: %d | Fan-out: %d | Exported: %d%s\n", pkg.FanIn, pkg.FanOut, pkg.ExportedSymbols, warn)
	fmt.Fprintln(w)

	// Types from inventory.
	if pkg.Inventory != nil && len(pkg.Inventory.Types) > 0 {
		// Separate exported and unexported types.
		var exported, unexported []TypeInfo
		for _, t := range pkg.Inventory.Types {
			if t.Exported {
				exported = append(exported, t)
			} else {
				unexported = append(unexported, t)
			}
		}

		fmt.Fprintln(w, "Types:")
		for _, t := range exported {
			formatTypeEntry(w, t, pkg)
		}
		if len(unexported) > 0 {
			fmt.Fprintln(w, "  unexported:")
			for _, t := range unexported {
				formatTypeEntry(w, t, pkg)
			}
		}
		fmt.Fprintln(w)
	}

	// Functions with violations only.
	hasViolatingFuncs := false
	for _, f := range pkg.Functions {
		if f.Cyclomatic > 10 || f.Cognitive > 15 || f.Params > 4 {
			if !hasViolatingFuncs {
				fmt.Fprintln(w, "Functions (violations only):")
				hasViolatingFuncs = true
			}
			name := f.Name
			if f.Receiver != "" {
				name = f.Receiver + "." + f.Name
			}
			warnings := []string{}
			warnings = append(warnings, fmt.Sprintf("params=%d", f.Params))
			if f.Cyclomatic > 10 {
				warnings = append(warnings, fmt.Sprintf("cyclo=%d ⚠", f.Cyclomatic))
			}
			if f.Cognitive > 15 {
				warnings = append(warnings, fmt.Sprintf("cognitive=%d ⚠", f.Cognitive))
			}
			fmt.Fprintf(w, "  %s (%s:%d) — %s\n", name, f.File, f.Line, strings.Join(warnings, ", "))
		}
	}
	if hasViolatingFuncs {
		fmt.Fprintln(w)
	}
}

func formatTypeEntry(w io.Writer, t TypeInfo, pkg *PackageMetrics) {
	// Find matching struct metrics for violation indicators.
	var sm *StructMetrics
	for i := range pkg.Structs {
		if pkg.Structs[i].Name == t.Name {
			sm = &pkg.Structs[i]
			break
		}
	}

	warnings := []string{}
	if sm != nil {
		if sm.LCOM4 > 1 {
			warnings = append(warnings, fmt.Sprintf("LCOM4=%d ⚠", sm.LCOM4))
		}
		if sm.ExportedMutableFields > 0 {
			warnings = append(warnings, fmt.Sprintf("%d mutable fields ⚠", sm.ExportedMutableFields))
		}
		if sm.ConstructorBypass {
			warnings = append(warnings, "constructor bypass ⚠")
		}
	}

	warnStr := ""
	if len(warnings) > 0 {
		warnStr = " — " + strings.Join(warnings, ", ")
	}

	methodCount := len(t.Methods)
	fmt.Fprintf(w, "  %s (%s:%d) — %d methods%s\n", t.Name, t.File, t.Line, methodCount, warnStr)

	// Fields summary.
	if len(t.Fields) > 0 {
		fieldStrs := make([]string, 0, len(t.Fields))
		for _, f := range t.Fields {
			fieldStrs = append(fieldStrs, f.Name+" "+f.Type)
		}
		fieldLine := strings.Join(fieldStrs, ", ")
		if len(fieldLine) > 120 {
			fieldLine = fieldLine[:117] + "..."
		}
		fmt.Fprintf(w, "    Fields: %s\n", fieldLine)
	}

	// Methods summary.
	if len(t.Methods) > 0 {
		methodStrs := make([]string, 0, len(t.Methods))
		for _, m := range t.Methods {
			sig := strings.TrimPrefix(m.Signature, "func ")
			methodStrs = append(methodStrs, m.Name+sig)
		}
		methodLine := strings.Join(methodStrs, ", ")
		if len(methodLine) > 120 {
			methodLine = methodLine[:117] + "..."
		}
		fmt.Fprintf(w, "    Methods: %s\n", methodLine)
	}
}

func formatSignalsText(w io.Writer, s *Signals) {
	if len(s.ParameterGroups) == 0 && len(s.DanglingFunctions) == 0 && len(s.DanglingTypes) == 0 {
		return
	}

	fmt.Fprintln(w, "=== Signals ===")

	if len(s.ParameterGroups) > 0 {
		fmt.Fprintln(w, "Parameter groups:")
		for _, pg := range s.ParameterGroups {
			fmt.Fprintf(w, "  [%s] appears in %d functions\n", strings.Join(pg.Types, ", "), pg.Occurrences)
			fmt.Fprintf(w, "    %s\n", strings.Join(pg.Functions, ", "))
		}
		fmt.Fprintln(w)
	}

	if len(s.DanglingFunctions) > 0 {
		fmt.Fprintln(w, "Dangling exported functions:")
		for _, d := range s.DanglingFunctions {
			fmt.Fprintf(w, "  %s (%s:%d) — 0 cross-file references\n", d.Name, d.File, d.Line)
		}
		fmt.Fprintln(w)
	}

	if len(s.DanglingTypes) > 0 {
		fmt.Fprintln(w, "Dangling exported types:")
		for _, d := range s.DanglingTypes {
			fmt.Fprintf(w, "  %s (%s:%d) — 0 cross-file references\n", d.Name, d.File, d.Line)
		}
		fmt.Fprintln(w)
	}
}

func formatViolationsText(w io.Writer, v *PhasedViolations) {
	fmt.Fprintln(w, "=== Violations by Phase ===")

	if len(v.StructuralDefense) > 0 {
		fmt.Fprintf(w, "Structural Defense (%d):\n", len(v.StructuralDefense))
		// Sort by severity (actual value descending).
		sorted := make([]Violation, len(v.StructuralDefense))
		copy(sorted, v.StructuralDefense)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Actual > sorted[j].Actual
		})
		for i, viol := range sorted {
			loc := ""
			if viol.File != "" {
				loc = fmt.Sprintf(" (%s:%d)", viol.File, viol.Line)
			}
			fmt.Fprintf(w, "  %d. %s: %s=%d (threshold %d)%s\n",
				i+1, viol.Name, viol.Metric, viol.Actual, viol.Threshold, loc)
		}
		fmt.Fprintln(w)
	}

	if len(v.InternalStructure) > 0 {
		fmt.Fprintf(w, "Internal Structure (%d):\n", len(v.InternalStructure))
		sorted := make([]Violation, len(v.InternalStructure))
		copy(sorted, v.InternalStructure)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Actual > sorted[j].Actual
		})
		for i, viol := range sorted {
			loc := ""
			if viol.File != "" {
				loc = fmt.Sprintf(" (%s:%d)", viol.File, viol.Line)
			}
			fmt.Fprintf(w, "  %d. %s: %s=%d (threshold %d)%s\n",
				i+1, viol.Name, viol.Metric, viol.Actual, viol.Threshold, loc)
		}
		fmt.Fprintln(w)
	}
}

func formatBudgetText(w io.Writer, b *BudgetReport) {
	fmt.Fprintln(w, "=== Budget Attribution ===")
	fmt.Fprintln(w)

	for _, fa := range b.Functions {
		name := fa.Name
		if fa.Receiver != "" {
			name = fa.Receiver + "." + fa.Name
		}

		if fa.Summary.Unattributed == 0 {
			// Fully attributed — one-line summary.
			fmt.Fprintf(w, "%s (%s:%d) — cyclomatic: %d — fully attributed (%d attributed, %d language)\n",
				name, fa.File, fa.Line, fa.Cyclomatic,
				fa.Summary.Attributed, fa.Summary.LanguageEssential)
			continue
		}

		fmt.Fprintf(w, "%s (%s:%d) — cyclomatic: %d\n", name, fa.File, fa.Line, fa.Cyclomatic)

		// Attributed concepts.
		if len(fa.Attributed) > 0 {
			fmt.Fprintln(w, "  Attributed:")
			for _, ca := range fa.Attributed {
				fmt.Fprintf(w, "    %-30s %d  %s\n", ca.Concept+":", ca.Count, "["+strings.Join(ca.Keywords, ", ")+"]")
			}
		}

		// Language-essential.
		if len(fa.LanguageEssential) > 0 {
			var langParts []string
			for _, lb := range fa.LanguageEssential {
				if lb.Count > 1 {
					langParts = append(langParts, fmt.Sprintf("%s ×%d", lb.Pattern, lb.Count))
				} else {
					langParts = append(langParts, lb.Pattern)
				}
			}
			langTotal := 0
			for _, lb := range fa.LanguageEssential {
				langTotal += lb.Count
			}
			fmt.Fprintf(w, "  Language-essential:          %d  [%s]\n", langTotal, strings.Join(langParts, ", "))
		}

		// Divider and summary.
		total := fa.Summary.Attributed + fa.Summary.LanguageEssential + fa.Summary.Unattributed
		fmt.Fprintln(w, "  ─────────────")
		if total > 0 {
			fmt.Fprintf(w, "  Attributed:    %2d (%d%%)\n", fa.Summary.Attributed, int(fa.Summary.AttributedRatio*100))
			fmt.Fprintf(w, "  Language:      %2d (%d%%)\n", fa.Summary.LanguageEssential, percent(fa.Summary.LanguageEssential, total))
			fmt.Fprintf(w, "  Unattributed: %2d (%d%%)\n", fa.Summary.Unattributed, int(fa.Summary.UnattributedRatio*100))
		}

		// Unattributed branches.
		if len(fa.Unattributed) > 0 {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "  Unattributed branches:")
			for _, ub := range fa.Unattributed {
				partial := ""
				if len(ub.PartialMatches) > 0 {
					partial = "  [partial: " + strings.Join(ub.PartialMatches, ", ") + "]"
				}
				fmt.Fprintf(w, "    L%d:  %s%s\n", ub.Line, ub.Text, partial)
			}
		}
		fmt.Fprintln(w)
	}

	// Overall summary.
	fmt.Fprintln(w, "Budget Summary:")
	fmt.Fprintf(w, "  Functions analyzed:   %d\n", b.Summary.FunctionsAnalyzed)
	fmt.Fprintf(w, "  Fully attributed:     %d\n", b.Summary.FullyAttributed)
	fmt.Fprintf(w, "  Total attributed:     %d\n", b.Summary.TotalAttributed)
	fmt.Fprintf(w, "  Total language:       %d\n", b.Summary.TotalLanguage)
	fmt.Fprintf(w, "  Total unattributed:   %d\n", b.Summary.TotalUnattributed)
	if b.Summary.HighestUnattributed != "" {
		fmt.Fprintf(w, "  Highest unattributed: %s\n", b.Summary.HighestUnattributed)
	}
	fmt.Fprintln(w)
}

func percent(n, total int) int {
	if total == 0 {
		return 0
	}
	return n * 100 / total
}
