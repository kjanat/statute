package statute

import (
	"bytes"
	"encoding/json"
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
