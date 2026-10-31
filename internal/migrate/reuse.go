package migrate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const (
	middlewareType = "[]statute.Middleware"
	transportType  = "statute.Transport"
)

type middlewarePart struct {
	call string
	code []string
}

type reusable struct {
	name    string
	typ     string
	body    string
	origins []string
}

func (m middleware) segments() []middlewarePart {
	if len(m.code) == 0 {
		return m.parts
	}
	return append([]middlewarePart{{code: m.code}}, m.parts...)
}

func sliceExpr(parts []middlewarePart) string {
	exprs := make([]string, len(parts))
	for i, p := range parts {
		exprs[i] = p.call
		if p.call == "" {
			exprs[i] = middlewareType + "{" + strings.Join(p.code, ",") + "}"
		}
	}
	if len(exprs) == 1 {
		return exprs[0]
	}
	return "slices.Concat(" + strings.Join(exprs, ",") + ")"
}

func variadicArgs(parts []middlewarePart) string {
	if len(parts) == 1 && parts[0].call == "" {
		return strings.Join(parts[0].code, ",")
	}
	return sliceExpr(parts) + "..."
}

func withCalls(method string, parts []middlewarePart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString("." + method + "(" + variadicArgs([]middlewarePart{p}) + ")")
	}
	return b.String()
}

func (g *generator) reuse(typ, source, origin, body string) string {
	key := typ + "\x00" + body
	if h := g.reusables[key]; h != nil {
		if !slices.Contains(h.origins, origin) {
			h.origins = append(h.origins, origin)
		}
		return h.name + "()"
	}
	base := helperIdentifier(source) + typ[strings.LastIndex(typ, ".")+1:]
	name := base
	for n := 2; g.reusableNames[name]; n++ {
		name = base + strconv.Itoa(n)
	}
	g.reusableNames[name] = true
	g.reusables[key] = &reusable{name: name, typ: typ, body: body, origins: []string{origin}}
	return name + "()"
}

func helperIdentifier(source string) string {
	var b strings.Builder
	upper := false
	for _, r := range source {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upper = b.Len() > 0
			continue
		}
		if b.Len() == 0 {
			if unicode.IsDigit(r) {
				b.WriteString("source")
			} else {
				r = unicode.ToLower(r)
			}
		} else if upper {
			r = unicode.ToUpper(r)
		}
		upper = false
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "source"
	}
	return b.String()
}

func (g *generator) reusableSource(body string) (string, error) {
	byName := map[string]*reusable{}
	for _, h := range g.reusables {
		byName[h.name] = h
	}
	used := map[string]bool{}
	pending := []string{body}
	for len(pending) > 0 {
		calls, err := calledFunctions(pending[0])
		if err != nil {
			return "", err
		}
		pending = pending[1:]
		for name := range calls {
			if h := byName[name]; h != nil && !used[name] {
				used[name] = true
				pending = append(pending, "func _() { _ = "+h.body+" }")
			}
		}
	}
	var b strings.Builder
	for _, name := range keys(used) {
		h := byName[name]
		code, err := layoutExpr(h.body)
		if err != nil {
			return "", err
		}
		b.WriteString("\n")
		for _, origin := range h.origins {
			b.WriteString(originComment(origin) + "\n")
		}
		b.WriteString("func " + h.name + "() " + h.typ + " {\nreturn " + code + "\n}\n")
	}
	return b.String(), nil
}

func calledFunctions(source string) (map[string]bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "", "package main\n"+source, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	calls := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				calls[id.Name] = true
			}
		}
		return true
	})
	return calls, nil
}
