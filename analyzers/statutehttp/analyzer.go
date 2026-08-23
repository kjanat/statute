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
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}

			name, ok := constantString(pass, call.Args[0])
			if !ok {
				return true
			}
			canonical := textproto.CanonicalMIMEHeaderKey(name)
			field, special := requestSpecialFields[canonical]
			if !special {
				return true
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

			return true
		})
	}

	return nil, nil
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

	header, ok := method.X.(*ast.SelectorExpr)
	if !ok || header.Sel.Name != "Header" {
		return false
	}

	return isHTTPRequest(pass.TypesInfo.TypeOf(header.X))
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
