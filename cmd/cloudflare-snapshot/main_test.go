package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/internal/cloudflare"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureClient(t *testing.T, fail bool) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Error("fetch lacks total five-second deadline")
		}
		body := "192.0.2.0/24\n"
		switch r.URL.String() {
		case strings.TrimSuffix(cloudflare.IPv4URL, "#"):
		case strings.TrimSuffix(cloudflare.IPv6URL, "#"):
			if fail {
				return nil, errors.New("IPv6 unavailable")
			}
			body = "2001:db8::/32\n"
		default:
			t.Fatalf("unexpected request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
}

func TestGenerate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cloudflare.json")
	var out bytes.Buffer
	if err := run(t.Context(), fixtureClient(t, false), []string{"-out", path}, &out); err != nil {
		t.Fatal(err)
	}
	original := readFile(t, path)
	snapshot, err := cloudflare.DecodeSnapshot(original)
	if err != nil || len(snapshot.CIDRs()) != 2 || !strings.Contains(out.String(), "Rebuild to embed") {
		t.Fatalf("generated snapshot: %+v; output %q; error %v", snapshot, out.String(), err)
	}
	out.Reset()
	if err := run(t.Context(), fixtureClient(t, true), []string{"-out", path}, &out); err == nil {
		t.Fatal("accepted partial source failure")
	}
	if !bytes.Equal(original, readFile(t, path)) || out.Len() != 0 {
		t.Fatal("failed generation changed artifact or reported success")
	}
}

func TestArgumentsAndOutputErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"-unknown"}, {"extra"}, {"-out", ""}, {"-out", filepath.Join(t.TempDir(), "absent", "cloudflare.json")}, {"-out", t.TempDir()}} {
		if err := run(t.Context(), fixtureClient(t, false), args, io.Discard); err == nil {
			t.Fatalf("accepted invalid arguments/output %v", args)
		}
	}
}

func TestExternalConsumer(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := run(t.Context(), fixtureClient(t, false), []string{"-out", filepath.Join(dir, "cloudflare.json")}, io.Discard); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"), []byte("module consumer.example/app\n\ngo 1.27.0\n\nrequire statute.kjanat.dev v0.0.0\n\nreplace statute.kjanat.dev => "+root+"\n"))
	writeFile(t, filepath.Join(dir, "go.sum"), readFile(t, filepath.Join(root, "go.sum")))
	writeFile(t, filepath.Join(dir, "main.go"), []byte(consumerSource))
	cmd := exec.CommandContext(t.Context(), testGoBinary(t), "run", "-mod=mod", ".") //nolint:gosec // G204: trusted test toolchain path; fixed arguments keep the external consumer offline.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	output, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "192.0.2.0/24" {
		t.Fatalf("external consumer: %v\n%s", err, output)
	}
}

func testGoBinary(t *testing.T) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "env", "GOROOT")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(strings.TrimSpace(string(output)), "bin", "go")
}

const consumerSource = `package main

import (
    _ "embed"
    "fmt"
    "statute.kjanat.dev"
)

//go:embed cloudflare.json
var fallback []byte

func main() {
    cfg, err := statute.Resolve(statute.Config{
        Listeners: []*statute.Listener{
            statute.HTTPS(":443", statute.StaticTLS("cert.pem", "key.pem"),
                statute.CloudflareTrustedProxy().FallbackSnapshot(fallback)),
        },
    })
    if err != nil { panic(err) }
    fmt.Println(cfg.Listeners[0].CloudflareFallback.IPv4[0])
}
`

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
