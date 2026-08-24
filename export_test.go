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
