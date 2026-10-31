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
		realm = sourceTraefik
	}
	if !literal(realm) {
		return
	}
	chain.values = append(chain.values, statute.BasicAuth(realm, map[string]string{"migration-placeholder": validationHash}))
	chain.code = append(chain.code, textExpr(fmt.Sprintf("statute.BasicAuth(%q,%s)", realm, g.usersFile(path))))
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
	chain.code = append(chain.code, textExpr(fmt.Sprintf("statute.BasicAuth(%q,map[string]string{%s})", realm, strings.Join(entries, ","))))
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
	chain.code = append(chain.code, textExpr(fmt.Sprintf("statute.%s%sHeader(%q%s)", operation, direction, name, codeValue)))
}

func (g *generator) nativeRewrite(o *Object, chain *middleware) {
	if prefix := o.text("addPrefix.prefix"); prefix != "" && literal(prefix) {
		chain.values = append(chain.values, statute.PrefixPath(prefix))
		chain.code = append(chain.code, textExpr(fmt.Sprintf("statute.PrefixPath(%q)", prefix)))
		o.mark("addPrefix.prefix", Translated, "prefix applied at its declared middleware position")
	}
	if path := o.text("replacePath.path"); path != "" && literal(path) {
		if _, err := url.PathUnescape(path); err == nil {
			chain.values = append(chain.values, statute.ReplaceEscapedPath(path).RecordOriginal())
			chain.code = append(chain.code, textExpr(fmt.Sprintf("statute.ReplaceEscapedPath(%q).RecordOriginal()", path)))
			o.mark("replacePath.path", Translated, "escaped replacement with X-Replaced-Path at its declared middleware position")
		}
	}
	g.regexRewrite(o, chain)
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
		chain.code = append(chain.code, textExpr(code))
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
	if !sourceCompressionMinimum(o, &p) {
		return
	}
	fallback, ok := sourceLiteralText(o, "compress.defaultEncoding")
	if !ok {
		o.mark("compress.defaultEncoding", Manual, "default encoding requires a literal string")
		return
	}
	p.DefaultEncoding = fallback
	if !sourceCompressionTypes(o, &p) {
		return
	}
	chain.values = append(chain.values, statute.EncodeResponse(p))
	chain.code = append(chain.code, textExpr(fmt.Sprintf("statute.EncodeResponse(statute.EncodingPolicy{Encodings:[]string{%s},MinimumBytes:%d,DefaultEncoding:%q,IncludedTypes:[]string{%s},ExcludedTypes:[]string{%s}})", quoted(p.Encodings), p.MinimumBytes, p.DefaultEncoding, quoted(p.IncludedTypes), quoted(p.ExcludedTypes))))
	o.markConsumed("compress", []string{"encodings", "defaultEncoding", "minResponseBodyBytes", "includedContentTypes", "excludedContentTypes"}, "response encoding, media filters and minimum body size")
}

func sourceCompressionTypes(o *Object, p *statute.EncodingPolicy) bool {
	for _, field := range []struct {
		key string
		dst *[]string
	}{{"includedContentTypes", &p.IncludedTypes}, {"excludedContentTypes", &p.ExcludedTypes}} {
		if o.value("compress."+field.key) != nil {
			values, ok := stringList(o.value("compress."+field.key), o.Provider == providerDocker)
			if !ok {
				return false
			}
			*field.dst = values
		}
	}
	return true
}

