package migrate

import (
	"fmt"
	"net/url"
	"regexp"
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
	} else if publicHeaderValue(name, value) {
		codeValue = "," + strconv.Quote(value)
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
	if prefix := o.text("addPrefix.prefix"); prefix != "" && literal(prefix) {
		chain.values = append(chain.values, statute.PrefixPath(prefix))
		chain.code = append(chain.code, fmt.Sprintf("statute.PrefixPath(%q)", prefix))
		o.mark("addPrefix.prefix", Translated, "prefix applied at its declared middleware position")
	}
	if path := o.text("replacePath.path"); path != "" && literal(path) {
		if _, err := url.PathUnescape(path); err == nil {
			chain.values = append(chain.values, statute.ReplaceEscapedPath(path).RecordOriginal())
			chain.code = append(chain.code, fmt.Sprintf("statute.ReplaceEscapedPath(%q).RecordOriginal()", path))
			o.mark("replacePath.path", Translated, "escaped replacement with X-Replaced-Path at its declared middleware position")
		}
	}
	if o.value("replacePathRegex") != nil || hasFieldPrefix(o, "replacePathRegex.") {
		pattern, replacement := o.text("replacePathRegex.regex"), o.text("replacePathRegex.replacement")
		if _, err := regexp.Compile(strings.TrimSpace(pattern)); err == nil && !strings.Contains(pattern, "${") && !strings.Contains(pattern+replacement, "{{") {
			chain.values = append(chain.values, statute.RewriteEscapedPath(pattern, replacement).RecordOriginal())
			chain.code = append(chain.code, fmt.Sprintf("statute.RewriteEscapedPath(%q,%q).RecordOriginal()", pattern, replacement))
			o.mark("replacePathRegex.regex", Translated, "escaped path regexp")
			o.mark("replacePathRegex.replacement", Translated, "capture replacement with X-Replaced-Path")
		}
	}
	if prefixes, ok := stringList(o.value("stripPrefix.prefixes"), o.Provider == providerDocker); ok {
		force, valid := sourceBoolValue(o.value("stripPrefix.forceSlash"), false)
		if !valid {
			o.mark("stripPrefix.forceSlash", Manual, "invalid forceSlash boolean")
			return
		}
		value := statute.StripBytePrefixes(prefixes...).RecordOriginal()
		code := "statute.StripBytePrefixes(" + quoted(prefixes) + ").RecordOriginal()"
		if force {
			value.ForceSlash()
			code += ".ForceSlash()"
		}
		chain.values = append(chain.values, value)
		chain.code = append(chain.code, code)
		o.mark("stripPrefix.prefixes", Translated, "ordered byte-prefix stripping with X-Forwarded-Prefix")
		o.mark("stripPrefix.forceSlash", Translated, "explicit empty-path slash behavior")
	}
}

func (g *generator) compression(o *Object, chain *middleware) {
	if o.value("compress") == nil && !hasFieldPrefix(o, "compress.") {
		return
	}
	p := statute.EncodingPolicy{Encodings: []string{"gzip", "br", "zstd"}, MinimumBytes: 1024}
	if o.value("compress.encodings") != nil {
		values, ok := stringList(o.value("compress.encodings"), o.Provider == providerDocker)
		if !ok || len(values) == 0 {
			o.mark("compress.encodings", Manual, "encodings must be a nonempty list")
			return
		}
		p.Encodings = values
	}
	if value := o.value("compress.minResponseBodyBytes"); value != nil {
		n, err := integer(value)
		if err != nil {
			o.mark("compress.minResponseBodyBytes", Manual, "minimum body size must be an integer")
			return
		}
		if n > 0 {
			p.MinimumBytes = n
		}
	}
	p.DefaultEncoding = o.text("compress.defaultEncoding")
	for _, field := range []struct {
		key string
		dst *[]string
	}{{"includedContentTypes", &p.IncludedTypes}, {"excludedContentTypes", &p.ExcludedTypes}} {
		if o.value("compress."+field.key) != nil {
			values, ok := stringList(o.value("compress."+field.key), o.Provider == providerDocker)
			if !ok {
				return
			}
			*field.dst = values
		}
	}
	chain.values = append(chain.values, statute.EncodeResponse(p))
	chain.code = append(chain.code, fmt.Sprintf("statute.EncodeResponse(statute.EncodingPolicy{Encodings:[]string{%s},MinimumBytes:%d,DefaultEncoding:%q,IncludedTypes:[]string{%s},ExcludedTypes:[]string{%s}})", quoted(p.Encodings), p.MinimumBytes, p.DefaultEncoding, quoted(p.IncludedTypes), quoted(p.ExcludedTypes)))
	o.mark("compress", Translated, "response encoding, media filters and minimum body size")
}

