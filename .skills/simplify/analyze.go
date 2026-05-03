package main

import (
	"bufio"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// loadPackages loads Go packages matching the given patterns.
func loadPackages(dir string, patterns []string) ([]*packages.Package, *token.FileSet, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedImports | packages.NeedTypes,
		Fset:  fset,
		Tests: false,
	}
	if dir != "" {
		cfg.Dir = dir
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, nil, err
	}
	return pkgs, fset, nil
}

// analyzePackage computes metrics for a single package.
func analyzePackage(pkg *packages.Package, fset *token.FileSet, modulePrefix string, fanInMap map[string]int, baseDir string) PackageMetrics {
	pm := PackageMetrics{
		Path:  pkg.PkgPath,
		FanIn: fanInMap[pkg.PkgPath],
	}

	// Fan-out: count module-internal imports.
	for imp := range pkg.Imports {
		if isModuleImport(imp, modulePrefix) {
			pm.FanOut++
		}
	}

	// Collect struct info across files.
	structs := map[string]*structInfo{}
	methodsByRecv := map[string][]methodInfo{}

	for _, file := range pkg.Syntax {
		absPath := fset.Position(file.Pos()).Filename
		fname := relativePath(absPath, baseDir)

		if strings.HasSuffix(fname, "_test.go") {
			continue
		}
		if isGeneratedFile(absPath) {
			continue
		}

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if st, ok := s.Type.(*ast.StructType); ok {
							structs[s.Name.Name] = &structInfo{
								name:     s.Name.Name,
								file:     fname,
								line:     fset.Position(s.Pos()).Line,
								fields:   st.Fields.List,
								typeSpec: st,
							}
						}
						if s.Name.IsExported() {
							pm.ExportedSymbols++
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								pm.ExportedSymbols++
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil {
					if d.Name.IsExported() {
						pm.ExportedSymbols++
						if hasUntypedParams(d.Type) {
							pm.UntypedCrossings++
						}
					}
				} else {
					recvName := receiverTypeName(d)
					methodsByRecv[recvName] = append(methodsByRecv[recvName], methodInfo{funcDecl: d, file: fname})
					if d.Name.IsExported() {
						pm.ExportedSymbols++
						if hasUntypedParams(d.Type) {
							pm.UntypedCrossings++
						}
					}
				}

				fm := FunctionMetrics{
					Name:       d.Name.Name,
					File:       fname,
					Line:       fset.Position(d.Pos()).Line,
					Receiver:   receiverTypeName(d),
					Params:     countParams(d.Type),
					Returns:    countReturns(d.Type),
					Cyclomatic: cyclomaticComplexity(d),
					Cognitive:  cognitiveComplexity(d),
				}
				pm.Functions = append(pm.Functions, fm)
			}
		}
	}

	// Struct metrics.
	for _, si := range structs {
		methods := methodsByRecv[si.name]
		exported := ast.IsExported(si.name)
		sm := StructMetrics{
			Name:                  si.name,
			Exported:              exported,
			File:                  si.file,
			Line:                  si.line,
			Methods:               len(methods),
			ExportedMutableFields: countExportedMutableFields(si.fields),
		}
		// Constructor bypass only matters for exported types — unexported
		// types can't be constructed from outside the package.
		if exported {
			sm.ConstructorBypass = allFieldsExported(si.fields)
		}
		if len(methods) > 0 {
			sm.LCOM4 = computeLCOM4(si, methods)
		}
		pm.Structs = append(pm.Structs, sm)
	}

	return pm
}

// --- Helper functions ---

// detectModulePrefix finds the longest common package path prefix among loaded packages.
func detectModulePrefix(pkgs []*packages.Package) string {
	if len(pkgs) == 0 {
		return ""
	}
	parts := strings.Split(pkgs[0].PkgPath, "/")
	for _, pkg := range pkgs[1:] {
		pp := strings.Split(pkg.PkgPath, "/")
		n := len(parts)
		if len(pp) < n {
			n = len(pp)
		}
		match := 0
		for i := 0; i < n; i++ {
			if parts[i] != pp[i] {
				break
			}
			match = i + 1
		}
		parts = parts[:match]
	}
	if len(parts) < 3 {
		return strings.Join(parts, "/")
	}
	return strings.Join(parts[:3], "/")
}

func isModuleImport(imp, modulePrefix string) bool {
	if modulePrefix == "" {
		return false
	}
	return strings.HasPrefix(imp, modulePrefix)
}

func resolveDir(dir string) string {
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return dir
		}
		return abs
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func relativePath(absPath, baseDir string) string {
	if baseDir == "" {
		return absPath
	}
	rel, err := filepath.Rel(baseDir, absPath)
	if err != nil {
		return absPath
	}
	return rel
}

func isGeneratedFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for i := 0; i < 3 && scanner.Scan(); i++ {
		line := scanner.Text()
		if strings.Contains(line, "Code generated") && strings.Contains(line, "DO NOT EDIT") {
			return true
		}
	}
	return false
}