func (g *generator) buffering(o *Object, chain *middleware) {
	if o.value("buffering") == nil && !hasFieldPrefix(o, "buffering.") {
		return
	}
	p := statute.BodyBufferPolicy{}
	if !sourceBufferSizes(o, &p) {
		return
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
	retry, ok := sourceLiteralText(o, "buffering.retryExpression")
	if !ok {
		o.mark("buffering.retryExpression", Manual, "retry expression requires a literal string")
		return
	}
	p.RetryExpression = retry
	noteBufferBounds(o, p)
	chain.values = append(chain.values, statute.BufferBodies(p))
	chain.code = append(chain.code, textExpr(fmt.Sprintf("statute.BufferBodies(statute.BodyBufferPolicy{MaxRequestBytes:%d,MemoryRequestBytes:%d,MaxResponseBytes:%d,MemoryResponseBytes:%d,DisableRequest:%t,DisableResponse:%t,RetryExpression:%q})", p.MaxRequestBytes, p.MemoryRequestBytes, p.MaxResponseBytes, p.MemoryResponseBytes, p.DisableRequest, p.DisableResponse, p.RetryExpression)))
	o.markConsumed("buffering", []string{"maxRequestBodyBytes", "memRequestBodyBytes", "maxResponseBodyBytes", "memResponseBodyBytes", "disableRequestBuffer", "disableResponseBuffer", "retryExpression"}, "request and response buffering with disk spill and retry predicate")
}

func sourceBufferSizes(o *Object, p *statute.BodyBufferPolicy) bool {
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
				return false
			}
			*field.dst = int64(n)
		}
	}
	return true
}

func noteBufferBounds(o *Object, p statute.BodyBufferPolicy) {
	if (!p.DisableRequest && p.MaxRequestBytes == 0) || (!p.DisableResponse && p.MaxResponseBytes == 0) {
		o.note("$body-limits", Translated, "unlimited source body size becomes the native 8 MiB maximum")
	}
	if !p.DisableRequest || !p.DisableResponse {
		o.note("$in-flight", Translated, "native buffering admits 128 concurrent requests per route middleware")
	}
}

func sourceLiteralText(o *Object, path string) (string, bool) {
	value := o.value(path)
	if value == nil {
		return "", true
	}
	text, ok := value.(string)
	return text, ok && literal(text)
}

func (g *generator) retry(o *Object, chain *middleware) {
	attempts, err := integer(o.value("retry.attempts"))
	if err != nil || attempts <= 0 {
		return
	}
	chain.values = append(chain.values, statute.Retry(attempts))
	chain.code = append(chain.code, textExpr(fmt.Sprintf("statute.Retry(%d)", attempts)))
	o.mark("retry.attempts", Manual, "candidate attempt budget; native Retry buffers responses, retries configured statuses and bypasses streaming/non-idempotent requests, whereas Traefik tracks transport failures before response commit")
}

func (g *generator) retainMiddlewareCandidate(o *Object, chain middleware) {
	if o.ready() || len(chain.segments()) == 0 {
		return
	}
	// A blocked candidate remains reviewable without being registered on a route.
	// The whole generated program is additionally guarded by the audit result.
	name := candidatePrefix + strconv.Itoa(len(g.candidates))
	g.candidates[name] = fragment{origin: o.identity(), code: inline.render(sliceExpr(chain.segments()))}
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
		realm = sourceTraefik
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

func (g *generator) regexRewrite(o *Object, chain *middleware) {
	if o.value("replacePathRegex") != nil || hasFieldPrefix(o, "replacePathRegex.") {
		pattern, replacement := o.text("replacePathRegex.regex"), o.text("replacePathRegex.replacement")
		if _, err := regexp.Compile(strings.TrimSpace(pattern)); err == nil && !strings.Contains(pattern, "${") && !strings.Contains(pattern+replacement, "{{") {
			chain.values = append(chain.values, statute.RewriteEscapedPath(pattern, replacement).RecordOriginal())
			chain.code = append(chain.code, textExpr(fmt.Sprintf("statute.RewriteEscapedPath(%q,%q).RecordOriginal()", pattern, replacement)))
			o.mark("replacePathRegex.regex", Translated, "escaped path regexp")
			o.mark("replacePathRegex.replacement", Translated, "capture replacement with X-Replaced-Path")
		}
	}
}

func sourceCompressionMinimum(o *Object, p *statute.EncodingPolicy) bool {
	if value := o.value("compress.minResponseBodyBytes"); value != nil {
		n, err := integer(value)
		if err != nil {
			o.mark("compress.minResponseBodyBytes", Manual, "minimum body size must be an integer")
			return false
		}
		if n > 0 {
			p.MinimumBytes = n
		}
	}
	return true
}
