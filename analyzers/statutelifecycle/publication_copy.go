package statutelifecycle

import (
	"go/ast"
	"go/types"
)

// publicationCopySafe bounds value rebasing without changing SLC103 identity.
// Structs and arrays copy their value-owned servers and WaitGroups. Reference
// fields retain identity; unsupported type parameters cannot prove a safe copy.
func publicationCopySafe(t types.Type) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	if _, unknown := t.(*types.TypeParam); unknown {
		return false
	}
	if named, ok := t.(*types.Named); ok {
		if publicationValueResource(named) {
			return false
		}
	}
	switch value := t.Underlying().(type) {
	case *types.Struct:
		for field := range value.Fields() {
			if !publicationCopySafe(field.Type()) {
				return false
			}
		}
	case *types.Array:
		return publicationCopySafe(value.Elem())
	}
	return true
}

func publicationValueResource(named *types.Named) bool {
	if isAllowlistedServerType(named) {
		return true
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "sync" && obj.Name() == "WaitGroup"
}

// argumentIdentity refuses a copied aggregate that could manufacture ownership
// of the caller's value objects. It may still share reference fields, so mutation
// analysis invalidates any relevant caller path instead of discarding its effects.
func (w *publicationWalker) argumentIdentity(expr ast.Expr, parameter types.Type, bindings publicationBindings) groupKey {
	if !publicationCopySafe(parameter) || !publicationCopySafe(w.pass.TypesInfo.TypeOf(expr)) {
		if key, ok := w.resolveRaw(expr, bindings); ok && w.relevant(key) {
			w.invalid[key] = true
		}
		return groupKey{}
	}
	key, _ := w.resolveRaw(expr, bindings)
	return key
}
