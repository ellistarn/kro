package main

import (
	"go/ast"
	"go/token"
)

// --- Internal types ---

type structInfo struct {
	name     string
	file     string
	line     int
	fields   []*ast.Field
	typeSpec *ast.StructType
}

type methodInfo struct {
	funcDecl *ast.FuncDecl
	file     string
}

// --- Receiver helpers ---

func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if ident, ok := t.(*ast.Ident); ok {
		return ident.Name
	}
	if idx, ok := t.(*ast.IndexExpr); ok {
		if ident, ok := idx.X.(*ast.Ident); ok {
			return ident.Name
		}
	}
	if idx, ok := t.(*ast.IndexListExpr); ok {
		if ident, ok := idx.X.(*ast.Ident); ok {
			return ident.Name
		}
	}
	return ""
}

func receiverVarName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	names := fn.Recv.List[0].Names
	if len(names) == 0 {
		return ""
	}
	return names[0].Name
}

// --- Untyped crossings ---

func hasUntypedParams(ft *ast.FuncType) bool {
	if ft.Params != nil {
		for _, f := range ft.Params.List {
			if isUntypedType(f.Type) {
				return true
			}
		}
	}
	if ft.Results != nil {
		for _, f := range ft.Results.List {
			if isUntypedType(f.Type) {
				return true
			}
		}
	}
	return false
}

func isUntypedType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "any"
	case *ast.InterfaceType:
		return t.Methods == nil || len(t.Methods.List) == 0
	case *ast.MapType:
		if key, ok := t.Key.(*ast.Ident); ok && key.Name == "string" {
			return isUntypedType(t.Value)
		}
	case *ast.StarExpr:
		return isUntypedType(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name == "Unstructured"
	}
	return false
}

// --- Field analysis ---

func countExportedMutableFields(fields []*ast.Field) int {
	count := 0
	for _, f := range fields {
		if !hasExportedName(f) {
			continue
		}
		if isMutableType(f.Type) {
			n := len(f.Names)
			if n == 0 {
				n = 1
			}
			count += n
		}
	}
	return count
}

func hasExportedName(f *ast.Field) bool {
	if len(f.Names) == 0 {
		return ast.IsExported(embeddedTypeName(f.Type))
	}
	for _, n := range f.Names {
		if n.IsExported() {
			return true
		}
	}
	return false
}

func embeddedTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return embeddedTypeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

func isMutableType(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.MapType:
		return true
	case *ast.ArrayType:
		if e.Len == nil {
			return true
		}
	case *ast.StarExpr:
		return true
	case *ast.FuncType:
		return true
	}
	return false
}

func allFieldsExported(fields []*ast.Field) bool {
	if len(fields) == 0 {
		return true
	}
	for _, f := range fields {
		if len(f.Names) == 0 {
			if !ast.IsExported(embeddedTypeName(f.Type)) {
				return false
			}
			continue
		}
		for _, n := range f.Names {
			if !n.IsExported() {
				return false
			}
		}
	}
	return true
}

// --- Param / Return counting ---

func countParams(ft *ast.FuncType) int {
	if ft.Params == nil {
		return 0
	}
	count := 0
	for _, f := range ft.Params.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		count += n
	}
	return count
}

func countReturns(ft *ast.FuncType) int {
	if ft.Results == nil {
		return 0
	}
	count := 0
	for _, f := range ft.Results.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		count += n
	}
	return count
}

// --- Cyclomatic complexity ---

func cyclomaticComplexity(fn *ast.FuncDecl) int {
	if fn.Body == nil {
		return 1
	}
	complexity := 1
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			complexity++
		case *ast.ForStmt:
			complexity++
		case *ast.RangeStmt:
			complexity++
		case *ast.CaseClause:
			if n.List != nil {
				complexity++
			}
		case *ast.CommClause:
			if n.Comm != nil {
				complexity++
			}
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				complexity++
			}
		}
		return true
	})
	return complexity
}

// --- Cognitive complexity ---

