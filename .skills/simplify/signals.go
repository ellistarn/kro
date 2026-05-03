package main

import (
	"go/ast"
	"go/token"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// computeSignals detects cross-cutting patterns across all loaded packages.
func computeSignals(pkgs []*packages.Package, fset *token.FileSet, baseDir string) Signals {
	var signals Signals

	// Collect all exported function/type definitions and all ident references across files.
	type exportedDef struct {
		name    string
		pkg     string
		file    string // relative
		absFile string
		line    int
		isFunc  bool
		isType  bool
	}

	var defs []exportedDef
	// Map from filename -> set of ident names in that file.
	identsByFile := map[string]map[string]bool{}

	// For parameter group detection: collect param type lists.
	type funcParamEntry struct {
		name    string
		pkg     string
		types   []string
		typeSet string // sorted, joined key
	}
	var paramEntries []funcParamEntry

	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			absPath := fset.Position(file.Pos()).Filename
			fname := relativePath(absPath, baseDir)
			if strings.HasSuffix(fname, "_test.go") {
				continue
			}
			if isGeneratedFile(absPath) {
				continue
			}

			// Collect all idents in this file for cross-reference.
			fileIdents := map[string]bool{}
			ast.Inspect(file, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					fileIdents[id.Name] = true
				}
				return true
			})
			identsByFile[absPath] = fileIdents

			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						if ts, ok := spec.(*ast.TypeSpec); ok {
							if ts.Name.IsExported() {
								defs = append(defs, exportedDef{
									name:    ts.Name.Name,
									pkg:     pkg.PkgPath,
									file:    fname,
									absFile: absPath,
									line:    fset.Position(ts.Pos()).Line,
									isType:  true,
								})
							}
						}
					}
				case *ast.FuncDecl:
					if d.Name.IsExported() && d.Recv == nil {
						defs = append(defs, exportedDef{
							name:    d.Name.Name,
							pkg:     pkg.PkgPath,
							file:    fname,
							absFile: absPath,
							line:    fset.Position(d.Pos()).Line,
							isFunc:  true,
						})
					}

					// Parameter group detection: functions with >2 params.
					if d.Type.Params != nil {
						var paramTypes []string
						for _, f := range d.Type.Params.List {
							typeStr := exprToString(f.Type)
							n := len(f.Names)
							if n == 0 {
								n = 1
							}
							for range n {
								paramTypes = append(paramTypes, typeStr)
							}
						}
						if len(paramTypes) > 2 {
							funcName := d.Name.Name
							if d.Recv != nil {
								funcName = receiverTypeName(d) + "." + funcName
							}
							sorted := make([]string, len(paramTypes))
							copy(sorted, paramTypes)
							sort.Strings(sorted)
							paramEntries = append(paramEntries, funcParamEntry{
								name:    funcName,
								pkg:     pkg.PkgPath,
								types:   paramTypes,
								typeSet: strings.Join(sorted, ","),
							})
						}
					}
				}
			}
		}
	}

	// Dangling detection: exported functions/types with zero cross-file references.
	for _, def := range defs {
		crossFileRefs := 0
		for filePath, idents := range identsByFile {
			if filePath == def.absFile {
				continue
			}
			if idents[def.name] {
				crossFileRefs++
			}
		}
		if crossFileRefs == 0 {
			if def.isFunc {
				signals.DanglingFunctions = append(signals.DanglingFunctions, DanglingSymbol{
					Name:    def.name,
					Package: def.pkg,
					File:    def.file,
					Line:    def.line,
				})
			}
			if def.isType {
				signals.DanglingTypes = append(signals.DanglingTypes, DanglingSymbol{
					Name:    def.name,
					Package: def.pkg,
					File:    def.file,
					Line:    def.line,
				})
			}
		}
	}

	// Parameter group co-occurrence: group by typeSet, report those with >2 occurrences.
	typeSetGroups := map[string][]funcParamEntry{}
	for _, entry := range paramEntries {
		typeSetGroups[entry.typeSet] = append(typeSetGroups[entry.typeSet], entry)
	}
	for _, entries := range typeSetGroups {
		if len(entries) <= 2 {
			continue
		}
		funcNames := make([]string, len(entries))
		for i, e := range entries {
			funcNames[i] = e.name
		}
		// Use the types from the first entry (they're all the same set).
		sorted := make([]string, len(entries[0].types))
		copy(sorted, entries[0].types)
		sort.Strings(sorted)
		// Deduplicate type list for display.
		deduped := dedupStrings(sorted)
		signals.ParameterGroups = append(signals.ParameterGroups, ParameterGroup{
			Types:       deduped,
			Occurrences: len(entries),
			Functions:   funcNames,
		})
	}

	// Sort parameter groups by occurrence count descending.
	sort.Slice(signals.ParameterGroups, func(i, j int) bool {
		return signals.ParameterGroups[i].Occurrences > signals.ParameterGroups[j].Occurrences
	})

	return signals
}

func dedupStrings(sorted []string) []string {
	if len(sorted) == 0 {
		return sorted
	}
	result := []string{sorted[0]}
	for _, s := range sorted[1:] {
		if s != result[len(result)-1] {
			result = append(result, s)
		}
	}
	return result
}
