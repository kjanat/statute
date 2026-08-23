// Package statutehttp provides Statute-specific net/http correctness checks.
package statutehttp

import (
	"go/ast"
	"go/constant"
	"go/types"
	"net/textproto"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
)

const (
	pluginName        = "statutehttp"
	statuteImportPath = "statute.kjanat.dev"
)

var requestSpecialFields = map[string]string{
	"Host":              "Host",
	"Content-Length":    "ContentLength",
	"Transfer-Encoding": "TransferEncoding",
	"Trailer":           "Trailer",
}

var requestHeaderConstructors = map[string]struct{}{
	"SetRequestHeader":    {},
	"AddRequestHeader":    {},
	"RemoveRequestHeader": {},
}

// Analyzer rejects compile-time-known attempts to mutate request special
// fields through the Header map. net/http serializes these values from
// dedicated http.Request fields, so Header mutations are silently ignored.
var Analyzer = &analysis.Analyzer{
	Name: pluginName,
	Doc:  "reject ineffective mutations of net/http request special fields through Header",
	Run:  run,
}

func init() {
	register.Plugin(pluginName, newPlugin)
}

type plugin struct{}

func newPlugin(any) (register.LinterPlugin, error) {
	return plugin{}, nil
}

func (plugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{Analyzer}, nil
}

func (plugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CallExpr:
				checkCall(pass, n)
			case *ast.AssignStmt:
				checkHeaderIndexAssign(pass, n)
			}
			return true
		})
	}

	return nil, nil
}

func checkCall(pass *analysis.Pass, call *ast.CallExpr) {
	if name, arg, ok := deleteFromRequestHeader(pass, call); ok {
		if field, special := requestSpecialFields[name]; special {
			pass.Reportf(arg.Pos(),
				"request header %q is represented by http.Request.%s; mutating Request.Header is ignored",
				name, field)
		}
		return
	}

	if len(call.Args) == 0 {
		return
	}
	name, ok := constantString(pass, call.Args[0])
	if !ok {
		return
	}
	canonical := textproto.CanonicalMIMEHeaderKey(name)
	field, special := requestSpecialFields[canonical]
	if !special {
		return
	}

	switch {
	case isDirectRequestHeaderMutation(pass, call):
		pass.Reportf(call.Args[0].Pos(),
			"request header %q is represented by http.Request.%s; mutating Request.Header is ignored",
			canonical, field)
	case isStatuteRequestHeaderConstructor(pass, call):
		pass.Reportf(call.Args[0].Pos(),
			"%s cannot mutate request header %q because net/http uses http.Request.%s; use field-aware middleware or reject the configuration",
			calledFunction(pass, call).Name(), canonical, field)
	}
}

// checkHeaderIndexAssign flags r.Header["Host"] = ... map-index writes. Raw
// map access does not canonicalise, so only the exact canonical key names the
// field net/http serializes around; a differently-cased key is a different
// (and serialized) map entry.
func checkHeaderIndexAssign(pass *analysis.Pass, assign *ast.AssignStmt) {
	for _, lhs := range assign.Lhs {
		index, ok := lhs.(*ast.IndexExpr)
		if !ok || !isRequestHeaderExpr(pass, index.X) {
			continue
		}
		name, ok := constantString(pass, index.Index)
		if !ok {
			continue
		}
		field, special := requestSpecialFields[name]
		if !special {
			continue
		}
		pass.Reportf(index.Index.Pos(),
			"request header %q is represented by http.Request.%s; mutating Request.Header is ignored",
			name, field)
	}
}

// deleteFromRequestHeader matches delete(r.Header, "Name") and returns the
// key with the argument that carries it. Like map indexing, the delete
// builtin does not canonicalise, so the key is matched exactly as written.
func deleteFromRequestHeader(pass *analysis.Pass, call *ast.CallExpr) (string, ast.Expr, bool) {
	id, ok := call.Fun.(*ast.Ident)
	if !ok || id.Name != "delete" || len(call.Args) != 2 {
		return "", nil, false
	}
	if _, builtin := pass.TypesInfo.Uses[id].(*types.Builtin); !builtin {
		return "", nil, false
	}
	if !isRequestHeaderExpr(pass, call.Args[0]) {
		return "", nil, false
	}
	name, ok := constantString(pass, call.Args[1])
	return name, call.Args[1], ok
}

func constantString(pass *analysis.Pass, expr ast.Expr) (string, bool) {
	tv, ok := pass.TypesInfo.Types[expr]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

func isDirectRequestHeaderMutation(pass *analysis.Pass, call *ast.CallExpr) bool {
	method, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (method.Sel.Name != "Set" && method.Sel.Name != "Add" && method.Sel.Name != "Del") {
		return false
	}

	return isRequestHeaderExpr(pass, method.X)
}

// isRequestHeaderExpr reports whether expr is the Header field of an
// http.Request.
func isRequestHeaderExpr(pass *analysis.Pass, expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Header" {
		return false
	}

	return isHTTPRequest(pass.TypesInfo.TypeOf(sel.X))
}

func isHTTPRequest(t types.Type) bool {
	t = types.Unalias(t)
	if pointer, ok := t.(*types.Pointer); ok {
		t = types.Unalias(pointer.Elem())
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Name() == "Request" && obj.Pkg() != nil && obj.Pkg().Path() == "net/http"
}

func isStatuteRequestHeaderConstructor(pass *analysis.Pass, call *ast.CallExpr) bool {
	fn := calledFunction(pass, call)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != statuteImportPath {
		return false
	}
	_, ok := requestHeaderConstructors[fn.Name()]
	return ok
}

func calledFunction(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	var obj types.Object
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		obj = pass.TypesInfo.Uses[fun]
	case *ast.SelectorExpr:
		obj = pass.TypesInfo.Uses[fun.Sel]
	}
	fn, _ := obj.(*types.Func)
	return fn
}
