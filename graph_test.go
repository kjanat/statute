package statute

import (
	"bytes"
	"strings"
	"testing"
)

func TestGraphDOT_ContainsExpectedNodes(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{
			HTTP(":80").RedirectTo("https"),
			HTTPS(":443",
				StaticTLS("/etc/cert.pem", "/etc/key.pem"),
				HTTP2(),
			),
		},
		Upstreams: Upstreams{
			"api": Pool{
				Backends: []Backend{
					{Address: "10.0.0.1:8080"},
					{Address: "10.0.0.2:8080", Backup: true},
				},
				Strategy: LeastConnections,
			},
		},
		Routes: Routes{
			Match("/api/*").Host("api.example.com").ProxyTo("api"),
		},
	}
	var buf bytes.Buffer
	if err := GraphDOT(cfg, &buf); err != nil {
		t.Fatalf("GraphDOT: %v", err)
	}
	out := buf.String()
	wants := []string{
		"digraph statute {",
		"rankdir=LR",
		":80",
		":443",
		"least-conn",
		"10.0.0.1:8080",
		"10.0.0.2:8080",
		"api.example.com",
		"redirect 301",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("graph missing %q in output:\n%s", w, out)
		}
	}
}

func TestGraphDOT_BadConfigReturnsError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := GraphDOT(Config{
		Listeners: Listeners{HTTP(":80")},
		Routes:    Routes{Match("/*").ProxyTo("missing")},
	}, &buf)
	if err == nil {
		t.Fatal("want error for bad config")
	}
}
