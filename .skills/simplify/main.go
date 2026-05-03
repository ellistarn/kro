// Command simplify computes code simplicity metrics, semantic inventory,
// and cross-cutting signals from Go source via AST analysis.
//
// Usage: go run . [-dir=<path>] [-format=json|text] <package-pattern>...
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	dir := flag.String("dir", "", "directory to use as working directory for package resolution")
	format := flag.String("format", "json", "output format: json or text")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: %s [-dir=<path>] [-format=json|text] <package-pattern>...\n", os.Args[0])
		os.Exit(1)
	}

	pkgs, fset, err := loadPackages(*dir, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading packages: %v\n", err)
		os.Exit(1)
	}

	baseDir := resolveDir(*dir)

	// Detect module prefix for fan-in/fan-out filtering.
	modulePrefix := detectModulePrefix(pkgs)

	// Build fan-in map.
	fanInMap := map[string]int{}
	for _, pkg := range pkgs {
		for imp := range pkg.Imports {
			if isModuleImport(imp, modulePrefix) {
				fanInMap[imp]++
			}
		}
	}

	report := Report{
		Dir:       baseDir,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	for _, pkg := range pkgs {
		if len(pkg.Syntax) == 0 {
			continue
		}
		pm := analyzePackage(pkg, fset, modulePrefix, fanInMap, baseDir)
		report.Packages = append(report.Packages, pm)
	}

	// Compute inventory.
	for i, pkg := range pkgs {
		if len(pkg.Syntax) == 0 || i >= len(report.Packages) {
			continue
		}
		report.Packages[i].Inventory = buildInventory(pkg, fset, baseDir)
	}

	// Compute signals across all packages.
	report.Signals = computeSignals(pkgs, fset, baseDir)

	// Compute violations grouped by phase.
	report.Violations = computeViolations(report.Packages)

	switch *format {
	case "text":
		formatText(os.Stdout, &report)
	default:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "error encoding JSON: %v\n", err)
			os.Exit(1)
		}
	}
}
