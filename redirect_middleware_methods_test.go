package statute

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSchemeRedirectPreservesIPv6Authority(t *testing.T) {
	for _, tc := range []struct{ authority, port, want string }{
		{"[::1]:8080", "", "https://[::1]/path?q=1"},
		{"[2001:db8::1]:8080", "8443", "https://[2001:db8::1]:8443/path?q=1"},
		{"[::1]", "", "https://[::1]/path?q=1"},
	} {
		h := chain(t, http.NotFoundHandler(), RedirectScheme("https").Port(tc.port))
		r := httptest.NewRequest(http.MethodGet, "http://"+tc.authority+"/path?q=1", nil)
		r.RequestURI = "/path?q=1"
		w := runRequest(t, h, r)
		if got := w.Header().Get("Location"); got != tc.want {
			t.Fatalf("authority=%s Location=%q want=%q", tc.authority, got, tc.want)
		}
	}
}

func TestConditionalRedirectMethodsAndAbsoluteURIs(t *testing.T) {
	for _, tc := range []struct {
		name, method, url, location string
		policy                      Middleware
		emptyURI                    bool
		status                      int
	}{
		{"absolute scheme", "GET", "http://api.example:8080/path?q=1", "https://api.example:8443/path?q=1", RedirectScheme("https").Port("8443"), false, 302},
		{"non GET", "POST", "http://api.example/path", "https://api.example/path", RedirectScheme("https"), false, 307},
		{"permanent HEAD", "HEAD", "http://api.example/path", "https://new.example/path", RedirectRegex(`^http://api\.example/(.*)$`, "https://new.example/${1}").Permanent(), false, 308},
		{"explicit status", "POST", "http://api.example/path", "https://new.example/path", RedirectRegex(`^http://api\.example/(.*)$`, "https://new.example/${1}").Status(303), false, 303},
		{"empty request URI", "GET", "http://api.example/path?q=1", "https://api.example/path?q=1", RedirectScheme("https"), true, 302},
		{"equal standard port", "GET", "http://api.example:80/path", "", RedirectScheme("http").Port("80"), false, 204},
		{"invalid target", "GET", "http://api.example/path", "", RedirectRegex(`^http://api\.example/(.*)$`, "http://[invalid"), false, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := chain(t, statusHandler(http.StatusNoContent), tc.policy)
			r := httptest.NewRequest(tc.method, tc.url, nil)
			if tc.emptyURI {
				r.RequestURI = ""
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Location") != tc.location {
				t.Fatalf("status=%d location=%q", w.Code, w.Header().Get("Location"))
			}
		})
	}
}
