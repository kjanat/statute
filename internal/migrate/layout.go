package migrate

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	inlineWidth  = 80
	inlineChains = 2
)

type fragment struct {
	origin string
	code   string
	key    string
	hosts  []string
	tail   string
}

func (f fragment) render() string {
	if f.hosts == nil {
		return f.code
	}
	return f.code + ".Hosts(" + quoted(f.hosts) + ")" + f.tail
}

func mergeHosts(items []fragment) []fragment {
	var out []fragment
	for _, item := range items {
		if n := len(out); n > 0 && item.hosts != nil && sameRoute(out[n-1], item) && !sharedHost(out[n-1].hosts, item.hosts) {
			out[n-1].hosts = append(slices.Clone(out[n-1].hosts), item.hosts...)
			continue
		}
		out = append(out, item)
	}
	return out
}

func sameRoute(a, b fragment) bool {
	return a.hosts != nil && a.origin == b.origin && a.code == b.code && a.tail == b.tail
}

func sharedHost(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if strings.EqualFold(x, y) {
				return true
			}
		}
	}
	return false
}

type layout struct {
	fset *token.FileSet
}

func layoutExpr(src string) (string, error) {
	l := layout{fset: token.NewFileSet()}
	expr, err := parser.ParseExprFrom(l.fset, "", src, 0)
	if err != nil {
		return "", err
	}
	return l.expr(expr), nil
}

var statusNames = map[int]string{
	http.StatusOK:                  "StatusOK",
	http.StatusNoContent:           "StatusNoContent",
	http.StatusMovedPermanently:    "StatusMovedPermanently",
	http.StatusFound:               "StatusFound",
	http.StatusSeeOther:            "StatusSeeOther",
	http.StatusTemporaryRedirect:   "StatusTemporaryRedirect",
	http.StatusPermanentRedirect:   "StatusPermanentRedirect",
	http.StatusNotFound:            "StatusNotFound",
	http.StatusGone:                "StatusGone",
	http.StatusInternalServerError: "StatusInternalServerError",
	http.StatusBadGateway:          "StatusBadGateway",
	http.StatusServiceUnavailable:  "StatusServiceUnavailable",
	http.StatusGatewayTimeout:      "StatusGatewayTimeout",
}

func statusCode(status int) string {
	if name, ok := statusNames[status]; ok {
		return "http." + name
	}
	return strconv.Itoa(status)
}

var importedPackages = map[string]string{"http": "net/http", "math": "math", "os": "os", "slices": "slices", "strings": "strings"}

func usedImports(source string) (map[string]bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "", source, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && importedPackages[id.Name] != "" {
				used[importedPackages[id.Name]] = true
			}
		}
		return true
	})
	return used, nil
}

func originComment(origin string) string {
	quoted := strconv.Quote(origin)
	return "// " + quoted[1:len(quoted)-1]
}

func (l layout) expr(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.CallExpr:
		return l.call(e)
	case *ast.CompositeLit:
		return l.composite(e)
	case *ast.KeyValueExpr:
		return l.expr(e.Key) + ": " + l.expr(e.Value)
	case *ast.UnaryExpr:
		return e.Op.String() + l.expr(e.X)
	default:
		return l.print(e)
	}
}

func (l layout) print(node ast.Node) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, l.fset, node); err != nil {
		return ""
	}
	return b.String()
}

func (l layout) call(e *ast.CallExpr) string {
	var chain []*ast.CallExpr
	base := e
	for {
		sel, ok := base.Fun.(*ast.SelectorExpr)
		if !ok {
			break
		}
		inner, ok := sel.X.(*ast.CallExpr)
		if !ok {
			break
		}
		chain = append(chain, base)
		base = inner
	}
	fun := l.expr(base.Fun)
	segments := []string{fun + l.arguments(fun, base)}
	for _, call := range slices.Backward(chain) {
		name := call.Fun.(*ast.SelectorExpr).Sel.Name
		segments = append(segments, name+l.arguments(name, call))
	}
	if inline := strings.Join(segments, "."); len(chain) <= inlineChains && fits(inline) {
		return inline
	}
	return strings.Join(segments, ".\n")
}

func (l layout) arguments(fun string, e *ast.CallExpr) string {
	args := make([]string, len(e.Args))
	for i, arg := range e.Args {
		args[i] = l.expr(arg)
	}
	if e.Ellipsis.IsValid() && len(args) > 0 {
		args[len(args)-1] += "..."
	}
	inline := "(" + strings.Join(args, ", ") + ")"
	if len(args) == 0 || fits(fun+inline) {
		return inline
	}
	if _, composite := e.Args[0].(*ast.CompositeLit); composite && len(args) == 1 {
		return inline
	}
	return "(\n" + strings.Join(args, ",\n") + ",\n)"
}

func (l layout) composite(e *ast.CompositeLit) string {
	typ := ""
	if e.Type != nil {
		typ = l.expr(e.Type)
	}
	var elts []string
	for _, elt := range e.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok && structField(kv) && zeroValue(kv.Value) {
			continue
		}
		elts = append(elts, l.expr(elt))
	}
	inline := typ + "{" + strings.Join(elts, ", ") + "}"
	if len(elts) == 0 || len(elts) == 1 && fits(inline) {
		return inline
	}
	return typ + "{\n" + strings.Join(elts, ",\n") + ",\n}"
}

func fits(text string) bool {
	return len(text) <= inlineWidth && !strings.Contains(text, "\n")
}

func structField(kv *ast.KeyValueExpr) bool {
	_, ok := kv.Key.(*ast.Ident)
	return ok
}

func zeroValue(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name == falseValue || e.Name == nilValue
	case *ast.BasicLit:
		return e.Value == "0" || e.Value == `""`
	case *ast.CompositeLit:
		for _, elt := range e.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok || !structField(kv) || !zeroValue(kv.Value) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
