package statute

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestResolveScopedClientAuth(t *testing.T) {
	t.Parallel()
	hosts := []string{" PROTECTED.Example.. ", "*.BÜCHER.example."}
	files := []string{" ca.pem "}
	opt := ClientAuthFor(hosts, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: files})
	hosts[0], files[0] = "changed.example", "changed.pem"
	cfg := tlsRouterConfig(StaticTLS("cert.pem", "key.pem"), opt)
	r := mustResolve(t, cfg)
	binding := r.Listeners[0].ClientAuthPolicies[0]
	if !slices.Equal(binding.Hosts, []string{"protected.example", "*.xn--bcher-kva.example"}) ||
		binding.Policy.Mode != resolved.ClientAuthRequireAndVerify || !slices.Equal(binding.Policy.CAFiles, []string{"ca.pem"}) {
		t.Fatalf("binding = %+v", binding)
	}
	cfg.Listeners[0].clientAuthPolicies[0].hosts[0] = "another.example"
	cfg.Listeners[0].clientAuthPolicies[0].policy.CAFiles[0] = "another.pem"
	if binding.Hosts[0] != "protected.example" || binding.Policy.CAFiles[0] != "ca.pem" {
		t.Fatal("resolved binding aliases surface slices")
	}
	second := mustResolve(t, tlsRouterConfig(StaticTLS("cert.pem", "key.pem"), opt))
	if second.Listeners[0].ClientAuthPolicies[0].Hosts[0] != "protected.example" {
		t.Fatal("applying an option aliases another listener")
	}
}

func TestResolveScopedClientAuthErrors(t *testing.T) {
	t.Parallel()
	policy := ClientAuth{Mode: RequestClientCert}
	for _, hosts := range [][]string{nil, {}, {" "}, {"."}, {"*"}, {"*."}, {"*.*.example"}, {"foo.*.example"}, {"a.example:443"}, {"a/b.example"}, {"a.example", " A.EXAMPLE.. "}, {"bücher.example", "xn--bcher-kva.example"}} {
		if _, err := Resolve(tlsRouterConfig(StaticTLS("c", "k"), ClientAuthFor(hosts, policy))); err == nil {
			t.Errorf("accepted invalid hosts %q", hosts)
		}
	}
	for _, policy := range []ClientAuth{{}, {Mode: ClientAuthMode(99)}, {Mode: VerifyClientCertIfGiven}, {Mode: RequireAndVerifyClientCert}, {Mode: RequestClientCert, CAFiles: []string{" "}}} {
		if _, err := Resolve(tlsRouterConfig(StaticTLS("c", "k"), ClientAuthFor([]string{"a.example"}, policy))); err == nil {
			t.Errorf("accepted invalid policy %+v", policy)
		}
	}
	if _, err := Resolve(tlsRouterConfig(StaticTLS("c", "k"), ClientAuthFor([]string{"a.example"}, policy), ClientAuthFor([]string{"A.example."}, policy))); err == nil {
		t.Fatal("accepted duplicate across bindings")
	}
	for _, listener := range []*Listener{HTTP(":80"), HTTPS(":443", StaticTLS("c", "k")).RedirectTo("http")} {
		ClientAuthFor([]string{"a.example"}, policy).applyListener(listener)
		cfg := tlsRouterConfig(StaticTLS("c", "k"))
		cfg.Listeners = Listeners{listener}
		if _, err := Resolve(cfg); err == nil {
			t.Error("accepted scoped auth on nonterminating listener")
		}
	}
}

func TestScopedClientAuthUnicodeDuplicates(t *testing.T) {
	t.Parallel()
	for _, suffix := range []string{"\u3002", "\uff0e", "\uff61", "\u3002\uff0e.\uff61.."} {
		if _, err := Resolve(tlsRouterConfig(StaticTLS("c", "k"), ClientAuthFor([]string{"one.example" + suffix, "one.example"}, ClientAuth{Mode: RequestClientCert}))); err == nil {
			t.Errorf("accepted duplicate Unicode separator alias %q", suffix)
		}
		if _, err := Resolve(tlsRouterConfig(StaticTLSFor("one.example"+suffix, "c", "k"), StaticTLSFor("one.example", "c", "k"))); err == nil {
			t.Errorf("accepted duplicate TLS source Unicode alias %q", suffix)
		}
	}
}

func TestScopedClientAuthSelection(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, tlsRouterConfig(StaticTLS("c", "k"),
		ClientAuth{Mode: RequestClientCert},
		ClientAuthFor([]string{"*.example"}, ClientAuth{Mode: RequireAnyClientCert}),
		ClientAuthFor([]string{"one.example", "bücher.example"}, ClientAuth{Mode: VerifyClientCertIfGiven, CAFiles: []string{"ca.pem"}}),
	))
	l := r.Listeners[0]
	for _, tc := range []struct {
		host string
		want *resolved.ClientAuth
	}{
		{"ONE.EXAMPLE..", &l.ClientAuthPolicies[1].Policy},
		{"bücher.example", &l.ClientAuthPolicies[1].Policy},
		{"xn--bcher-kva.example", &l.ClientAuthPolicies[1].Policy},
		{"two.example", &l.ClientAuthPolicies[0].Policy},
		{"nested.two.example", l.ClientAuth},
		{"example", l.ClientAuth},
		{"", l.ClientAuth},
		{"elsewhere.test", l.ClientAuth},
	} {
		if got := selectClientAuth(l, tc.host); got != tc.want {
			t.Errorf("%q: got %p, want %p", tc.host, got, tc.want)
		}
	}
}