func (g *generator) buffering(o *Object, chain *middleware) {
	if o.value("buffering") == nil && !hasFieldPrefix(o, "buffering.") {
		return
	}
	p := statute.BodyBufferPolicy{}
	for _, field := range []struct {
		key string
		dst *int64
	}{
		{"maxRequestBodyBytes", &p.MaxRequestBytes},
		{"memRequestBodyBytes", &p.MemoryRequestBytes},
		{"maxResponseBodyBytes", &p.MaxResponseBytes},
		{"memResponseBodyBytes", &p.MemoryResponseBytes},
	} {
		if value := o.value("buffering." + field.key); value != nil {
			n, err := integer(value)
			if err != nil || n < 0 {
				o.mark("buffering."+field.key, Manual, "buffer size must be a nonnegative integer")
				return
			}
			*field.dst = int64(n)
		}
	}
	for _, field := range []struct {
		key string
		dst *bool
	}{{"disableRequestBuffer", &p.DisableRequest}, {"disableResponseBuffer", &p.DisableResponse}} {
		value, ok := sourceBoolValue(o.value("buffering."+field.key), false)
		if !ok {
			o.mark("buffering."+field.key, Manual, "invalid buffer switch")
			return
		}
		*field.dst = value
	}
	p.RetryExpression = o.text("buffering.retryExpression")
	if !literal(p.RetryExpression) {
		o.mark("buffering.retryExpression", Manual, "retry expression contains unresolved interpolation")
		return
	}
	chain.values = append(chain.values, statute.BufferBodies(p))
	chain.code = append(chain.code, fmt.Sprintf("statute.BufferBodies(statute.BodyBufferPolicy{MaxRequestBytes:%d,MemoryRequestBytes:%d,MaxResponseBytes:%d,MemoryResponseBytes:%d,DisableRequest:%t,DisableResponse:%t,RetryExpression:%q})", p.MaxRequestBytes, p.MemoryRequestBytes, p.MaxResponseBytes, p.MemoryResponseBytes, p.DisableRequest, p.DisableResponse, p.RetryExpression))
	o.mark("buffering", Translated, "request and response buffering with disk spill and retry predicate")
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
	// Source middleware groups execute at their declared position.
}

func sourceResponseHeaders(m *Object) map[string]string {
	values := map[string]string{}
	for _, f := range []struct{ key, name, value string }{
		{"contentTypeNosniff", "x-content-type-options", "nosniff"},
		{"frameDeny", "x-frame-options", "DENY"},
		{"browserXssFilter", "x-xss-protection", "1; mode=block"},
	} {
		if scalar(m.value("headers."+f.key)) == trueValue {
			values[f.name] = f.value
		}
	}
	for _, f := range []struct{ key, name string }{
		{fieldReferrerPolicy, "referrer-policy"},
		{"customFrameOptionsValue", "x-frame-options"},
		{"contentSecurityPolicy", "content-security-policy"},
		{"permissionsPolicy", "permissions-policy"},
	} {
		if value := m.text("headers." + f.key); value != "" {
			values[f.name] = value
		}
	}
	if value := sourceHSTSValue(m); value != "" {
		values["strict-transport-security"] = value
	}

	const prefix = "headers.customresponseheaders."
	for _, f := range m.Fields {
		if path := strings.ToLower(f.Path); strings.HasPrefix(path, prefix) {
			values[strings.TrimPrefix(path, prefix)] = scalar(m.value(f.Path))
		}
	}
	return values
}

func hasFieldPrefix(o *Object, prefix string) bool {
	prefix = strings.ToLower(prefix)
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

func sourceHSTSValue(m *Object) string {
	seconds, err := integer(m.value("headers.stsSeconds"))
	if err != nil || seconds <= 0 {
		return ""
	}
	value := fmt.Sprintf("max-age=%d", seconds)
	if scalar(m.value("headers.stsIncludeSubdomains")) == trueValue {
		value += "; includeSubDomains"
	}
	if scalar(m.value("headers.stsPreload")) == trueValue {
		value += "; preload"
	}
	return value
}
