package statute

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestCloudflareExportOmitsToken(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"export-secret-sentinel", `secret"<&>\\value`} {
		t.Run(token, func(t *testing.T) {
			cfg := Config{Listeners: Listeners{HTTPS(":443", AutoTLS("example.com").
				Email("ops@example.com").Storage(t.TempDir()).CloudflareDNS01(token).Zone("zone-1").
				Propagation(DNSPropagation{Delay: "1s"}))}}
			const secondToken = "second-source-token"
			cfg.Listeners = append(cfg.Listeners, HTTPS(":4443", AutoTLS("other.example.com").
				Email("ops@example.com").Storage(t.TempDir()).CloudflareDNS01(secondToken)))
			rc, err := Resolve(cfg)
			if err != nil {
				t.Fatal(err)
			}
			dns := rc.Listeners[0].AutoTLS.DNS01
			type copiedDNS resolved.CloudflareDNS01
			values := []any{rc, *rc, dns, *dns, copiedDNS(*dns), []resolved.CloudflareDNS01{*dns}, map[string]any{"dns": dns}}
			assertExportVariants(t, values, token)
			var output bytes.Buffer
			if err := Export(cfg, &output); err != nil {
				t.Fatal(err)
			}
			assertNoExportToken(t, output.Bytes(), token)
			assertNoExportToken(t, output.Bytes(), secondToken)
			var decoded resolved.Config
			if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			got := decoded.Listeners[0].AutoTLS
			if got.DNS01.ZoneID != "zone-1" || got.DNS01.Propagation.Delay != dns.Propagation.Delay || got.Directory != rc.Listeners[0].AutoTLS.Directory {
				t.Fatal("export lost non-secret DNS-01 metadata")
			}
			if dns.APIToken != token {
				t.Fatal("encoding changed the runtime credential")
			}
		})
	}
}

func assertExportVariants(t *testing.T, values []any, token string) {
	t.Helper()
	for _, value := range values {
		for range 2 {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			assertNoExportToken(t, data, token)
		}
	}
}

func assertNoExportToken(t *testing.T, data []byte, token string) {
	t.Helper()
	encoded, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"APIToken"`)) || bytes.Contains(data, encoded) {
		t.Fatal("JSON contains the DNS-01 credential")
	}
}

func TestCloudflareExportPreservesRuntimeAuthentication(t *testing.T) {
	const token = "runtime-token-sentinel"
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	called := false
	http.DefaultTransport = exportTokenTransport(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("DNS client lost its runtime credential")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"success":true,"result":[{"id":"zone-1","name":"example.com"}]}`))}, nil
	})
	cfg := &resolved.AutoTLS{Storage: t.TempDir(), DNS01: &resolved.CloudflareDNS01{APIToken: token}}
	if _, err := json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	m, err := newDNS01Manager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.solver.(*dns01Solver).cf.FindZoneID(t.Context(), "example.com")
	if err != nil || id != "zone-1" || !called {
		t.Fatalf("runtime DNS authentication: zone=%q called=%t error=%v", id, called, err)
	}
}

type exportTokenTransport func(*http.Request) (*http.Response, error)

func (f exportTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
