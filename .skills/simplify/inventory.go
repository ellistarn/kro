package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"

	"golang.org/x/tools/go/packages"
)

// buildInventory creates a semantic inventory for a single package.
func buildInventory(pkg *packages.Package, fset *token.FileSet, baseDir string) *PackageInventory {
	inv := &PackageInventory{
		Path: pkg.PkgPath,
	}

	// Collect methods by receiver type for later attachment.
	type methodEntry struct {
		name      string
		signature string
		exported  bool
	}
	methodsByRecv := map[string][]methodEntry{}

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
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					ti := TypeInfo{
						Name:     ts.Name.Name,
						Exported: ts.Name.IsExported(),
						File:     fname,
						Line:     fset.Position(ts.Pos()).Line,
					}
					switch st := ts.Type.(type) {
					case *ast.StructType:
						ti.Kind = "struct"
						if st.Fields != nil {
							for _, f := range st.Fields.List {
								typeStr := exprToString(f.Type)
								if len(f.Names) == 0 {
									// Embedded field.
									ti.Fields = append(ti.Fields, FieldInfo{
										Name:     embeddedTypeName(f.Type),
										Type:     typeStr,
										Exported: ast.IsExported(embeddedTypeName(f.Type)),
									})
								} else {
									for _, name := range f.Names {
										ti.Fields = append(ti.Fields, FieldInfo{
											Name:     name.Name,
											Type:     typeStr,
											Exported: name.IsExported(),
										})
									}
								}
							}
						}
					case *ast.InterfaceType:
						ti.Kind = "interface"
					default:
						ti.Kind = "alias"
					}
					inv.Types = append(inv.Types, ti)
				}
			case *ast.FuncDecl:
				sig := funcSignature(d.Type)
				if d.Recv != nil {
					recvName := receiverTypeName(d)
					methodsByRecv[recvName] = append(methodsByRecv[recvName], methodEntry{
						name:      d.Name.Name,
						signature: sig,
						exported:  d.Name.IsExported(),
					})
				} else {
					inv.Functions = append(inv.Functions, FuncInfo{
						Name:      d.Name.Name,
						Signature: "func " + d.Name.Name + sig,
						Exported:  d.Name.IsExported(),
						File:      fname,
						Line:      fset.Position(d.Pos()).Line,
					})
				}
			}
		}
	}

	// Attach methods to types.
	for i := range inv.Types {
		for _, m := range methodsByRecv[inv.Types[i].Name] {
			inv.Types[i].Methods = append(inv.Types[i].Methods, MethodInfo{
				Name:      m.name,
				Signature: "func " + m.signature,
				Exported:  m.exported,
			})
		}
	}

	return inv
}

// funcSignature produces a readable signature string from an ast.FuncType.
// e.g. "(ctx context.Context, name string) (error)"
func funcSignature(ft *ast.FuncType) string {
	var b strings.Builder
	b.WriteString("(")
	if ft.Params != nil {
		writeFieldList(&b, ft.Params.List)
	}
	b.WriteString(")")
	if ft.Results != nil && len(ft.Results.List) > 0 {
		b.WriteString(" ")
		if len(ft.Results.List) == 1 && len(ft.Results.List[0].Names) == 0 {
			b.WriteString(exprToString(ft.Results.List[0].Type))
		} else {
			b.WriteString("(")
			writeFieldList(&b, ft.Results.List)
			b.WriteString(")")
		}
	}
	return b.String()
}

func writeFieldList(b *strings.Builder, fields []*ast.Field) {
	for i, f := range fields {
		if i > 0 {
			b.WriteString(", ")
		}
		typeStr := exprToString(f.Type)
		if len(f.Names) == 0 {
			b.WriteString(typeStr)
		} else {
			for j, name := range f.Names {
				if j > 0 {
					b.WriteString(", ")
				}
				b.WriteString(name.Name)
			}
			b.WriteString(" ")
			b.WriteString(typeStr)
		}
	}
}

// exprToString converts an ast.Expr to a readable type string.
// This is approximate — it's for LLM consumption, not compilation.
func exprToString(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprToString(t.X)
	case *ast.SelectorExpr:
		return exprToString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + exprToString(t.Elt)
		}
		return "[" + exprToString(t.Len) + "]" + exprToString(t.Elt)
	case *ast.MapType:
		return "map[" + exprToString(t.Key) + "]" + exprToString(t.Value)
	case *ast.InterfaceType:
		if t.Methods == nil || len(t.Methods.List) == 0 {
			return "any"
		}
		return "interface{...}"
	case *ast.FuncType:
		return "func" + funcSignature(t)
	case *ast.ChanType:
		switch t.Dir {
		case ast.SEND:
			return "chan<- " + exprToString(t.Value)
		case ast.RECV:
			return "<-chan " + exprToString(t.Value)
		default:
			return "chan " + exprToString(t.Value)
		}
	case *ast.Ellipsis:
		return "..." + exprToString(t.Elt)
	case *ast.ParenExpr:
		return "(" + exprToString(t.X) + ")"
	case *ast.BasicLit:
		return t.Value
	case *ast.IndexExpr:
		return exprToString(t.X) + "[" + exprToString(t.Index) + "]"
	case *ast.IndexListExpr:
		parts := make([]string, len(t.Indices))
		for i, idx := range t.Indices {
			parts[i] = exprToString(idx)
		}
		return exprToString(t.X) + "[" + strings.Join(parts, ", ") + "]"
	case *ast.StructType:
		return "struct{...}"
	default:
		return fmt.Sprintf("<%T>", expr)
	}
}
