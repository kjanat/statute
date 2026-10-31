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
	"main": true, "init": true, "configuration": true, "migrationEnv": true, "migrationUsersFile": true, "migrationNoop": true, "makePool": true, rateLimitSettingsType: true, "statute": true,
	"generatedListeners": true, "generatedUpstreams": true, "generatedRoutes": true, "generatedDocker": true,
}

type middlewarePart struct {
	call   string
	single bool
	code   []expr
}

type reusable struct {
	name    string
	typ     string
	body    expr
	origins []string
}

type reusableKey struct {
	typ string
	key string
}

func (m middleware) segments() []middlewarePart {
	if len(m.code) == 0 {
		return m.parts
	}
	return append([]middlewarePart{{code: m.code}}, m.parts...)
}

func (p middlewarePart) elements() ([]expr, bool) {
	switch {
	case p.call == "":
		return p.code, true
	case p.single:
		return []expr{textExpr(p.call)}, true
	default:
		return nil, false
	}
}

func sliceExpr(parts []middlewarePart) expr {
	var exprs, run []expr
	flush := func() {
		if len(run) > 0 {
			exprs = append(exprs, wrapExpr(middlewareType+"{", run, "}"))
			run = nil
		}
	}
	for _, p := range parts {
		if elements, ok := p.elements(); ok {
			run = append(run, elements...)
			continue
		}
		flush()
		exprs = append(exprs, textExpr(p.call))
	}
	flush()
	if len(exprs) == 1 {
		return exprs[0]
	}
	return wrapExpr("slices.Concat(", exprs, ")")
}

func variadicArgs(parts []middlewarePart) expr {
	var args []expr
	for _, p := range parts {
		elements, ok := p.elements()
		if !ok {
			return wrapExpr("", []expr{sliceExpr(parts)}, "...")
		}
		args = append(args, elements...)
	}
	return wrapExpr("", args, "")
}

func withCalls(method string, parts []middlewarePart) string {
	var b strings.Builder
	var run []expr
	flush := func() {
		if len(run) > 0 {
			b.WriteString(inline.render(wrapExpr("."+method+"(", run, ")")))
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

func (g *generator) reuse(typ, key, base string, body expr, origins ...string) string {
	if h := g.reusables[reusableKey{typ, key}]; h != nil {
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
	g.reusables[reusableKey{typ, key}] = &reusable{name: name, typ: typ, body: body, origins: origins}
	return name + "()"
}

func (g *generator) reservedName(name string) bool {
	_, imported := importedPackages[name]
	_, shared := rateLimitConstructors[name]
	return g.reusableNames[name] || generatedIdentifiers[name] || shared || imported || token.IsKeyword(name) || types.Universe.Lookup(name) != nil
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
	shared := sharedConstructors(used)
	var b strings.Builder
	for _, name := range keys(used) {
		h := used[name]
		code, err := layoutExpr(shared.render(h.body))
		if err != nil {
			return "", err
		}
		b.WriteString("\n")
		for _, origin := range h.origins {
			b.WriteString(originComment(origin) + "\n")
		}
		b.WriteString("func " + h.name + "() " + h.typ + " {\nreturn " + code + "\n}\n")
	}
	if len(shared) > 0 {
		b.WriteString(rateLimitSettingsDecl)
	}
	for _, name := range keys(shared) {
		b.WriteString("\nfunc " + name + "(s " + rateLimitSettingsType + ") " + singleMiddlewareType + " {\nreturn " + rateLimitConstructors[name] + "\n}\n")
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
			expr, err := parser.ParseExpr(inline.render(h.body))
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

func sharedConstructors(used map[string]*reusable) renderer {
	users := map[string]int{}
	for _, h := range used {
		found := map[string]bool{}
		h.body.constructors(found)
		for name := range found {
			users[name]++
		}
	}
	shared := renderer{}
	for name, n := range users {
		if n > 1 {
			shared[name] = true
		}
	}
	return shared
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
