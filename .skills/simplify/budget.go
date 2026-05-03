package main

import (
	"bytes"
	"go/ast"
	"go/printer"
	"go/token"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// --- Budget file types ---

type BudgetFile struct {
	Concepts []BudgetConcept `yaml:"concepts"`
}

type BudgetConcept struct {
	Name        string   `yaml:"name"`
	Design      string   `yaml:"design"`
	Keywords    []string `yaml:"keywords"`
	MinBranches int      `yaml:"min_branches"`
}

// --- Branch extraction ---

// branchInfo represents a single branch condition extracted from the AST.
type branchInfo struct {
	line int
	text string
}

// extractBranches walks a function body and extracts all branch condition texts.
// It captures: IfStmt conditions, CaseClause list expressions, CommClause presence,
// and BinaryExpr with && or || operators (counted as additional decision points).
func extractBranches(fn *ast.FuncDecl, fset *token.FileSet) []branchInfo {
	if fn.Body == nil {
		return nil
	}
	var branches []branchInfo
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IfStmt:
			if node.Cond != nil {
				text := nodeToString(fset, node.Cond)
				line := fset.Position(node.Cond.Pos()).Line
				branches = append(branches, branchInfo{line: line, text: text})
			}
		case *ast.CaseClause:
			for _, expr := range node.List {
				text := nodeToString(fset, expr)
				line := fset.Position(expr.Pos()).Line
				branches = append(branches, branchInfo{line: line, text: text})
			}
		case *ast.CommClause:
			if node.Comm != nil {
				text := nodeToString(fset, node.Comm)
				line := fset.Position(node.Comm.Pos()).Line
				branches = append(branches, branchInfo{line: line, text: text})
			}
		case *ast.BinaryExpr:
			if node.Op == token.LAND || node.Op == token.LOR {
				// Only count top-level logical operators (not nested inside an IfStmt.Cond
				// we already captured). We check if the parent is not another BinaryExpr
				// with && or ||. Since ast.Inspect doesn't give parent context, we instead
				// skip these — they're already captured by the IfStmt/CaseClause extraction.
				// Actually, for cyclomatic counting, && and || are separate decision points.
				// But for budget attribution, we attribute at the whole-condition level.
				// So we skip standalone BinaryExpr to avoid double-counting.
				return true
			}
		}
		return true
	})
	return branches
}

// nodeToString converts an AST node back to its source text representation.
func nodeToString(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return "<unknown>"
	}
	return buf.String()
}

// --- Budget loading ---

func loadBudgetFile(path string) (*BudgetFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bf BudgetFile
	if err := yaml.Unmarshal(data, &bf); err != nil {
		return nil, err
	}
	return &bf, nil
}

// --- Language-essential detection ---

// isLanguageEssential checks if a branch condition text matches common
// language-essential patterns (error handling, nil checks, ok-idiom).
// Returns the pattern name if matched, empty string otherwise.
func isLanguageEssential(text string, conceptKeywords map[string]bool) string {
	// err != nil or err == nil
	if strings.Contains(text, "err != nil") || strings.Contains(text, "err == nil") {
		return "err != nil"
	}
	// !ok idiom
	if strings.Contains(text, "!ok") {
		return "ok-idiom"
	}
	// Generic nil checks: == nil or != nil where the identifier is not a concept keyword.
	if strings.Contains(text, "== nil") || strings.Contains(text, "!= nil") {
		// Check if any concept keyword appears in the text.
		for kw := range conceptKeywords {
			if strings.Contains(text, kw) {
				return "" // This is a concept-related nil check, not language-essential.
			}
		}
		return "nil check"
	}
	return ""
}

// --- Keyword matching ---

// matchConcept checks if a branch condition text matches any keyword of any concept.
// Returns the concept name and matched keywords.
func matchConcept(text string, concepts []BudgetConcept) (string, []string) {
	for _, concept := range concepts {
		var matched []string
		for _, kw := range concept.Keywords {
			if strings.Contains(text, kw) {
				matched = append(matched, kw)
			}
		}
		if len(matched) > 0 {
			return concept.Name, matched
		}
	}
	return "", nil
}

// partialMatches returns concept keywords that partially appear in the text
// but didn't fully attribute the branch.
func partialMatches(text string, concepts []BudgetConcept) []string {
	var matches []string
	for _, concept := range concepts {
		for _, kw := range concept.Keywords {
			if strings.Contains(text, kw) {
				matches = append(matches, kw)
			}
		}
	}
	return matches
}

// --- Budget analysis ---

