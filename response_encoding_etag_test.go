package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponseEncodingETagChecksSelectedRepresentation(t *testing.T) {
	for _, coding := range []string{"br", "zstd"} {
		for _, minimum := range []int{1, 4096} {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, strings.Repeat("body", 256))
			}), EncodeResponse(EncodingPolicy{Encodings: []string{coding}, MinimumBytes: minimum}), ETag())
			identity := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
			tag := identity.Header().Get("ETag")
			if tag == "" || strings.HasPrefix(tag, "W/") {
				t.Fatal("identity lacks strong validator")
			}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Accept-Encoding", coding)
			r.Header.Set("If-Match", tag)
			w := runRequest(t, h, r)
			want := http.StatusPreconditionFailed
			if minimum == 4096 {
				want = http.StatusOK
			}
			if w.Code != want {
				t.Fatalf("coding=%s minimum=%d If-Match status=%d want=%d", coding, minimum, w.Code, want)
			}
			r.Header.Del("If-Match")
			r.Header.Set("If-None-Match", tag)
			w = runRequest(t, h, r)
			if w.Code != http.StatusNotModified {
				t.Fatalf("If-None-Match status=%d", w.Code)
			}
		}
	}
}
