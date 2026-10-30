package migrate

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"slices"
	"strings"

	statute "statute.kjanat.dev"
	"statute.kjanat.dev/internal/docker"
)

func parseRuleHosts(o *Object) ([]string, error) {
	ms, err := docker.ParseRule(o.text("rule"))
	var out []string
	for _, m := range ms {
		if m.Host != "" {
			out = append(out, m.Host)
		}
	}
	return out, err
}

func (g *generator) acme(name string, domains []string) (statute.ListenerOption, string) {
	o := g.objects["certificatesresolvers:"+name+"@static"]
	if o == nil {
		g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: kindTLS, Code: "missing-certificate-resolver", Blocking: true})
		return nil, ""
	}
	if len(domains) == 0 {
		o.note("$domains", Manual, "ACME resolver has no literal Host matcher or explicit certificate domains")
		return nil, ""
	}
	a := statute.AutoTLS(domains...)
	var builder strings.Builder
	builder.WriteString("statute.AutoTLS(" + quoted(domains) + ")")
	for _, f := range []struct {
		key, method string
		apply       func(string) *statute.AutoTLSConfig
	}{{"email", "Email", a.Email}, {"caServer", "Directory", a.Directory}} {
		v := o.text("acme." + f.key)
		if !acmeAccountValue(f.key, v) {
			continue
		}
		f.apply(v)
		fmt.Fprintf(&builder, ".%s(%q)", f.method, v)
		o.mark("acme."+f.key, Translated, "explicit ACME account parameter")
	}
	code := builder.String()
	if storage := o.text("acme.storage"); storage != "" && literal(storage) {
		a.Storage(storage + ".statute")
		code += fmt.Sprintf(".Storage(%q)", storage+".statute")
		o.mark("acme.storage", Translated, "new sibling .statute directory; Traefik JSON accounts and certificates are not copied; issuance starts with new native ACME state")
	} else {
		o.note("$storage", Manual, "supply persistent native ACME storage; source JSON cannot serve as a native storage directory")
	}
	code += g.acmeChallenge(o, a)
	code += g.acmePropagation(o, a)
	kind := o.text("acme.keyType")
	if kind == "" {
		kind = "RSA4096"
	}
	switch kind {
	case "EC256", "EC384", "RSA2048", "RSA3072", "RSA4096", "RSA8192":
		a.KeyType(kind)
		code += fmt.Sprintf(".KeyType(%q)", kind)
		o.mark("acme.keyType", Translated, "explicit certificate key type")
	default:
		o.mark("acme.keyType", Unsupported, "unsupported certificate key type")
	}
	o.Generated = true
	return a, code
}

func (g *generator) acmeChallenge(o *Object, a *statute.AutoTLSConfig) string {
	if ep := o.text("acme.httpChallenge.entryPoint"); ep != "" {
		entry := g.entrypoints[ep]
		if entry == nil || g.entryTLS[ep] {
			o.note("$challenge", Manual, "HTTP-01 requires the named plain HTTP challenge listener")
		}
		a.HTTP01()
		o.mark("acme.httpChallenge.entryPoint", Translated, "native HTTP-01 challenge served by plain HTTP listeners before routing")
		return ".HTTP01()"
	}
	if o.text("acme.dnsChallenge.provider") == "cloudflare" {
		a.CloudflareDNS01("migration-binding")
		o.mark("acme.dnsChallenge.provider", Translated, "native Cloudflare DNS-01 with explicit CF_DNS_API_TOKEN binding")
		return ".CloudflareDNS01(" + g.envBinding("CF_DNS_API_TOKEN") + ")"
	}
	o.note("$challenge", Manual, "select HTTP-01 or Cloudflare DNS-01; native automatic challenge selection does not pin Traefik TLS-ALPN-01 or another DNS provider")
	return ""
}

func (g *generator) tlsPolicy(ep *Object, opts []statute.ListenerOption, codes []string) ([]statute.ListenerOption, []string) {
	defaults, defaultCode, ok := g.selectedTLSPolicy("default@file")
	if ok {
		opts = append(opts, defaults...)
		codes = append(codes, defaultCode...)
	}
	selected, owners, conflicts := g.collectTLSPolicyAssignments(ep)
	g.disableTLSConflicts(ep, owners, conflicts)
	grouped := map[string][]string{}
	for _, host := range keys(selected) {
		name := selected[host]
		if name != "default@file" {
			grouped[name] = append(grouped[name], host)
		}
	}
	for _, name := range keys(grouped) {
		values, code, ok := g.selectedTLSPolicy(name)
		if !ok {
			continue
		}
		hosts := grouped[name]
		hasAuth := false
		for i, value := range values {
			switch value := value.(type) {
			case statute.TLSPolicy:
				opts = append(opts, statute.TLSPolicyFor(hosts, value))
				codes = append(codes, "statute.TLSPolicyFor([]string{"+quoted(hosts)+"},"+code[i]+")")
			case statute.ClientAuth:
				hasAuth = true
				opts = append(opts, statute.ClientAuthFor(hosts, value))
				codes = append(codes, "statute.ClientAuthFor([]string{"+quoted(hosts)+"},"+code[i]+")")
			}
		}
		if !hasAuth {
			opts = append(opts, statute.ClientAuthFor(hosts, statute.ClientAuth{Mode: statute.NoClientCert}))
			codes = append(codes, "statute.ClientAuthFor([]string{"+quoted(hosts)+"},statute.ClientAuth{Mode:statute.NoClientCert})")
		}
	}
	return opts, codes
}

