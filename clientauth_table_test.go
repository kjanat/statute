package statute

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestClientAuthTableDEREquivalence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Equal subjects deliberately do not imply equal certificate identities.
	caA, _, a := makeTestCA(t, "same-subject", 1)
	caB, _, b := makeTestCA(t, "same-subject", 2)
	wantPool := x509.NewCertPool()
	wantPool.AddCert(caA)
	wantPool.AddCert(caB)
	writeFile(t, dir, "a.pem", pemCert(a))
	writeFile(t, dir, "b.pem", pemCert(b))
	writeFile(t, dir, "ab.pem", pemCert(a)+pemCert(b))
	writeFile(t, dir, "ba-duplicates.pem", "CA bundle\n"+strings.ReplaceAll(pemCert(b)+pemCert(a)+pemCert(b), "\n", "\r\n"))
	for _, mode := range []resolved.ClientAuthMode{resolved.ClientAuthRequest, resolved.ClientAuthRequireAny, resolved.ClientAuthVerifyIfGiven, resolved.ClientAuthRequireAndVerify} {
		t.Run(string(mode), func(t *testing.T) {
			policy := func(files ...string) resolved.ClientAuth { return resolved.ClientAuth{Mode: mode, CAFiles: files} }
			fallback := policy(dir + "/ab.pem")
			table, err := buildClientAuthTable(&resolved.Listener{
				ClientAuth: &fallback,
				ClientAuthPolicies: []*resolved.ClientAuthPolicy{
					{Hosts: []string{"one.example", "alias.example"}, Policy: policy(dir + "/ab.pem")},
					{Hosts: []string{"two.example"}, Policy: policy(dir+"/b.pem", dir+"/a.pem", dir+"/a.pem")},
					{Hosts: []string{"*.equivalent.example"}, Policy: policy(dir + "/ba-duplicates.pem")},
					{Hosts: []string{"a.example"}, Policy: policy(dir + "/a.pem")},
					{Hosts: []string{"b.example"}, Policy: policy(dir + "/b.pem")},
					{Hosts: []string{"different-mode.example"}, Policy: resolved.ClientAuth{Mode: resolved.ClientAuthRequest, CAFiles: []string{dir + "/ab.pem"}}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			assertClientAuthTableEquivalence(t, table, mode, wantPool)
		})
	}
}

func assertClientAuthTableEquivalence(t *testing.T, table *clientAuthTable, mode resolved.ClientAuthMode, wantPool *x509.CertPool) {
	t.Helper()
	for _, host := range []string{"one.example", "alias.example", "two.example", "x.equivalent.example", "unknown.example", ""} {
		selected := table.selectPolicy(host)
		if selected.id != table.defaultAuth.id || len(selected.der) != 2 || !selected.clientCAs.Equal(wantPool) {
			t.Errorf("%q: did not intern equivalent DER set with the default", host)
		}
		assertClientAuthAdmission(t, table, "one.example", host, 204)
	}
	for _, host := range []string{"a.example", "b.example"} {
		if table.selectPolicy(host).id == table.defaultAuth.id {
			t.Errorf("%q: overlapping CA sets were equated", host)
		}
		assertClientAuthAdmission(t, table, "one.example", host, 421)
	}
	if table.selectPolicy("a.example").id == table.selectPolicy("b.example").id {
		t.Error("same-subject certificates were equated")
	}
	want := 421
	if mode == resolved.ClientAuthRequest {
		want = 204
	}
	assertClientAuthAdmission(t, table, "one.example", "different-mode.example", want)
}

func assertClientAuthAdmission(t *testing.T, table *clientAuthTable, sni, host string, want int) {
	t.Helper()
	if host == "" {
		// Absent SNI selects the default; an empty HTTP authority is invalid.
		sni, host = "", "one.example"
	}
	req := httptest.NewRequest(http.MethodGet, "https://one.example/", nil)
	req.Host, req.TLS = host, &tls.ConnectionState{ServerName: sni}
	w := httptest.NewRecorder()
	clientAuthAdmission(table, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, req)
	if w.Code != want || (want == 421 && w.Header().Get("Cache-Control") != "no-store") {
		t.Errorf("SNI=%q authority=%q: status=%d Cache-Control=%q, want %d", sni, host, w.Code, w.Header().Get("Cache-Control"), want)
	}
}

func TestClientAuthTableDeclarationGrouping(t *testing.T) {
	t.Parallel()
	for _, grouped := range []bool{false, true} {
		assignments := []*resolved.ClientAuthPolicy{{Hosts: []string{"a.example"}, Policy: resolved.ClientAuth{Mode: resolved.ClientAuthRequest}}}
		if grouped {
			assignments[0].Hosts = append(assignments[0].Hosts, "b.example")
		} else {
			assignments = append(assignments, &resolved.ClientAuthPolicy{Hosts: []string{"b.example"}, Policy: resolved.ClientAuth{Mode: resolved.ClientAuthRequest}})
		}
		table, err := buildClientAuthTable(&resolved.Listener{ClientAuthPolicies: assignments})
		if err != nil {
			t.Fatal(err)
		}
		assertClientAuthAdmission(t, table, "a.example", "b.example", 204)
		assertClientAuthAdmission(t, table, "a.example", "public.example", 421)
		if table.defaultAuth.id == table.selectPolicy("a.example").id {
			t.Fatal("nil default equated with optional certificate request")
		}
	}
}

func TestClientAuthTableValidatesEveryFileBeforeInterning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, _, der := makeTestCA(t, "ca", 1)
	writeFile(t, dir, "ca.pem", pemCert(der))
	writeFile(t, dir, "bad.pem", "not PEM")
	for _, invalid := range []string{dir + "/missing.pem", dir + "/bad.pem"} {
		policy := resolved.ClientAuth{Mode: resolved.ClientAuthRequireAndVerify, CAFiles: []string{dir + "/ca.pem"}}
		_, err := buildClientAuthTable(&resolved.Listener{ClientAuth: &policy, ClientAuthPolicies: []*resolved.ClientAuthPolicy{
			{Hosts: []string{"a.example"}, Policy: policy},
			{Hosts: []string{"b.example"}, Policy: resolved.ClientAuth{Mode: policy.Mode, CAFiles: []string{dir + "/ca.pem", invalid}}},
		}})
		if err == nil || !strings.Contains(err.Error(), "client_auth_policies[1]") {
			t.Errorf("bad equivalent material %q: %v", invalid, err)
		}
	}
}

func TestClientAuthTableSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, _, der := makeTestCA(t, "ca", 1)
	writeFile(t, dir, "ca.pem", pemCert(der))
	l := &resolved.Listener{ClientAuthPolicies: []*resolved.ClientAuthPolicy{{Hosts: []string{"a.example"}, Policy: resolved.ClientAuth{Mode: resolved.ClientAuthRequireAndVerify, CAFiles: []string{dir + "/ca.pem"}}}}}
	table, err := buildClientAuthTable(l)
	if err != nil {
		t.Fatal(err)
	}
	before := table.selectPolicy("a.example")
	writeFile(t, dir, "ca.pem", "invalid after construction")
	l.ClientAuthPolicies[0].Hosts[0] = "changed.example"
	l.ClientAuthPolicies[0].Policy.Mode = resolved.ClientAuthRequest
	if selected := table.selectPolicy("a.example"); selected.id != before.id || selected.mode != tls.RequireAndVerifyClientCert || !slices.Equal(selected.der, []string{string(der)}) {
		t.Fatal("compiled table retained live configuration or material dependencies")
	}
	assertClientAuthAdmission(t, table, "a.example", "changed.example", 421)
}