func TestScopedClientAuthACMEFallback(t *testing.T) {
	t.Parallel()
	required := ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{"ca.pem"}}
	optional := ClientAuth{Mode: RequestClientCert}
	for _, tc := range []struct {
		name    string
		opts    []ListenerOption
		wantErr bool
	}{
		{"exact protected", []ListenerOption{ClientAuthFor([]string{"one.example"}, required)}, true},
		{"wildcard protected", []ListenerOption{ClientAuthFor([]string{"*.example"}, required)}, true},
		{"unrelated protected", []ListenerOption{ClientAuthFor([]string{"other.example"}, required)}, false},
		{"default overridden", []ListenerOption{required, ClientAuthFor([]string{"one.example", "two.example"}, optional)}, false},
		{"wildcard overridden", []ListenerOption{ClientAuthFor([]string{"*.example"}, required), ClientAuthFor([]string{"one.example", "two.example"}, optional)}, false},
		{"second domain protected", []ListenerOption{ClientAuthFor([]string{"two.example"}, required)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]ListenerOption{AutoTLS("one.example", "two.example").Email("ops@example.test").Storage(t.TempDir())}, tc.opts...)
			cfg := tlsRouterConfig(opts...)
			_, err := Resolve(cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Resolve = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !strings.Contains(err.Error(), `statute.HTTP(":80").RedirectTo("https")`) {
					t.Errorf("guidance = %v", err)
				}
				cfg.Listeners = append(cfg.Listeners, HTTP(":80").RedirectTo("https"))
				if _, err := Resolve(cfg); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestScopedClientAuthAuthorityAdmission(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, tlsRouterConfig(StaticTLS("c", "k"),
		ClientAuthFor([]string{"one.example", "alias.example", "*.protected.example"}, ClientAuth{Mode: RequestClientCert}),
		ClientAuthFor([]string{"two.example"}, ClientAuth{Mode: RequestClientCert}),
	))
	for _, tc := range []struct {
		sni, host string
		tls       bool
		status    int
	}{
		{"direct.example", "one.example", true, 421},
		{"", "one.example", true, 421},
		{"one.example", "two.example", true, 421},
		{"one.example", "direct.example", true, 421},
		{"one.example", "ALIAS.EXAMPLE..:443", true, 204},
		{"x.protected.example", "y.protected.example", true, 204},
		{"one.example", "deep.x.protected.example", true, 421},
		{"direct.example", "other.example", true, 204},
		{"", "[::1]:443", true, 204},
		{"", "[::1]", true, 204},
		{"one.example", "one.example", false, 421},
		{"", "", true, 421},
		{"", ".", true, 421},
		{"", "bad:port", true, 421},
		{"", "a.example:65536", true, 421},
		{"", "a.example:", true, 421},
		{"", "a/b.example", true, 421},
		{"", "*.example", true, 421},
	} {
		req := httptest.NewRequest(http.MethodGet, "https://direct.example/", nil)
		req.Host = tc.host
		req.TLS = nil
		if tc.tls {
			req.TLS = &tls.ConnectionState{ServerName: tc.sni}
		}
		rec := httptest.NewRecorder()
		called := false
		clientAuthAdmission(r.Listeners[0], http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(204) })).ServeHTTP(rec, req)
		if rec.Code != tc.status || called != (tc.status == 204) {
			t.Errorf("SNI=%q Host=%q tls=%v: status %d called=%v", tc.sni, tc.host, tc.tls, rec.Code, called)
		}
	}
}

func TestScopedClientAuthTooling(t *testing.T) {
	t.Parallel()
	cfg := tlsRouterConfig(StaticTLS("c", "k"),
		ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{"default.pem"}},
		ClientAuthFor([]string{"ONE.example", "*.private.example"}, ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{"private.pem"}}),
		ClientAuthFor([]string{"weak.example"}, ClientAuth{Mode: RequestClientCert}),
	)
	assertScopedClientAuthExport(t, cfg)
	assertScopedClientAuthGraph(t, cfg)
	assertScopedClientAuthLint(t, cfg)
}

func assertScopedClientAuthExport(t *testing.T, cfg Config) {
	t.Helper()
	var buf bytes.Buffer
	if err := Export(cfg, &buf); err != nil {
		t.Fatal(err)
	}
	var out resolved.Config
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Listeners[0].ClientAuthPolicies) != 2 || out.Listeners[0].ClientAuthPolicies[0].Hosts[0] != "one.example" || out.Listeners[0].ClientAuthPolicies[0].Policy.CAFiles[0] != "private.pem" {
		t.Fatalf("export = %s", buf.String())
	}
	buf.Reset()
	if err := Export(tlsRouterConfig(StaticTLS("c", "k"), ClientAuth{Mode: RequestClientCert}), &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "ClientAuthPolicies") {
		t.Fatal("legacy export acquired a scoped policy field")
	}
}

func assertScopedClientAuthGraph(t *testing.T, cfg Config) {
	t.Helper()
	var buf bytes.Buffer
	if err := GraphDOT(cfg, &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"client-auth-for=[one.example *.private.example]: require-and-verify", "client-ca=[private.pem]", "client-auth-for=[weak.example]: request"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("graph missing %q", want)
		}
	}
}

func assertScopedClientAuthLint(t *testing.T, cfg Config) {
	t.Helper()
	findings, err := Lint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, finding := range findings {
		if finding.Code == "TLS007" {
			paths = append(paths, finding.Path)
		}
	}
	if !slices.Equal(paths, []string{"listeners[0].client_auth_policies[1].policy.mode"}) {
		t.Errorf("TLS007 paths = %v", paths)
	}
}
