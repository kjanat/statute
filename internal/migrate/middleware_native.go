package migrate

import (
	"fmt"
	"strconv"
	"strings"

	statute "statute.kjanat.dev"
)

const validationHash = "$2a$10$N9qo8uLOickgx2ZMRZoMye.IjPeMRZjnOcTBE3UCJfo4xVEhYqL0K"

func (g *generator) basicAuth(o *Object, chain *middleware) {
	path := o.text("basicAuth.usersFile")
	if path == "" || !literal(path) {
		g.inlineBasicAuth(o, chain)
		return
	}
	realm := o.text("basicAuth.realm")
	if realm == "" {
		realm = "traefik"
	}
	if !literal(realm) {
		return
	}
	chain.values = append(chain.values, statute.BasicAuth(realm, map[string]string{"migration-placeholder": validationHash}))
	chain.code = append(chain.code, fmt.Sprintf("statute.BasicAuth(%q,%s)", realm, g.usersFile(path)))
	o.mark("basicAuth.usersFile", Translated, "authentication file reference retained; generated startup validates bcrypt hashes; audit does not read credentials")
	o.mark("basicAuth.realm", Translated, "explicit authentication realm including Traefik default")
	if scalar(o.value("basicAuth.removeHeader")) == falseValue {
		o.mark("basicAuth.removeHeader", Exact, "authenticated Authorization header remains on the upstream request")
	}
}

func (g *generator) inlineBasicAuth(o *Object, chain *middleware) {
	values, ok := inlineUsers(o)
	if !ok || len(values) == 0 || len(values) > 256 {
		return
	}
	users := map[string]string{}
	var entries []string
	for i, value := range values {
		name, hash, found := strings.Cut(stringValue(value), ":")
		if !found || !validAuthUser(name) {
			return
		}
		if _, exists := users[name]; exists {
			return
		}
		path := fmt.Sprintf("basicAuth.users[%d]", i)

		_, binding := g.sensitiveValue(o, path, hash)
		users[name] = validationHash
		entries = append(entries, fmt.Sprintf("%q:%s", name, binding))
	}
	realm, ok := authenticationRealm(o)
	if !ok {
		return
	}
	chain.values = append(chain.values, statute.BasicAuth(realm, users))
	chain.code = append(chain.code, fmt.Sprintf("statute.BasicAuth(%q,map[string]string{%s})", realm, strings.Join(entries, ",")))
	o.mark("basicAuth.realm", Translated, "explicit authentication realm")
	classifyInlineUsers(o, values)
}

func (g *generator) headers(o *Object, chain *middleware) {
	for _, direction := range []string{"Request", "Response"} {
		base := "headers.custom" + direction + "Headers"
		if o.Provider == providerDocker {
			base = strings.ToLower(base)
		}
		for _, f := range o.Fields {
			if !strings.HasPrefix(f.Path, base+".") {
				continue
			}
			name := strings.TrimPrefix(f.Path, base+".")
			value, ok := o.value(f.Path).(string)
			if !ok || !literal(name) {
				continue
			}
			g.header(o, chain, direction, name, value, f.Path)
		}
	}
}

func (g *generator) header(o *Object, chain *middleware, direction, name, value, path string) {
	if len(chain.values) >= 256 {
		o.note("$size", Unsupported, "expanded middleware exceeds 256 operations")
		return
	}
	operation := "Set"
	codeValue := ""
	o.mark(path, Translated, "route-owned header operation; relative order checked when chains are assembled")
	if value == "" {
		operation = removeOperation
	} else {
		var code string
		value, code = g.sensitiveValue(o, path, value)
		codeValue = "," + code
	}
	if direction == "Request" {
		if operation == removeOperation {
			chain.values = append(chain.values, statute.RemoveRequestHeader(name))
		} else {
			chain.values = append(chain.values, statute.SetRequestHeader(name, value))
		}
	} else {
		if operation == removeOperation {
			chain.values = append(chain.values, statute.RemoveResponseHeader(name))
		} else {
			chain.values = append(chain.values, statute.SetResponseHeader(name, value))
		}
	}
	chain.code = append(chain.code, fmt.Sprintf("statute.%s%sHeader(%q%s)", operation, direction, name, codeValue))
}

