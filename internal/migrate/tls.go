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
		g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: "tls", Code: "missing-certificate-resolver", Blocking: true})
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
	if o.text("acme.keyType") == "EC256" {
		o.mark("acme.keyType", Translated, "pinned native ACME sources issue ECDSA P-256 keys")
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
	names := map[string]bool{}
	g.each("routers", func(o *Object) {
		if !slices.Contains(g.routerEntries[o], ep.Name) {
			return
		}
		name := o.text("tls.options")
		if name == "" {
			name = ep.text("http.tls.options")
		}
		if name == "" {
			name = defaultName
		}
		names[qualified(name, providerFile)] = true
	})
	if len(names) > 1 {
		ep.note("$tls-options", Manual, "routers select different SNI TLS policies; native protocol/cipher policy is listener-wide")
		return opts, codes
	}
	for name := range names {
		native, code, ok := g.selectedTLSPolicy(name)
		if ok {
			opts = append(opts, native...)
			codes = append(codes, code...)
			ep.mark("http.tls.options", Translated, "selected policy applied at listener scope")
			g.each("routers", func(o *Object) {
				if slices.Contains(g.routerEntries[o], ep.Name) {
					o.mark("tls.options", Translated, "selected policy matches every router on the listener")
				}
			})
		}
	}
	return opts, codes
}

func (g *generator) selectedTLSPolicy(name string) ([]statute.ListenerOption, []string, bool) {
	plain, provider, _ := strings.Cut(name, "@")
	if provider != providerFile {
		return nil, nil, false
	}
	object := g.objects["tls:options."+name]
	if object == nil || object.value("options."+plain) == nil {
		if plain != defaultName {
			g.r.Report.Checks = append(g.r.Report.Checks, Check{Tool: "tls", Code: "missing-named-tls-policy", Blocking: true})
		}
		return nil, nil, plain == defaultName
	}
	base := "options." + plain
	markEmptyTLSPolicy(object, base)
	p := statute.TLSPolicy{}
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
	codes := []string{fmt.Sprintf("statute.TLSPolicy{MinVersion:statute.TLSVersion(%d),MaxVersion:statute.TLSVersion(%d),CipherSuites:[]statute.CipherSuite{%s}}", p.MinVersion, p.MaxVersion, code)}
	opts, codes = clientAuthentication(object, base, opts, codes)
	if scalar(object.value(base+".sniStrict")) == falseValue {
		object.mark(base+".sniStrict", Exact, "unknown SNI may use the fallback certificate")
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
