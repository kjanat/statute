//go:build !statute_htmlrewrite

package htmlrewrite

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRewriteUnavailable(t *testing.T) {
	if e, err := newEngine(t.Context()); e != nil || !errors.Is(err, errRewriteUnavailable) {
		t.Fatalf("engine: %v, %v", e, err)
	}
	if e, err := newHTTPEngine(t.Context(), 1); e != nil || !errors.Is(err, errRewriteUnavailable) {
		t.Fatalf("HTTP engine: %v, %v", e, err)
	}
	if !strings.Contains(errRewriteUnavailable.Error(), "-tags statute_htmlrewrite") {
		t.Fatal("missing rebuild instruction")
	}
	for _, failure := range []failurePolicy{failOpen, failClosed} {
		p := httpPolicy{failure: failure, inputLimit: 1024, outputLimit: 1024, timeout: time.Second}
		if transport, err := newRewriteTransport(nil, http.DefaultTransport, p); transport != nil || err == nil {
			t.Fatalf("policy %v accepted missing engine: %v, %v", failure, transport, err)
		}
	}
	var e engine
	if s, err := e.newStream(t.Context(), io.Discard, 1024); s != nil || !errors.Is(err, errRewriteUnavailable) {
		t.Fatalf("stream: %v, %v", s, err)
	}
	var s stream
	if !errors.Is(s.write([]byte("<p>test</p>")), errRewriteUnavailable) || !errors.Is(s.finish(), errRewriteUnavailable) {
		t.Fatal("disabled stream accepted input")
	}
	if err := errors.Join(s.close(), e.close()); err != nil {
		t.Fatal(err)
	}
}