func (g *generator) nativeRewrite(o *Object, chain *middleware) {
	prefix := o.text("addPrefix.prefix")
	if strings.HasPrefix(prefix, "/") && !strings.HasSuffix(prefix, "/") && !strings.ContainsAny(prefix, "%?#") && literal(prefix) {
		chain.values = append(chain.values, statute.AddPrefix(prefix))
		chain.code = append(chain.code, fmt.Sprintf("statute.AddPrefix(%q)", prefix))
		o.mark("addPrefix.prefix", Translated, "literal prefix applied once at native route entry; chain order and retry interactions checked separately")
	}
	if path := o.text("replacePath.path"); path != "" && literal(path) {
		chain.values = append(chain.values, statute.ReplacePath(path))
		chain.code = append(chain.code, fmt.Sprintf("statute.ReplacePath(%q)", path))
		o.mark("replacePath.path", Manual, "native ReplacePath candidate lacks Traefik's dynamic X-Replaced-Path header and hoists mutation before middleware")
	}
	if prefixes, ok := stringList(o.value("stripPrefix.prefixes"), o.Provider == providerDocker); ok && len(prefixes) == 1 {
		chain.values = append(chain.values, statute.StripPrefix(prefixes[0]))
		chain.code = append(chain.code, fmt.Sprintf("statute.StripPrefix(%q)", prefixes[0]))
		o.mark("stripPrefix.prefixes", Manual, "native StripPrefix candidate matches segments and omits Traefik's dynamic X-Forwarded-Prefix header; Traefik strips byte prefixes")
	}
}

func (g *generator) compression(o *Object, chain *middleware) {
	if o.value("compress") == nil && !hasFieldPrefix(o, "compress.") {
		return
	}
	chain.values = append(chain.values, statute.Compress(statute.Gzip, statute.Brotli))
	chain.code = append(chain.code, "statute.Compress(statute.Gzip,statute.Brotli)")
	o.mark("compress", Manual, "native gzip/Brotli candidate has no zstd, content-type allow/deny lists or minimum response size; negotiation and streaming differ")
}

func (g *generator) buffering(o *Object, chain *middleware) {
	maximum, err := integer(o.value("buffering.maxRequestBodyBytes"))
	if err != nil || maximum <= 0 {
		return
	}
	size := strconv.Itoa(maximum)
	m := statute.RequestBuffer(size)
	code := fmt.Sprintf("statute.RequestBuffer(%q)", size)
	if threshold, err := integer(o.value("buffering.memRequestBodyBytes")); err == nil && threshold > 0 && threshold <= maximum {
		m.MemoryThreshold(strconv.Itoa(threshold))
		code += fmt.Sprintf(".MemoryThreshold(%q)", strconv.Itoa(threshold))
		o.mark("buffering.memRequestBodyBytes", Manual, "candidate memory threshold needs explicit server-owned spill directory and aggregate disk budget")
	}
	chain.values = append(chain.values, m)
	chain.code = append(chain.code, code)
	o.mark("buffering.maxRequestBodyBytes", Manual, "candidate preserves request size ceiling; native shared 64 MiB/128-slot admission may return 503, unlike unbounded Traefik spill; choose server resources explicitly")
	o.mark("buffering.maxResponseBodyBytes", Unsupported, "native RequestBuffer does not buffer or limit responses; Retry response buffering is not a standalone substitute")
	o.mark("buffering.memResponseBodyBytes", Unsupported, "native response streaming has no equivalent disk spill threshold")
	o.mark("buffering.retryExpression", Unsupported, "Traefik buffering retry expressions have no native expression evaluator")
}