func routerTLSSource(router, entrypoint *Object) (*Object, string) {
	if routerHasTLS(router) {
		return router, kindTLS
	}
	return entrypoint, "http.tls"
}

func (g *generator) selectedTLSPolicy(name string) ([]statute.ListenerOption, []string, bool) {
	plain, provider, _ := strings.Cut(name, "@")
	if provider != providerFile {
		return nil, nil, false
	}
	object := g.objects["tls:options."+name]
	if object == nil || object.value("options."+plain) == nil {
		if plain != defaultName {
			g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: kindTLS, Code: "missing-named-tls-policy", Blocking: true})
		}
		return nil, nil, plain == defaultName
	}
	base := "options." + plain
	markEmptyTLSPolicy(object, base)
	p := statute.TLSPolicy{Identity: name}
	p.StrictSNI, _ = sourceBoolValue(object.value(base+".sniStrict"), false)
	for _, f := range []struct {
		key string
		dst *statute.TLSVersion
	}{{"minVersion", &p.MinVersion}, {"maxVersion", &p.MaxVersion}} {
		switch object.text(base + "." + f.key) {
		case "VersionTLS12":
			*f.dst = statute.TLS12
		case "VersionTLS13":
			*f.dst = statute.TLS13
		default:
			continue
		}
		object.mark(base+"."+f.key, Translated, "explicit listener TLS version bound")
	}
	suites, code := tlsSuites(object, base)
	p.CipherSuites = suites
	opts := []statute.ListenerOption{p}
	codes := []string{fmt.Sprintf("statute.TLSPolicy{MinVersion:statute.TLSVersion(%d),MaxVersion:statute.TLSVersion(%d),CipherSuites:[]statute.CipherSuite{%s},StrictSNI:%t,Identity:%q}", p.MinVersion, p.MaxVersion, code, p.StrictSNI, name)}
	opts, codes = clientAuthentication(object, base, opts, codes)
	if scalar(object.value(base+".sniStrict")) == falseValue {
		object.mark(base+".sniStrict", Exact, "unknown SNI may use the fallback certificate")
	}
	if scalar(object.value(base+".sniStrict")) == trueValue {
		object.mark(base+".sniStrict", Translated, "certificate must cover a nonempty SNI")
	}
	object.Generated = true
	return opts, codes, true
}

func tlsSuites(o *Object, base string) ([]statute.CipherSuite, string) {
	names, ok := stringList(o.value(base+".cipherSuites"), false)
	if !ok {
		return nil, ""
	}
	var out []statute.CipherSuite
	var code []string
	for _, name := range names {
		found := false
		for _, suite := range append(tls.CipherSuites(), tls.InsecureCipherSuites()...) {
			if suite.Name == name {
				out = append(out, statute.CipherSuite(suite.ID))
				code = append(code, fmt.Sprintf("statute.CipherSuite(%d)", suite.ID))
				found = true
				break
			}
		}
		if !found {
			return nil, ""
		}
	}
	o.mark(base+".cipherSuites", Translated, "explicit cipher IDs; canonical resolution rejects unsupported or unsafe suites")
	return out, strings.Join(code, ",")
}

func clientAuthentication(o *Object, base string, opts []statute.ListenerOption, codes []string) ([]statute.ListenerOption, []string) {
	path := base + ".clientAuth"
	modes := map[string]statute.ClientAuthMode{"RequestClientCert": statute.RequestClientCert, "RequireAnyClientCert": statute.RequireAnyClientCert, "VerifyClientCertIfGiven": statute.VerifyClientCertIfGiven, "RequireAndVerifyClientCert": statute.RequireAndVerifyClientCert}
	name := o.text(path + ".clientAuthType")
	mode, ok := modes[name]
	if !ok {
		if name == "NoClientCert" {
			o.mark(path+".clientAuthType", Exact, "client certificates are not requested")
		}
		return opts, codes
	}
	files, valid := stringList(o.value(path+".caFiles"), false)
	if !valid && o.value(path+".caFiles") != nil {
		return opts, codes
	}
	opts = append(opts, statute.ClientAuth{Mode: mode, CAFiles: files})
	codes = append(codes, fmt.Sprintf("statute.ClientAuth{Mode:statute.%s,CAFiles:[]string{%s}}", name, quoted(files)))
	o.mark(path+".clientAuthType", Translated, "listener-owned client certificate authentication")
	o.mark(path+".caFiles", Translated, "CA file references retained without reading material")
	return opts, codes
}