// analyzeBudget runs budget attribution analysis across all packages.
func analyzeBudget(budget *BudgetFile, pkgs []PackageMetrics, fset *token.FileSet, allFuncs map[string]*ast.FuncDecl) *BudgetReport {
	report := &BudgetReport{}

	// Build a set of all concept keywords for language-essential detection.
	conceptKeywords := map[string]bool{}
	for _, concept := range budget.Concepts {
		for _, kw := range concept.Keywords {
			conceptKeywords[kw] = true
		}
	}

	for _, pkg := range pkgs {
		for _, fm := range pkg.Functions {
			// Only analyze functions with cyclomatic > 5.
			if fm.Cyclomatic <= 5 {
				continue
			}

			// Look up the AST node.
			key := fm.File + ":" + fm.Name
			if fm.Receiver != "" {
				key = fm.File + ":" + fm.Receiver + "." + fm.Name
			}
			fn, ok := allFuncs[key]
			if !ok {
				continue
			}

			branches := extractBranches(fn, fset)
			if len(branches) == 0 {
				continue
			}

			fa := FunctionAttribution{
				Name:       fm.Name,
				File:       fm.File,
				Line:       fm.Line,
				Receiver:   fm.Receiver,
				Cyclomatic: fm.Cyclomatic,
			}

			// Track concept attributions.
			conceptCounts := map[string]*ConceptAttribution{}
			langCounts := map[string]*LanguageBranch{}

			for _, branch := range branches {
				// Check language-essential first.
				if pattern := isLanguageEssential(branch.text, conceptKeywords); pattern != "" {
					if lb, ok := langCounts[pattern]; ok {
						lb.Count++
						lb.Lines = append(lb.Lines, branch.line)
					} else {
						langCounts[pattern] = &LanguageBranch{
							Pattern: pattern,
							Count:   1,
							Lines:   []int{branch.line},
						}
					}
					continue
				}

				// Check concept keywords.
				concept, keywords := matchConcept(branch.text, budget.Concepts)
				if concept != "" {
					if ca, ok := conceptCounts[concept]; ok {
						ca.Count++
						ca.Lines = append(ca.Lines, branch.line)
						// Merge keywords.
						kwSet := map[string]bool{}
						for _, kw := range ca.Keywords {
							kwSet[kw] = true
						}
						for _, kw := range keywords {
							if !kwSet[kw] {
								ca.Keywords = append(ca.Keywords, kw)
							}
						}
					} else {
						conceptCounts[concept] = &ConceptAttribution{
							Concept:  concept,
							Count:    1,
							Keywords: keywords,
							Lines:    []int{branch.line},
						}
					}
					continue
				}

				// Unattributed.
				ub := UnattributedBranch{
					Line: branch.line,
					Text: truncateText(branch.text, 100),
				}
				// Check for partial matches.
				if pm := partialMatches(branch.text, budget.Concepts); len(pm) > 0 {
					ub.PartialMatches = pm
				}
				fa.Unattributed = append(fa.Unattributed, ub)
			}

			// Collect attributed concepts.
			for _, ca := range conceptCounts {
				fa.Attributed = append(fa.Attributed, *ca)
			}
			// Collect language-essential branches.
			for _, lb := range langCounts {
				fa.LanguageEssential = append(fa.LanguageEssential, *lb)
			}

			// Compute summary.
			attrCount := 0
			for _, ca := range fa.Attributed {
				attrCount += ca.Count
			}
			langCount := 0
			for _, lb := range fa.LanguageEssential {
				langCount += lb.Count
			}
			unattrCount := len(fa.Unattributed)
			total := attrCount + langCount + unattrCount

			fa.Summary = AttributionSummary{
				Attributed:        attrCount,
				LanguageEssential: langCount,
				Unattributed:      unattrCount,
			}
			if total > 0 {
				fa.Summary.AttributedRatio = float64(attrCount) / float64(total)
				fa.Summary.UnattributedRatio = float64(unattrCount) / float64(total)
			}

			report.Functions = append(report.Functions, fa)
		}
	}

	// Compute summary.
	report.Summary = computeBudgetSummary(report.Functions)

	return report
}

func computeBudgetSummary(functions []FunctionAttribution) BudgetSummary {
	s := BudgetSummary{
		FunctionsAnalyzed: len(functions),
	}

	highestUnattrCount := 0
	for _, fa := range functions {
		s.TotalAttributed += fa.Summary.Attributed
		s.TotalLanguage += fa.Summary.LanguageEssential
		s.TotalUnattributed += fa.Summary.Unattributed

		if fa.Summary.Unattributed == 0 {
			s.FullyAttributed++
		}
		if fa.Summary.Unattributed > highestUnattrCount {
			highestUnattrCount = fa.Summary.Unattributed
			name := fa.Name
			if fa.Receiver != "" {
				name = fa.Receiver + "." + fa.Name
			}
			s.HighestUnattributed = name
		}
	}

	return s
}

func truncateText(s string, maxLen int) string {
	// Collapse whitespace.
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxLen {
		return s[:maxLen-3] + "..."
	}
	return s
}
