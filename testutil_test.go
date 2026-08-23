package statute

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

// echoRequest is what newEchoBackend returns as a JSON body for every
// request. Test code reads this back to assert the upstream saw the right
// headers, method, path, etc.
type echoRequest struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Host    string              `json:"host"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

// newEchoBackend returns an httptest.Server that echoes every request back
// as a JSON-encoded echoRequest. Callers can also set response headers via
// the X-Test-Set-Header inbound request header (format: "Name: value"); the
// backend will copy it into the response so middleware tests can assert
// header propagation in both directions.
func newEchoBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		if h := r.Header.Get("X-Test-Set-Header"); h != "" {
			if k, v, ok := strings.Cut(h, ": "); ok {
				w.Header().Set(k, v)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(echoRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Host:    r.Host,
			Headers: r.Header,
			Body:    string(body),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mustResolve resolves the given surface Config and fails the test on error.
func mustResolve(t *testing.T, cfg Config) *resolved.Config {
	t.Helper()
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return r
}

// runRequest runs req through handler and returns the recorder.
func runRequest(t *testing.T, handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// assertHeader fails the test if h[name] does not equal want.
func assertHeader(t *testing.T, h http.Header, name, want string) {
	t.Helper()
	got := h.Get(name)
	if got != want {
		t.Errorf("header %s: got %q, want %q", name, got, want)
	}
}

// assertNoHeader fails the test if h has any value for the named header.
func assertNoHeader(t *testing.T, h http.Header, name string) {
	t.Helper()
	if got := h.Get(name); got != "" {
		t.Errorf("header %s: want absent, got %q", name, got)
	}
}

// decodeEcho parses the response body as an echoRequest.
func decodeEcho(t *testing.T, body io.Reader) echoRequest {
	t.Helper()
	var e echoRequest
	if err := json.NewDecoder(body).Decode(&e); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	return e
}

// writeFile writes name (relative to dir) with the given contents.
func writeFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// assertEchoHeader fails the test unless the upstream saw exactly one value
// for the named header. Names are canonicalised before the lookup: the echo
// map's keys come from http.Header, which only holds canonical ones.
func assertEchoHeader(t *testing.T, e echoRequest, name, want string) {
	t.Helper()
	got := e.Headers[textproto.CanonicalMIMEHeaderKey(name)]
	if len(got) != 1 || got[0] != want {
		t.Errorf("upstream %s: got %v, want [%s]", name, got, want)
	}
}

// assertNoEchoHeader fails the test if the upstream saw the named header.
func assertNoEchoHeader(t *testing.T, e echoRequest, name string) {
	t.Helper()
	if got, ok := e.Headers[textproto.CanonicalMIMEHeaderKey(name)]; ok {
		t.Errorf("upstream saw %s: %v, want it absent", name, got)
	}
}

// readerFromRecorder is a ResponseRecorder with an io.ReaderFrom, recording
// whether a body copy was delegated to it rather than looped through Write.
type readerFromRecorder struct {
	*httptest.ResponseRecorder
	readFrom bool
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.readFrom = true
	return io.Copy(r.ResponseRecorder, src)
}

// plainReader hides any WriterTo the wrapped reader has, forcing io.Copy to
// consult the destination's ReadFrom — the same shape http.ServeContent
// produces when it copies a file through io.CopyN.
type plainReader struct{ io.Reader }
