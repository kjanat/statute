package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Symbol kinds, spelled the way the pkg.go.dev v1 API spells them.
const (
	kindType     = "Type"
	kindFunction = "Function"
	kindMethod   = "Method"
	kindField    = "Field"
	kindConstant = "Constant"
	kindVariable = "Variable"
)

// Symbol is one exported API element in the shape the pkg.go.dev v1 symbol
// endpoint returns it: a "Type.Member" style name, a kind, a one-line
// synopsis, and the type the symbol is documented under (a symbol with no
// enclosing type is its own parent).
type Symbol struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Synopsis string `json:"synopsis"`
	Parent   string `json:"parent"`
}

// Surface is an exported API surface keyed by package import path.
type Surface map[string][]Symbol

// localSurface parses every importable library package under root and returns
// its exported surface. Commands (package main) and internal packages are
// skipped: neither is API anyone outside the module can import, so neither can
// be broken by a change here.
func localSurface(root, modulePath string) (Surface, error) {
	surface := Surface{}
	err := filepath.WalkDir(root, func(dir string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case !d.IsDir():
			return nil
		case dir != root && skipDir(d.Name()):
			return fs.SkipDir
		}
		importPath, syms, err := dirSymbols(dir, root, modulePath)
		if err != nil || syms == nil {
			return err
		}
		surface[importPath] = syms
		return nil
	})
	if err != nil {
		return nil, err
	}
	return surface, nil
}

// skipDir reports whether a directory can never hold importable library code.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
		name == "testdata" || name == "vendor" || name == "internal"
}

// isInternal reports whether an import path is under an internal/ element.
// The published package list includes internal packages; the API gate must
// not, or every refactor of an internal helper reads as a breaking change.
func isInternal(importPath string) bool {
	return slices.Contains(strings.Split(importPath, "/"), "internal")
}

// dirSymbols returns the import path and exported symbols of the package in
// dir, or a nil symbol slice if dir holds no importable library package.
func dirSymbols(dir, root, modulePath string) (string, []Symbol, error) {
	bp, err := build.ImportDir(dir, 0)
	if err != nil {
		if _, ok := errors.AsType[*build.NoGoError](err); ok {
			return "", nil, nil // a directory of docs, fixtures or nothing at all
		}
		return "", nil, fmt.Errorf("%s: %w", dir, err)
	}
	if bp.Name == "main" {
		return "", nil, nil
	}

	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(bp.GoFiles))
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return "", nil, fmt.Errorf("parse: %w", err)
		}
		files = append(files, f)
	}

	importPath := importPathFor(root, dir, modulePath)
	// The zero mode is the one pkg.go.dev renders with: unexported
	// declarations, struct fields, interface methods and composite-literal
	// elements are all filtered out of the AST before we ever see them.
	dp, err := doc.NewFromFiles(fset, files, importPath)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", importPath, err)
	}
	return importPath, packageSymbols(fset, dp), nil
}

// importPathFor maps a directory inside the module to its import path.
func importPathFor(root, dir, modulePath string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return modulePath
	}
	return path.Join(modulePath, filepath.ToSlash(rel))
}

// packageSymbols flattens a documented package into the flat symbol list the
// pkg.go.dev API publishes. Order is irrelevant — the diff is keyed by package
// and symbol name.
func packageSymbols(fset *token.FileSet, dp *doc.Package) []Symbol {
	syms := make([]Symbol, 0, len(dp.Types)+len(dp.Funcs))
	syms = append(syms, valueSymbols(fset, dp.Consts, kindConstant, "")...)
	syms = append(syms, valueSymbols(fset, dp.Vars, kindVariable, "")...)
	for _, fn := range dp.Funcs {
		syms = append(syms, funcSymbol(fset, fn, fn.Name, fn.Name))
	}
	for _, t := range dp.Types {
		syms = append(syms, typeSymbols(fset, t)...)
	}
	return syms
}

// typeSymbols renders a documented type plus everything pkg.go.dev files under
// it: struct fields, interface methods, declared methods, the constructors
// that return it, and the constants and variables of that type.
func typeSymbols(fset *token.FileSet, t *doc.Type) []Symbol {
	spec := typeSpec(t)
	syms := []Symbol{{
		Name:     t.Name,
		Kind:     kindType,
		Synopsis: typeSynopsis(fset, t.Name, spec),
		Parent:   t.Name,
	}}
	syms = append(syms, memberSymbols(fset, t.Name, spec)...)
	syms = append(syms, valueSymbols(fset, t.Consts, kindConstant, t.Name)...)
	syms = append(syms, valueSymbols(fset, t.Vars, kindVariable, t.Name)...)
	for _, fn := range t.Funcs {
		syms = append(syms, funcSymbol(fset, fn, fn.Name, t.Name))
	}
	for _, m := range t.Methods {
		if m.Level != 0 {
			continue // promoted from an embedded type, not declared on this one
		}
		syms = append(syms, funcSymbol(fset, m, t.Name+"."+m.Name, t.Name))
	}
	return syms
}