func cognitiveComplexity(fn *ast.FuncDecl) int {
	if fn.Body == nil {
		return 0
	}
	total := 0
	var walk func(stmts []ast.Stmt, depth int)
	walk = func(stmts []ast.Stmt, depth int) {
		for _, stmt := range stmts {
			switch s := stmt.(type) {
			case *ast.IfStmt:
				total += 1 + depth
				total += countLogicalOps(s.Cond)
				walk(s.Body.List, depth+1)
				if s.Else != nil {
					switch e := s.Else.(type) {
					case *ast.BlockStmt:
						total++
						walk(e.List, depth+1)
					case *ast.IfStmt:
						total++
						total += countLogicalOps(e.Cond)
						walk(e.Body.List, depth+1)
						if e.Else != nil {
							walkElse(e.Else, depth, &total, walk)
						}
					}
				}
			case *ast.ForStmt:
				total += 1 + depth
				if s.Cond != nil {
					total += countLogicalOps(s.Cond)
				}
				walk(s.Body.List, depth+1)
			case *ast.RangeStmt:
				total += 1 + depth
				walk(s.Body.List, depth+1)
			case *ast.SwitchStmt:
				total += 1 + depth
				walk(s.Body.List, depth+1)
			case *ast.TypeSwitchStmt:
				total += 1 + depth
				walk(s.Body.List, depth+1)
			case *ast.SelectStmt:
				total += 1 + depth
				walk(s.Body.List, depth+1)
			case *ast.CaseClause:
				walk(s.Body, depth)
			case *ast.CommClause:
				walk(s.Body, depth)
			case *ast.BlockStmt:
				walk(s.List, depth)
			case *ast.BranchStmt:
				if s.Tok == token.GOTO {
					total++
				}
				if s.Label != nil && (s.Tok == token.BREAK || s.Tok == token.CONTINUE) {
					total++
				}
			case *ast.ExprStmt:
				total += countLogicalOpsInExpr(s.X)
			case *ast.AssignStmt:
				for _, rhs := range s.Rhs {
					total += countLogicalOpsInExpr(rhs)
				}
			case *ast.ReturnStmt:
				for _, r := range s.Results {
					total += countLogicalOpsInExpr(r)
				}
			case *ast.DeferStmt:
				total += countLogicalOpsInExpr(s.Call)
			case *ast.GoStmt:
				total += countLogicalOpsInExpr(s.Call)
			}
			inspectFuncLiterals(stmt, depth, &total, walk)
		}
	}
	walk(fn.Body.List, 0)
	return total
}

func walkElse(els ast.Stmt, depth int, total *int, walk func([]ast.Stmt, int)) {
	switch e := els.(type) {
	case *ast.BlockStmt:
		*total++
		walk(e.List, depth+1)
	case *ast.IfStmt:
		*total++
		*total += countLogicalOps(e.Cond)
		walk(e.Body.List, depth+1)
		if e.Else != nil {
			walkElse(e.Else, depth, total, walk)
		}
	}
}

func inspectFuncLiterals(node ast.Node, depth int, total *int, walk func([]ast.Stmt, int)) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt,
			*ast.TypeSwitchStmt, *ast.SelectStmt:
			if n != node {
				return false
			}
		case *ast.FuncLit:
			fl := n.(*ast.FuncLit)
			if fl.Body != nil {
				walk(fl.Body.List, depth+1)
			}
			return false
		}
		return true
	})
}

func countLogicalOps(expr ast.Expr) int {
	count := 0
	ast.Inspect(expr, func(n ast.Node) bool {
		if bin, ok := n.(*ast.BinaryExpr); ok {
			if bin.Op == token.LAND || bin.Op == token.LOR {
				count++
			}
		}
		return true
	})
	return count
}

func countLogicalOpsInExpr(expr ast.Expr) int {
	count := 0
	ast.Inspect(expr, func(n ast.Node) bool {
		if bin, ok := n.(*ast.BinaryExpr); ok {
			if bin.Op == token.LAND || bin.Op == token.LOR {
				count++
			}
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		return true
	})
	return count
}

// --- LCOM4 ---

func computeLCOM4(si *structInfo, methods []methodInfo) int {
	fieldNames := map[string]bool{}
	for _, f := range si.fields {
		if len(f.Names) == 0 {
			if name := embeddedTypeName(f.Type); name != "" {
				fieldNames[name] = true
			}
		} else {
			for _, n := range f.Names {
				fieldNames[n.Name] = true
			}
		}
	}

	methodIndex := map[string]int{}
	for i, m := range methods {
		methodIndex[m.funcDecl.Name.Name] = i
	}

	methodFieldAccess := make([]map[string]bool, len(methods))
	methodCalls := make([]map[int]bool, len(methods))
	for i, m := range methods {
		recvVar := receiverVarName(m.funcDecl)
		accessed := map[string]bool{}
		calls := map[int]bool{}
		if m.funcDecl.Body != nil && recvVar != "" {
			ast.Inspect(m.funcDecl.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != recvVar {
					return true
				}
				if fieldNames[sel.Sel.Name] {
					accessed[sel.Sel.Name] = true
				}
				if idx, ok := methodIndex[sel.Sel.Name]; ok && idx != i {
					calls[idx] = true
				}
				return true
			})
		}
		methodFieldAccess[i] = accessed
		methodCalls[i] = calls
	}

	// Union-Find.
	parent := make([]int, len(methods))
	rank := make([]int, len(methods))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if rank[ra] < rank[rb] {
			ra, rb = rb, ra
		}
		parent[rb] = ra
		if rank[ra] == rank[rb] {
			rank[ra]++
		}
	}

	for i := 0; i < len(methods); i++ {
		for j := i + 1; j < len(methods); j++ {
			for field := range methodFieldAccess[i] {
				if methodFieldAccess[j][field] {
					union(i, j)
					break
				}
			}
		}
	}
	for i, calls := range methodCalls {
		for j := range calls {
			union(i, j)
		}
	}

	components := map[int]bool{}
	for i := range methods {
		components[find(i)] = true
	}
	return len(components)
}