func (g *generator) certificates(o *Object) {
	prefix := "stores.default.defaultCertificate"
	cert, key := o.text(prefix+".certFile"), o.text(prefix+".keyFile")
	if cert == "" || key == "" {
		items, ok := o.value("certificates").([]any)
		if !ok || len(items) != 1 {
			return
		}
		m := asMap(items[0])
		cert, key = stringValue(m["certFile"]), stringValue(m["keyFile"])
		prefix = "certificates[0]"
		o.note("$certificate-selection", Manual, "source certificate SAN selection is unknown without reading material; hostless candidate changes fallback certificate selection; supply explicit native StaticTLSFor host mappings")
	}
	if cert == "" || key == "" || !literal(cert) || !literal(key) {
		return
	}
	if g.certFile != "" {
		o.note("$certificates", Manual, "multiple certificate inventories require explicit SNI source selection")
		return
	}
	g.certFile, g.keyFile = cert, key
	o.mark(prefix+".certFile", Translated, "certificate file reference retained without reading material")
	o.mark(prefix+".keyFile", Translated, "private key file reference retained without reading material")
	g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: toolBindings, Code: "certificate-material-and-host-coverage-required", Deferred: true})
	o.Generated = true
}

func markEmptyTLSPolicy(o *Object, path string) {
	if m, ok := o.value(path).(map[string]any); ok && len(m) == 0 {
		o.mark(path, Translated, "native default TLS policy selected for all listener routes")
	}
}

func acmeAccountValue(key, value string) bool {
	if value == "" || !literal(value) {
		return false
	}
	if key != "caServer" {
		return true
	}
	u, err := url.Parse(value)
	return err == nil && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Host != "" && (u.Scheme == protocolHTTP || u.Scheme == protocolHTTPS)
}

func (g *generator) unusedTLSPolicies() {
	g.each(kindTLS, func(o *Object) {
		if !o.Generated && strings.HasPrefix(o.Name, "options.") {
			o.mark("", OutOfScope, "TLS profile is not selected by any supplied HTTP router or listener")
		}
	})
}

func (g *generator) collectTLSPolicyAssignments(ep *Object) (map[string]string, map[string][]*Object, map[string]bool) {
	selected := map[string]string{}
	owners := map[string][]*Object{}
	conflicts := map[string]bool{}
	g.each("routers", func(o *Object) {
		if !slices.Contains(g.routerEntries[o], ep.Name) {
			return
		}
		owner, base := routerTLSSource(o, ep)
		name := owner.text(base + ".options")
		if name == "" {
			name = defaultName
		}
		name = qualified(name, providerFile)
		hosts, err := parseRuleHosts(o)
		if err != nil {
			return
		}
		for _, host := range hosts {
			owners[host] = append(owners[host], o)
			if old, exists := selected[host]; exists && old != name {
				selected[host] = "default@file"
				conflicts[host] = true
			} else {
				selected[host] = name
			}
		}
		owner.mark(base+".options", Translated, "TLS policy selected by SNI; conflicting hostname assignments use the default")
	})
	return selected, owners, conflicts
}

func (g *generator) disableTLSConflicts(ep *Object, owners map[string][]*Object, conflicts map[string]bool) {
	strict := false
	if core := g.objects["setting:core@static"]; core != nil {
		strict, _ = sourceBoolValue(core.value("strictTLSOptions"), false)
	}
	if strict {
		for _, host := range keys(conflicts) {
			for _, router := range owners[host] {
				entries := slices.Clone(g.routerEntries[router])
				entries = slices.DeleteFunc(entries, func(name string) bool { return name == ep.Name })
				g.routerEntries[router] = entries
				if len(entries) == 0 {
					g.disabledRouters[router] = true
				}
				router.note("$tls-conflict", Translated, "source disables this router on an entrypoint with conflicting TLS policies")
				g.guardDockerTLSConflict(router)
			}
		}
	}
}

func (g *generator) guardDockerTLSConflict(router *Object) {
	if router.Provider != providerDocker {
		return
	}
	g.r.Report.Checks = append(g.r.Report.Checks, Check{
		Tool: kindTLS, Code: "docker-strict-tls-conflict", Blocking: true,
		Reason:     "Docker discovery cannot enforce source router exclusions for conflicting TLS policies",
		References: []string{router.identity()},
	})
}
