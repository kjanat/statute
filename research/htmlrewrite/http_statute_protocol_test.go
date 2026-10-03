package htmlrewrite

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

// The child runs Statute's TCP/TLS and QUIC listener construction and shutdown.
func startHTTPStatuteTLS(t *testing.T, origin string, major int) (string, *http.Client) {
	t.Helper()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	defer fixture.Close()
	cert := fixture.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for path, data := range map[string][]byte{
		certPath: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		keyPath:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	endpoint, _ := startHTTPStatuteConfigured(t, origin, "", []string{
		"STATUTE_HTML_HTTP_CERT=" + certPath, "STATUTE_HTML_HTTP_KEY=" + keyPath,
		"STATUTE_HTML_HTTP_SINGLE_INSTANCE=1", "STATUTE_HTML_HTTP_SMALL_OUTPUT=1",
	})
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	tlsConfig := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
	var transport http.RoundTripper
	if major == 2 {
		tr := &http2.Transport{TLSClientConfig: tlsConfig, DisableCompression: true}
		transport = tr
		t.Cleanup(tr.CloseIdleConnections)
	} else {
		tr := &http3.Transport{TLSClientConfig: tlsConfig, DisableCompression: true}
		transport = tr
		t.Cleanup(func() { _ = tr.Close() })
	}
	return "https://" + strings.TrimPrefix(endpoint, "http://"), &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestHTTPStatuteProtocolInterruption(t *testing.T) {
	for _, major := range []int{2, 3} {
		for _, route := range []string{"/strict", "/open"} {
			for _, outcome := range []string{"disconnect", "output limit"} {
				t.Run(fmt.Sprintf("HTTP%d%s/%s", major, route, outcome), func(t *testing.T) {
					testStatuteProtocolInterruption(t, major, route, outcome)
				})
			}
		}
	}
}

func testStatuteProtocolInterruption(t *testing.T, major int, route, outcome string) {
	t.Helper()
	originDone, release := make(chan struct{}), make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/healthy" {
			_, _ = io.WriteString(w, `<a class="rewrite">healthy</a>`)
			return
		}
		defer close(originDone)
		_, _ = io.WriteString(w, `<a class="rewrite">prefix</a>`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, strings.Repeat("tail", 4096))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(origin.Close)
	endpoint, client := startHTTPStatuteTLS(t, origin.URL, major)
	res, err := client.Get(endpoint + route)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.ProtoMajor != major || res.StatusCode != 200 {
		t.Fatalf("wrong response: %s/%d", res.Proto, res.StatusCode)
	}
	prefix := make([]byte, len(`<a class="rewrite" href="https://example.invalid/rewritten">prefix<em>inserted</em>`))
	if _, err := io.ReadFull(res.Body, prefix); err != nil || !bytes.Contains(prefix, []byte("inserted")) {
		t.Fatalf("no rewritten prefix before EOF: %q %v", prefix, err)
	}
	if outcome == "disconnect" {
		_ = res.Body.Close()
	} else {
		close(release)
		_, err = io.ReadAll(res.Body)
		assertStatuteStreamReset(t, major, err)
	}
	select {
	case <-originDone:
	case <-time.After(5 * time.Second):
		t.Fatal("interruption did not release the origin request")
	}
	assertStatuteRewriteRecovery(t, client, endpoint+"/healthy", major)
}

func assertStatuteStreamReset(t *testing.T, major int, err error) {
	t.Helper()
	if major == 3 {
		if !errors.Is(err, &http3.Error{Remote: true, ErrorCode: http3.ErrCodeInternalError}) {
			t.Fatalf("expected HTTP/3 stream reset: %v", err)
		}
		return
	}
	var streamErr http2.StreamError
	if !errors.As(err, &streamErr) || streamErr.Code != http2.ErrCodeInternal {
		t.Fatalf("expected HTTP/2 stream reset: %v", err)
	}
}

// A successful rewrite requires the child's sole admission slot to be free.
func assertStatuteRewriteRecovery(t *testing.T, client *http.Client, endpoint string, major int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := client.Get(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode == 200 && res.ProtoMajor == major && bytes.Count(body, []byte("<em>inserted</em>")) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission not recovered: %d %q", res.StatusCode, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