// typeSpec digs the type specification out of a documented type.
func typeSpec(t *doc.Type) *ast.TypeSpec {
	if t.Decl == nil || len(t.Decl.Specs) == 0 {
		return nil
	}
	spec, _ := t.Decl.Specs[0].(*ast.TypeSpec)
	return spec
}

// typeSynopsis renders "type Name Underlying", with "=" for aliases and the
// type parameter list for generic types.
func typeSynopsis(fset *token.FileSet, name string, spec *ast.TypeSpec) string {
	if spec == nil {
		return "type " + name
	}
	params := ""
	if spec.TypeParams != nil {
		params = "[" + strings.Join(fieldStrings(fset, spec.TypeParams), ", ") + "]"
	}
	sep := " "
	if spec.Assign.IsValid() {
		sep = " = "
	}
	return "type " + name + params + sep + oneLine(fset, spec.Type)
}

// memberSymbols renders the fields of a struct type or the methods of an
// interface type. go/doc has already dropped the unexported ones; pkg.go.dev
// files both under the enclosing type as "Type.Member".
func memberSymbols(fset *token.FileSet, typeName string, spec *ast.TypeSpec) []Symbol {
	if spec == nil {
		return nil
	}
	var (
		list *ast.FieldList
		kind string
	)
	switch u := spec.Type.(type) {
	case *ast.StructType:
		list, kind = u.Fields, kindField
	case *ast.InterfaceType:
		list, kind = u.Methods, kindMethod
	default:
		return nil
	}
	if list == nil {
		return nil
	}

	syms := make([]Symbol, 0, len(list.List))
	for _, f := range list.List {
		for _, name := range memberNames(f) {
			syms = append(syms, Symbol{
				Name:     typeName + "." + name,
				Kind:     kind,
				Synopsis: name + " " + oneLine(fset, f.Type),
				Parent:   typeName,
			})
		}
	}
	return syms
}

// memberNames returns the names a field contributes. An embedded field is
// named after the type it embeds, which is how it is referenced by callers.
func memberNames(f *ast.Field) []string {
	if len(f.Names) > 0 {
		names := make([]string, 0, len(f.Names))
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
		return names
	}
	if name := embeddedName(f.Type); name != "" {
		return []string{name}
	}
	return nil
}

// embeddedName is the selector an embedded field is reached by: the bare type
// name, with any pointer, qualifier or type argument stripped.
func embeddedName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return embeddedName(e.X)
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.IndexExpr:
		return embeddedName(e.X)
	case *ast.IndexListExpr:
		return embeddedName(e.X)
	}
	return ""
}

// funcSymbol renders a function or method declaration as its full one-line
// signature, receiver included.
func funcSymbol(fset *token.FileSet, fn *doc.Func, name, parent string) Symbol {
	kind, recv := kindFunction, ""
	if fn.Decl.Recv != nil {
		kind = kindMethod
		if recvs := fieldStrings(fset, fn.Decl.Recv); len(recvs) > 0 {
			recv = "(" + recvs[0] + ") "
		}
	}
	return Symbol{
		Name:     name,
		Kind:     kind,
		Synopsis: "func " + recv + fn.Name + signature(fset, fn.Decl.Type),
		Parent:   parent,
	}
}

// valueSymbols renders a constant or variable group. pkg.go.dev publishes a
// constant as a bare "const Name" — the value is not part of the surface it
// records — but spells out a variable's type and initialiser.
func valueSymbols(fset *token.FileSet, vals []*doc.Value, kind, parent string) []Symbol {
	var syms []Symbol
	for _, v := range vals {
		for _, spec := range v.Decl.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if !ident.IsExported() {
					continue
				}
				syms = append(syms, Symbol{
					Name:     ident.Name,
					Kind:     kind,
					Synopsis: valueSynopsis(fset, kind, ident.Name, vs, i),
					Parent:   parentOr(parent, ident.Name),
				})
			}
		}
	}
	return syms
}

// valueSynopsis renders one name out of a const or var specification.
func valueSynopsis(fset *token.FileSet, kind, name string, spec *ast.ValueSpec, i int) string {
	if kind == kindConstant {
		return "const " + name
	}
	syn := "var " + name
	if spec.Type != nil {
		syn += " " + oneLine(fset, spec.Type)
	}
	// Names and values only line up one-to-one; a spec whose values came from
	// a single multi-valued call has no per-name initialiser to show.
	if len(spec.Values) == len(spec.Names) && i < len(spec.Values) {
		syn += " = " + oneLine(fset, spec.Values[i])
	}
	return syn
}

// parentOr defaults a symbol's parent to the symbol itself, matching how
// pkg.go.dev files a declaration that belongs to no documented type.
func parentOr(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent
}
