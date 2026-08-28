package statute

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExport_ProducesValidJSON(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":8080")},
		Upstreams: Upstreams{
			"api": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}}},
		},
		Routes: Routes{Match("/*").ProxyTo("api")},
	}
	var buf bytes.Buffer
	if err := Export(cfg, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if _, ok := out["Listeners"]; !ok {
		t.Errorf("missing Listeners in export")
	}
	if _, ok := out["Routes"]; !ok {
		t.Errorf("missing Routes in export")
	}
}

func TestExport_BadConfigReturnsError(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":8080")},
		Routes: Routes{
			Match("/*").ProxyTo("undefined"), // unknown upstream
		},
	}
	var buf bytes.Buffer
	err := Export(cfg, &buf)
	if err == nil {
		t.Fatal("want error for unknown upstream")
	}
	if !strings.Contains(err.Error(), "unknown upstream") {
		t.Errorf("error: %v", err)
	}
}

// TestExport_CarriesHealthPolicy — the probe Host override, accepted probe
// statuses, and passive policy are part of the exported schema.
func TestExport_CarriesHealthPolicy(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":8080")},
		Upstreams: Upstreams{
			"api": Pool{
				Backends: []Backend{{Address: "127.0.0.1:1"}},
				HealthCheck: HealthCheck{
					Path: "/healthz", Host: "probe.internal", Statuses: []int{200, 204},
				},
				PassiveHealthCheck: PassiveHealthCheck{FailureWindow: "30s", MaxFailures: 3},
			},
		},
		Routes: Routes{Match("/*").ProxyTo("api")},
	}
	var buf bytes.Buffer
	if err := Export(cfg, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out struct {
		Upstreams map[string]struct {
			HealthCheck struct {
				Host     string
				Statuses []int
			}
			PassiveHealthCheck struct {
				Enabled       bool
				FailureWindow int64
				MaxFailures   int
			}
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	p := out.Upstreams["api"]
	if p.HealthCheck.Host != "probe.internal" {
		t.Errorf("exported probe Host: %q", p.HealthCheck.Host)
	}
	if !slices.Equal(p.HealthCheck.Statuses, []int{200, 204}) {
		t.Errorf("exported Statuses: %v", p.HealthCheck.Statuses)
	}
	if !p.PassiveHealthCheck.Enabled || p.PassiveHealthCheck.FailureWindow != int64(30*time.Second) || p.PassiveHealthCheck.MaxFailures != 3 {
		t.Errorf("exported PassiveHealthCheck: %+v", p.PassiveHealthCheck)
	}
}

// TestExport_CarriesTransportResponsePolicy covers normalized response timing.
func TestExport_CarriesTransportResponsePolicy(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":8080")},
		Upstreams: Upstreams{
			"api": Pool{
				Backends:  []Backend{{Address: "127.0.0.1:1"}},
				Transport: Transport{ResponseHeaderTimeout: "5s", FlushInterval: "100ms"},
			},
		},
		Routes: Routes{Match("/*").ProxyTo("api")},
	}
	var buf bytes.Buffer
	if err := Export(cfg, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out struct {
		Upstreams map[string]struct {
			Transport struct {
				ResponseHeaderTimeout int64
				FlushInterval         int64
			}
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	transport := out.Upstreams["api"].Transport
	if got := transport.ResponseHeaderTimeout; got != int64(5*time.Second) {
		t.Errorf("exported ResponseHeaderTimeout: got %d, want %d", got, int64(5*time.Second))
	}
	if got := transport.FlushInterval; got != int64(100*time.Millisecond) {
		t.Errorf("exported FlushInterval: got %d, want %d", got, int64(100*time.Millisecond))
	}
}

func TestExport_CarriesDockerPoolPolicy(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":8080")},
		Docker: Docker().PoolPolicy("app@traefik", PoolPolicy{
			HealthCheck:        HealthCheck{Path: "/ready", Host: "probe.internal", Statuses: []int{200, 204}},
			PassiveHealthCheck: PassiveHealthCheck{FailureWindow: "30s", MaxFailures: 3},
			Transport:          Transport{ServerName: "app.internal", ResponseHeaderTimeout: "5s"},
			UpstreamHost:       HostValue("app.internal"),
		}),
	}
	var buf bytes.Buffer
	if err := Export(cfg, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out struct {
		Docker struct {
			PoolPolicy map[string]struct {
				HealthCheck        struct{ Enabled bool }
				PassiveHealthCheck struct{ Enabled bool }
				Transport          struct {
					ServerName            string
					ResponseHeaderTimeout int64
				}
				HostValue string
			}
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	policy := out.Docker.PoolPolicy["app@traefik"]
	if !policy.HealthCheck.Enabled || !policy.PassiveHealthCheck.Enabled || policy.Transport.ServerName != "app.internal" || policy.Transport.ResponseHeaderTimeout != int64(5*time.Second) || policy.HostValue != "app.internal" {
		t.Errorf("exported PoolPolicy = %+v", policy)
	}
}

// TestExport_CarriesTLSPolicy — the resolved downstream TLS policy is part
// of the exported schema, in its normalised form.
func TestExport_CarriesTLSPolicy(t *testing.T) {
	t.Parallel()
	cfg := tlsRouterConfig(
		StaticTLS("cert.pem", "key.pem"),
		TLSPolicy{
			MinVersion:   TLS12,
			MaxVersion:   TLS13,
			CipherSuites: []CipherSuite{TLSECDHEECDSAWithAES128GCM},
		},
	)
	var buf bytes.Buffer
	if err := Export(cfg, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out struct {
		Listeners []struct {
			TLSPolicy *struct {
				MinVersion   string
				MaxVersion   string
				CipherSuites []string
			}
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	p := out.Listeners[0].TLSPolicy
	if p == nil {
		t.Fatalf("export carries no TLSPolicy:\n%s", buf.String())
	}
	if p.MinVersion != "1.2" || p.MaxVersion != "1.3" {
		t.Errorf("versions: got %q/%q", p.MinVersion, p.MaxVersion)
	}
	want := []string{"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256"}
	if !slices.Equal(p.CipherSuites, want) {
		t.Errorf("cipher suites: got %v, want %v", p.CipherSuites, want)
	}
}

func TestExportCarriesClientAuth(t *testing.T) {
	t.Parallel()
	cfg := tlsRouterConfig(
		StaticTLS("cert.pem", "key.pem"),
		ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: []string{"/run/certs/client-ca.pem"}},
	)
	var buf bytes.Buffer
	if err := Export(cfg, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out struct {
		Listeners []struct {
			ClientAuth *struct {
				Mode    string
				CAFiles []string
			}
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	p := out.Listeners[0].ClientAuth
	if p == nil || p.Mode != "require-and-verify" || !slices.Equal(p.CAFiles, []string{"/run/certs/client-ca.pem"}) {
		t.Errorf("exported ClientAuth = %+v", p)
	}
}
