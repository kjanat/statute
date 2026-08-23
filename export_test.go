package statute

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
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