func (g *generator) retry(o *Object, chain *middleware) {
	attempts, err := integer(o.value("retry.attempts"))
	if err != nil || attempts <= 0 {
		return
	}
	chain.values = append(chain.values, statute.Retry(attempts))
	chain.code = append(chain.code, fmt.Sprintf("statute.Retry(%d)", attempts))
	o.mark("retry.attempts", Manual, "candidate attempt budget; native Retry buffers responses, retries configured statuses and bypasses streaming/non-idempotent requests, whereas Traefik tracks transport failures before response commit")
}

func (g *generator) retainMiddlewareCandidate(o *Object, chain middleware) {
	if o.ready() || len(chain.code) == 0 {
		return
	}
	// A blocked candidate remains reviewable without being registered on a route.
	// The whole generated program is additionally guarded by the audit result.
	name := fmt.Sprintf("migrationCandidate%d", len(g.helpers))
	g.helpers[name] = fmt.Sprintf("// %s is incomplete; see the audit for %s.\nfunc %s() []statute.Middleware {return []statute.Middleware{%s}}", name, strconv.Quote(o.identity()), name, strings.Join(chain.code, ","))
}

func (g *generator) checkMiddlewareOrder(o *Object, refs []string) {
	if o.Provider == providerDocker {
		defaults, _ := g.defaultMiddlewareRefs()
		refs = append(defaults, refs...)
	}
	var expanded []*Object
	for _, ref := range refs {
		g.flattenMiddleware(qualified(ref, o.Provider), &expanded, map[string]bool{})
	}
	seenPolicy := false
	for _, m := range expanded {
		hoisted := m.value("headers") != nil || m.value("addPrefix") != nil
		if m.Provider == providerDocker {
			hoisted = hasFieldPrefix(m, "headers.") || hasFieldPrefix(m, "addprefix.")
		}
		if hoisted && seenPolicy {
			o.note("$middleware-order", Manual, "header/path operations after another middleware would be hoisted before it; denial responses, authentication inputs and Retry re-entry can change")
		}
		if !hoisted {
			seenPolicy = true
		}
	}
}

func hasFieldPrefix(o *Object, prefix string) bool {
	for _, f := range o.Fields {
		if strings.HasPrefix(strings.ToLower(f.Path), prefix) {
			return true
		}
	}
	return false
}

func (g *generator) flattenMiddleware(id string, out *[]*Object, seen map[string]bool) {
	if seen[id] || len(*out) >= 256 {
		return
	}
	seen[id] = true
	o := g.objects["middlewares:"+id]
	if o == nil {
		return
	}
	if refs, ok := stringList(o.value("chain.middlewares"), o.Provider == providerDocker); ok {
		for _, ref := range refs {
			g.flattenMiddleware(qualified(ref, o.Provider), out, seen)
		}
	} else {
		*out = append(*out, o)
	}
	delete(seen, id)
}

func inlineUsers(o *Object) ([]any, bool) {
	values, ok := o.value("basicAuth.users").([]any)
	if o.Provider == providerDocker {
		if value, valid := o.value("basicAuth.users").(string); valid {
			for part := range strings.SplitSeq(value, ",") {
				values = append(values, part)
			}
			ok = true
		}
	}
	return values, ok
}

func authenticationRealm(o *Object) (string, bool) {
	realm := o.text("basicAuth.realm")
	if realm == "" {
		realm = "traefik"
	}
	return realm, literal(realm)
}

func validAuthUser(name string) bool { return name != "" && literal(name) }

func classifyInlineUsers(o *Object, values []any) {
	for _, value := range values {
		_, hash, _ := strings.Cut(stringValue(value), ":")
		if explicitEnv(hash) == "" {
			o.mark("basicAuth.users", Manual, "inline credentials withheld; populate each generated environment binding with its bcrypt hash")
			return
		}
	}
	o.mark("basicAuth.users", Translated, "explicit bcrypt hash environment bindings; generated startup validates hashes")
}
