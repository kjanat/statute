package statute

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestRequestPathOperationsInsideRetry(t *testing.T) {
	for _, tc := range []struct {
		name, input, path, header, value string
		mw                               Middleware
	}{
		{"replace", "/old%2Fpath", "/new%2Fvalue", "X-Replaced-Path", "/old%2Fpath", ReplaceEscapedPath("/new%2Fvalue").RecordOriginal()},
		{"regexp", "/old/a%2Fb", "/new/a%2Fb", "X-Replaced-Path", "/old/a%2Fb", RewriteEscapedPath("^/old/(.*)$", "/new/$1").RecordOriginal()},
		{"no match", "/other", "/other", "X-Replaced-Path", "", RewriteEscapedPath("^/old/(.*)$", "/new/$1").RecordOriginal()},
		{"prefix", "/old%2Fpath", "/v1/old%2Fpath", "X-Replaced-Path", "", PrefixPath("/v1")},
		{"unicode prefix", "/old%2Fpath", "/caf%C3%A9/old%2Fpath", "X-Replaced-Path", "", PrefixPath("/café")},
		{"space prefix", "/old%2fpath", "/two%20words/old%2fpath", "X-Replaced-Path", "", PrefixPath("/two words")},
		{"literal percent prefix", "/old%2Fpath", "/v%252F/old%2Fpath", "X-Replaced-Path", "", PrefixPath("/v%2F")},
		{"reserved prefix", "/old%2Fpath", "/a%3Fb%23c/old%2Fpath", "X-Replaced-Path", "", PrefixPath("/a?b#c")},
		{"byte strip", "/api/thing%2Ftail", "/i/thing%2Ftail", "X-Forwarded-Prefix", "/ap", StripBytePrefixes("/absent", "/ap").RecordOriginal()},
		{"empty strip", "/api", "/", "X-Forwarded-Prefix", "/api", StripBytePrefixes("/api").RecordOriginal().ForceSlash()},
		{"no strip", "/other", "/other", "X-Forwarded-Prefix", "", StripBytePrefixes("/api").RecordOriginal()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if got := r.URL.RequestURI(); got != tc.path+"?q=a%2Fb" {
					t.Errorf("attempt %d target=%q", calls, got)
				}
				if got := r.Header.Get(tc.header); got != tc.value {
					t.Errorf("attempt %d metadata=%q", calls, got)
				}
				if len(r.Header.Values(tc.header)) > 1 {
					t.Error("metadata accumulated on retry")
				}
				w.WriteHeader(http.StatusBadGateway)
			}), Retry(2, OnStatus(http.StatusBadGateway)), InOrder(tc.mw))
			r := httptest.NewRequest(http.MethodGet, tc.input+"?q=a%2Fb", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if calls != 2 {
				t.Fatalf("attempts=%d", calls)
			}
			if r.URL.RequestURI() != tc.input+"?q=a%2Fb" {
				t.Fatal("changed original request")
			}
		})
	}
}

func TestRequestPathRejectsUnsafeRewrite(t *testing.T) {
	h := chain(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unsafe path reached handler") }), RewriteEscapedPath(".*", "/../private"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/public", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatal(w.Code)
	}
}

func TestRequestPathValidation(t *testing.T) {
	for _, mw := range []Middleware{ReplaceEscapedPath("/%ZZ"), ReplaceEscapedPath("/bad\npath"), RewriteEscapedPath("[", "/"), PrefixPath("")} {
		if _, err := resolveMiddleware(mw); err == nil {
			t.Fatal("invalid path policy accepted")
		}
	}
}

func TestRequestPathInvalidResolvedTransformsRefuse(t *testing.T) {
	for _, tc := range []struct {
		mw     resolved.Middleware
		status int
	}{
		{resolved.Middleware{Type: resolved.MWRewriteEscapedPath, PathPattern: "["}, 500},
		{resolved.Middleware{Type: resolved.MWRewriteEscapedPath, PathPattern: ".*", PathReplacement: "/%ZZ"}, 500},
	} {
		w := httptest.NewRecorder()
		requestPathHandler(tc.mw, statusHandler(http.StatusTeapot)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/source", nil))
		if w.Code != tc.status {
			t.Fatalf("invalid path transform status=%d", w.Code)
		}
	}
}
