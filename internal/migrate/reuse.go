package migrate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const (
	middlewareType       = "[]statute.Middleware"
	singleMiddlewareType = "statute.Middleware"
	transportType        = "statute.Transport"
	candidatePrefix      = "migrationCandidate"
)

var generatedIdentifiers = map[string]bool{
	"main": true, "init": true, "configuration": true, "migrationEnv": true, "migrationUsersFile": true, "statute": true,
	"generatedListeners": true, "generatedUpstreams": true, "generatedRoutes": true, "generatedFallbackRoutes": true, "generatedDocker": true,
}

type middlewarePart struct {
	call   string
	single bool
	code   []string
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

func (p middlewarePart) elements() ([]string, bool) {
	switch {
	case p.call == "":
		return p.code, true
	case p.single:
		return []string{p.call}, true
	default:
		return nil, false
	}
}

func sliceExpr(parts []middlewarePart) string {
	var exprs, run []string
	flush := func() {
		if len(run) > 0 {
			exprs = append(exprs, middlewareType+"{"+strings.Join(run, ",")+"}")
			run = nil
		}
	}
	for _, p := range parts {
		if elements, ok := p.elements(); ok {
			run = append(run, elements...)
			continue
		}
		flush()
		exprs = append(exprs, p.call)
	}
	flush()
	if len(exprs) == 1 {
		return exprs[0]
	}
	return "slices.Concat(" + strings.Join(exprs, ",") + ")"
}

func variadicArgs(parts []middlewarePart) string {
	var args []string
	for _, p := range parts {
		elements, ok := p.elements()
		if !ok {
			return sliceExpr(parts) + "..."
		}
		args = append(args, elements...)
	}
	return strings.Join(args, ",")
}

func withCalls(method string, parts []middlewarePart) string {
	var b strings.Builder
	var run []string
	flush := func() {
		if len(run) > 0 {
			b.WriteString("." + method + "(" + strings.Join(run, ",") + ")")
			run = nil
		}
	}
	for _, p := range parts {
		if elements, ok := p.elements(); ok {
			run = append(run, elements...)
			continue
		}
		flush()
		b.WriteString("." + method + "(" + p.call + "...)")
	}
	flush()
	return b.String()
}

func (g *generator) middlewareHelper(name, origin string, parts []middlewarePart) middlewarePart {
	if len(parts) == 1 {
		if elements, ok := parts[0].elements(); ok && len(elements) == 1 {
			return middlewarePart{call: g.reuse(singleMiddlewareType, helperIdentifier(name), origin, elements[0]), single: true}
		}
	}
	return middlewarePart{call: g.reuse(middlewareType, helperIdentifier(name), origin, sliceExpr(parts))}
}

func (g *generator) reuse(typ, base, origin, body string) string {
	key := typ + "\x00" + body
	if h := g.reusables[key]; h != nil {
		if !slices.Contains(h.origins, origin) {
			h.origins = append(h.origins, origin)
		}
		return h.name + "()"
	}
	if strings.HasPrefix(base, candidatePrefix) {
		base = "source" + strings.ToUpper(base[:1]) + base[1:]
	}
	name := base
	for n := 2; g.reservedName(name); n++ {
		name = base + strconv.Itoa(n)
	}
	g.reusableNames[name] = true
	g.reusables[key] = &reusable{name: name, typ: typ, body: body, origins: []string{origin}}
	return name + "()"
}

func (g *generator) reservedName(name string) bool {
	_, imported := importedPackages[name]
	return g.reusableNames[name] || generatedIdentifiers[name] || imported || token.IsKeyword(name) || types.Universe.Lookup(name) != nil
}

func helperIdentifier(source string) string {
	source, _, _ = strings.Cut(source, "@")
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
	file, err := parser.ParseFile(token.NewFileSet(), "", "package main\n"+body, parser.SkipObjectResolution)
	if err != nil {
		return "", err
	}
	code := map[string]string{}
	pending := []ast.Node{file}
	for len(pending) > 0 {
		for name := range calledFunctions(pending[0]) {
			h := byName[name]
			if h == nil || code[name] != "" {
				continue
			}
			l := layout{fset: token.NewFileSet()}
			expr, err := parser.ParseExprFrom(l.fset, "", h.body, 0)
			if err != nil {
				return "", err
			}
			code[name] = l.expr(expr)
			pending = append(pending, expr)
		}
		pending = pending[1:]
	}
	var b strings.Builder
	for _, name := range keys(code) {
		h := byName[name]
		b.WriteString("\n")
		for _, origin := range h.origins {
			b.WriteString(originComment(origin) + "\n")
		}
		b.WriteString("func " + h.name + "() " + h.typ + " {\nreturn " + code[name] + "\n}\n")
	}
	return b.String(), nil
}

func calledFunctions(node ast.Node) map[string]bool {
	calls := map[string]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				calls[id.Name] = true
			}
		}
		return true
	})
	return calls
}
