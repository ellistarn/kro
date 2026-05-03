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
