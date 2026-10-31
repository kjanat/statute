package migrate

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGeneratedCandidateServesSanitizedRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "backend")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// #nosec G705 -- Test fixture returns text/plain, never HTML; echo proves the generated proxy request target.
		_, _ = io.WriteString(w, r.URL.RequestURI()+"|"+r.Header.Get("X-Migration-Token")+"|"+r.Header.Get("X-Real-IP"))
	}))
	defer backend.Close()
	address := testListenAddress(t)
	body := strings.ReplaceAll(simpleDynamic, "http://127.0.0.1:9000", backend.URL)
	body = strings.Replace(body, "service: api", "service: api\n      middlewares: [headers, prefix]", 1)
	body += `  middlewares:
    headers:
      headers:
        customRequestHeaders: {X-Migration-Token: '${STATUTE_TEST_MIGRATION_TOKEN}'}
        customResponseHeaders: {Server: ''}
    prefix:
      addPrefix: {prefix: /v1}
`
	in := basicInputs(t, body)
	in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  web: {address: '"+address+"', http: {sanitizePath: false}}\n")}
	result := audit(t, in)
	requireMigrationGaps(t, result)
	binary := compileGenerated(t, result)
	// A missing binding must fail validation before any listener starts.
	// #nosec G204 -- Execute only the generated test binary.
	missing := exec.CommandContext(t.Context(), binary, "-validate")
	missing.Env = []string{"STATUTE_TEST_MIGRATION_TOKEN="}
	if err := missing.Run(); err == nil {
		t.Fatal("missing binding accepted")
	}
	t.Setenv("STATUTE_TEST_MIGRATION_TOKEN", "bound-token")
	// #nosec G204 -- Execute only the generated test binary.
	cmd := exec.CommandContext(t.Context(), binary)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	response := waitGeneratedResponse(t, client, "http://"+address+"/health?x=1")
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("status: %d", response.StatusCode)
	}
	if string(data) != "/v1/health?x=1|bound-token|127.0.0.1" || response.Header.Get("Server") != "" {
		t.Fatalf("generated route behavior: status=%d body=%q server=%q", response.StatusCode, data, response.Header.Get("Server"))
	}
	assertUnmatched(t, client, address)
}

func compileGenerated(t *testing.T, r *Result) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	binary := filepath.Join(dir, "proxy")
	if err := os.WriteFile(source, r.Go, 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G204 -- Compile the generated fixture in a test-owned directory.
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", binary, source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile generated fixture: %v\n%s", err, out)
	}
	return binary
}

func waitGeneratedResponse(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "api.example"
		req.Header.Set("X-Real-IP", "198.51.100.123")
		response, err := client.Do(req)
		if err == nil {
			return response
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("generated fixture did not serve within startup deadline")
	return nil
}

func testListenAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func assertUnmatched(t *testing.T, client *http.Client, address string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/unmatched", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "api.example"
	miss, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer miss.Body.Close()
	if miss.StatusCode != 404 {
		t.Fatalf("unmatched request returned %d", miss.StatusCode)
	}
}
