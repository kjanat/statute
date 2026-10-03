package htmlrewrite

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev"
	"statute.kjanat.dev/resolved"
)

// validateHTTPConfig is the private experiment's startup gate. Only its direct
// reverse-proxy route actions are supported; this is not a public mechanism for
// discovering transformations hidden inside arbitrary application handlers.
func validateHTTPConfig(cfg statute.Config) error {
	rc, err := statute.Resolve(cfg)
	if err != nil {
		return err
	}
	for _, table := range []struct {
		name   string
		routes []*resolved.Route
	}{{"route", rc.Routes}, {"fallback_routes", rc.FallbackRoutes}} {
		for i, route := range table.routes {
			if err := validateHTTPRoute(route); err != nil {
				return fmt.Errorf("%s[%d]: %w", table.name, i, err)
			}
		}
	}
	return nil
}

func validateHTTPRoute(route *resolved.Route) error {
	proxy, ok := route.Handler.(*httputil.ReverseProxy)
	if !ok {
		return nil
	}
	if _, ok := proxy.Transport.(*rewriteTransport); !ok {
		return nil
	}
	for i, mw := range route.Middleware {
		if mw.Type != resolved.MWSetResponseHeader && mw.Type != resolved.MWAddResponseHeader {
			continue
		}
		if ownsRepresentationHeader(mw.HeaderName) {
			return fmt.Errorf("middleware[%d]: cannot set or append %s on an HTML-rewriting route; the representation producer owns this header (removal is allowed)", i, mw.HeaderName)
		}
	}
	return nil
}

func ownsRepresentationHeader(name string) bool {
	switch strings.ToLower(name) {
	case "content-type", "content-length", "content-encoding", "etag", "last-modified",
		"content-md5", "digest", "content-digest", "repr-digest", "accept-ranges",
		"content-range", "transfer-encoding", "trailer":
		return true
	default:
		return false
	}
}

func TestHTTPConfigRepresentationHeaders(t *testing.T) {
	proxy := &httputil.ReverseProxy{Transport: &rewriteTransport{}}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding", "ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Accept-Ranges", "Content-Range", "Transfer-Encoding", "Trailer"} {
		for _, op := range []struct {
			name string
			mw   statute.Middleware
		}{{"set", statute.SetResponseHeader(strings.ToLower(name), "value")}, {"append", statute.AddResponseHeader(name, "value")}} {
			t.Run(name+"/"+op.name, func(t *testing.T) {
				// Hoisting must not make declaration order or Retry a loophole.
				for _, mws := range [][]statute.Middleware{
					{op.mw, statute.Retry(2, statute.OnStatus(503)), statute.RemoveResponseHeader(name)},
					{statute.RemoveResponseHeader(name), statute.Compress(statute.Gzip), op.mw},
				} {
					route := statute.Match("/*").Handle(proxy).With(mws...)
					for _, fallback := range []bool{false, true} {
						cfg := statute.Config{Listeners: statute.Listeners{statute.HTTP(":8080")}, Routes: statute.Routes{route}}
						path := "route[0]"
						if fallback {
							cfg.Routes, cfg.FallbackRoutes = nil, cfg.Routes
							path = "fallback_routes[0]"
						}
						if err := validateHTTPConfig(cfg); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "representation producer") {
							t.Fatalf("conflict did not fail configuration: %v", err)
						}
					}
				}
			})
		}
	}
}

func TestHTTPConfigAllowedHeaders(t *testing.T) {
	base := http.DefaultTransport
	proxy := &httputil.ReverseProxy{Transport: &rewriteTransport{base: base}}
	var removes []statute.Middleware
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding", "ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Accept-Ranges", "Content-Range", "Transfer-Encoding", "Trailer"} {
		removes = append(removes, statute.RemoveResponseHeader(name))
	}
	cfg := statute.Config{
		Listeners: statute.Listeners{statute.HTTP(":8080")},
		Routes: statute.Routes{
			statute.Match("/remove").Handle(proxy).With(removes...),
			statute.Match("/ordinary").Handle(proxy).With(statute.SetResponseHeader("Content-Security-Policy", "default-src 'self'"), statute.AddResponseHeader("Set-Cookie", "key=value"), statute.SetResponseHeader("X-Test", "ok"), statute.SetRequestHeader("If-Match", `"origin"`), statute.Compress(statute.Gzip)),
			// The same base transport does not make a sibling a rewriting route.
			statute.Match("/plain").Handle(&httputil.ReverseProxy{Transport: base}).With(statute.SetResponseHeader("ETag", `"manual"`)),
		},
	}
	if err := validateHTTPConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPStatuteConflictingHeadersPreventStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHTTPStatuteProcess$")
	cmd.Env = append(os.Environ(), "STATUTE_HTML_HTTP_CHILD=1", "STATUTE_HTML_HTTP_ORIGIN=http://127.0.0.1:1", "STATUTE_HTML_HTTP_ADDR=127.0.0.1:0", "STATUTE_HTML_HTTP_CONFLICT=eTaG")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "representation producer owns this header") || strings.Contains(string(out), "statute: ready") {
		t.Fatalf("expected configuration rejection before serving: %v\n%s", err, out)
	}
}

func TestHTTPStatuteAllowedHeaders(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("ETag", `"origin"`)
		_, _ = io.WriteString(w, `<a class="rewrite">page</a>`)
	}))
	defer origin.Close()
	endpoint := startHTTPStatute(t, origin.URL)
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	res, err := client.Get(endpoint + "/headers")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || !strings.Contains(string(body), "https://example.invalid/rewritten") {
		t.Fatalf("rewritten gzip body: %s, %v", body, err)
	}
	if !res.Uncompressed || res.Header.Get("ETag") != "" || res.Header.Get("Content-Security-Policy") != "default-src 'self'" || res.Header.Get("Set-Cookie") != "key=value" {
		t.Fatalf("allowed header operations or compression failed: %+v", res)
	}
}
