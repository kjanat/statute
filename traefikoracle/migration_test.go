package traefikoracle

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/traefik/traefik/v3/pkg/config/dynamic"
	"github.com/traefik/traefik/v3/pkg/middlewares/headers"
	"github.com/traefik/traefik/v3/pkg/middlewares/redirect"

	"statute.kjanat.dev/internal/migrate"
)

// Compare the emitted, unmodified program with Traefik's actual middleware.
func TestGeneratedMigrationMatchesTraefikMiddleware(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "upstream")
		w.Header().Set("Strict-Transport-Security", "upstream")
		w.WriteHeader(http.StatusNoContent)
	})
	origin := httptest.NewServer(backend)
	defer origin.Close()
	address := migrationProgram(t, origin.URL)
	ctx := t.Context()
	maxAge := int64(31536000)
	security, err := headers.New(ctx, backend, dynamic.Headers{
		ContentTypeNosniff: true, FrameDeny: true, STSSeconds: &maxAge,
		STSIncludeSubdomains: true, STSPreload: true, ReferrerPolicy: "strict-origin-when-cross-origin",
	}, "browser-policy")
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := redirect.NewRedirectScheme(ctx, backend, dynamic.RedirectScheme{Scheme: "https", Permanent: true}, "scheme")
	if err != nil {
		t.Fatal(err)
	}
	regex, err := redirect.NewRedirectRegex(ctx, backend, dynamic.RedirectRegex{
		Regex: `^https?://[^/]+/(.*)`, Replacement: "https://new.example/${1}", Permanent: true,
	}, "regex")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for path, source := range map[string]http.Handler{"/headers": security, "/scheme": scheme, "/capture": regex} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, "CUSTOM"} {
			t.Run(path+"/"+method, func(t *testing.T) {
				uri := path + "?x=a%2Fb&empty=&q=%E2%82%AC"
				request := httptest.NewRequest(method, "http://api.example"+uri, nil)
				request.RequestURI = uri
				expected := httptest.NewRecorder()
				source.ServeHTTP(expected, request)
				req, err := http.NewRequestWithContext(ctx, method, "http://"+address+uri, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = "api.example"
				got, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer got.Body.Close()
				if got.StatusCode != expected.Code {
					t.Errorf("status: generated=%d Traefik=%d", got.StatusCode, expected.Code)
				}
				for _, name := range []string{"Location", "X-Content-Type-Options", "X-Frame-Options", "Strict-Transport-Security", "Referrer-Policy"} {
					if got.Header.Get(name) != expected.Header().Get(name) {
						t.Errorf("%s: generated=%q Traefik=%q", name, got.Header.Get(name), expected.Header().Get(name))
					}
				}
			})
		}
	}
}

func migrationProgram(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	static := filepath.Join(dir, "static.yml")
	file := filepath.Join(dir, "dynamic.yml")
	writeMigrationFile(t, static, []byte(fmt.Sprintf("entryPoints:\n  web: {address: %q}\n", address)))
	writeMigrationFile(t, file, []byte(fmt.Sprintf(`http:
  routers:
    browser: {rule: 'Host(%s) && Path(%s)', service: origin, middlewares: [browser]}
    scheme: {rule: 'Host(%s) && Path(%s)', service: origin, middlewares: [scheme]}
    capture: {rule: 'Host(%s) && Path(%s)', service: origin, middlewares: [capture]}
  services:
    origin:
      loadBalancer:
        servers: [{url: %q}]
  middlewares:
    browser:
      headers: {contentTypeNosniff: true, frameDeny: true, stsSeconds: 31536000, stsIncludeSubdomains: true, stsPreload: true, referrerPolicy: strict-origin-when-cross-origin}
    scheme:
      redirectScheme: {scheme: https, permanent: true}
    capture:
      redirectRegex: {regex: '^https?://[^/]+/(.*)', replacement: 'https://new.example/${1}', permanent: true}
`, "`api.example`", "`/headers`", "`api.example`", "`/scheme`", "`api.example`", "`/capture`", origin)))
	r, err := migrate.Audit(migrate.Inputs{Static: []string{static}, Dynamic: []string{file}})
	if err != nil || r.Report.Blocking {
		t.Fatalf("synthetic migration blocked: %v %+v", err, r)
	}
	source, binary := filepath.Join(dir, "main.go"), filepath.Join(dir, "proxy")
	writeMigrationFile(t, source, r.Go)
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatalf("build generated program: %v\n%s", err, out)
	}
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
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", address)
		if err == nil {
			_ = conn.Close()
			return address
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("generated program did not start")
	return ""
}

func writeMigrationFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
