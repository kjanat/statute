package migrate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
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

type constructor struct {
	signature  string
	body       string
	expansions map[string]string
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

func (g *generator) middlewareHelper(key string, parts []middlewarePart, origins ...string) middlewarePart {
	if len(parts) == 1 {
		if elements, ok := parts[0].elements(); ok && len(elements) == 1 {
			return middlewarePart{call: g.reuse(singleMiddlewareType, key, helperIdentifier(key), elements[0], origins...), single: true}
		}
	}
	return middlewarePart{call: g.reuse(middlewareType, key, helperIdentifier(key), sliceExpr(parts), origins...)}
}

func (g *generator) reuse(typ, key, base, body string, origins ...string) string {
	if h := g.reusables[key]; h != nil {
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
	g.reusables[key] = &reusable{name: name, typ: typ, body: body, origins: origins}
	return name + "()"
}

func (g *generator) reservedName(name string) bool {
	_, imported := importedPackages[name]
	_, shared := constructorSignatures[name]
	return g.reusableNames[name] || generatedIdentifiers[name] || shared || imported || token.IsKeyword(name) || types.Universe.Lookup(name) != nil
}

func (g *generator) construct(name, body, expansion, call string) {
	c := g.constructors[name]
	if c == nil {
		c = &constructor{signature: constructorSignatures[name], body: body, expansions: map[string]string{}}
		g.constructors[name] = c
	}
	c.expansions[expansion] = call
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
	used, err := g.usedReusables(body)
	if err != nil {
		return "", err
	}
	bodies, shared := g.sharedConstruction(used)
	var b strings.Builder
	for _, name := range keys(used) {
		h := used[name]
		code, err := layoutExpr(bodies[name])
		if err != nil {
			return "", err
		}
		b.WriteString("\n")
		for _, origin := range h.origins {
			b.WriteString(originComment(origin) + "\n")
		}
		b.WriteString("func " + h.name + "() " + h.typ + " {\nreturn " + code + "\n}\n")
	}
	for _, name := range shared {
		c := g.constructors[name]
		code, err := layoutExpr(c.body)
		if err != nil {
			return "", err
		}
		b.WriteString("\nfunc " + name + c.signature + " " + singleMiddlewareType + " {\nreturn " + code + "\n}\n")
	}
	return b.String(), nil
}

func (g *generator) usedReusables(body string) (map[string]*reusable, error) {
	byName := map[string]*reusable{}
	for _, h := range g.reusables {
		byName[h.name] = h
	}
	file, err := parser.ParseFile(token.NewFileSet(), "", "package main\n"+body, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	used := map[string]*reusable{}
	pending := []ast.Node{file}
	for len(pending) > 0 {
		for name := range calledFunctions(pending[0]) {
			h := byName[name]
			if h == nil || used[name] != nil {
				continue
			}
			expr, err := parser.ParseExpr(h.body)
			if err != nil {
				return nil, err
			}
			used[name] = h
			pending = append(pending, expr)
		}
		pending = pending[1:]
	}
	return used, nil
}

func (g *generator) sharedConstruction(used map[string]*reusable) (map[string]string, []string) {
	bodies := map[string]string{}
	for name, h := range used {
		bodies[name] = h.body
	}
	var shared []string
	for _, name := range keys(g.constructors) {
		c := g.constructors[name]
		var users []string
		for _, helper := range keys(bodies) {
			for expansion := range c.expansions {
				if strings.Contains(bodies[helper], expansion) {
					users = append(users, helper)
					break
				}
			}
		}
		if len(users) < 2 {
			continue
		}
		shared = append(shared, name)
		for _, helper := range users {
			for expansion, call := range c.expansions {
				bodies[helper] = strings.ReplaceAll(bodies[helper], expansion, call)
			}
		}
	}
	return bodies, shared
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
