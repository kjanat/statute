package statute

import (
	"net/http/httptest"
	"testing"
)

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"/", "/", true},
		{"/api", "/api", true},
		{"/api", "/api/v1", false},
		{"/api/*", "/api", true},
		{"/api/*", "/api/v1", true},
		{"/api/*", "/api/v1/users", true},
		{"/api/*", "/apix", false},
		{"/*", "/", true},
		{"/*", "/anything", true},
		{"/static/*", "/static/css/app.css", true},
		{"/static/*", "/staticfile", false},
	}
	for _, c := range cases {
		got := matchPattern(c.pattern, c.path)
		if got != c.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestStripPort(t *testing.T) {
	cases := map[string]string{
		"example.com":     "example.com",
		"example.com:443": "example.com",
		"127.0.0.1:8080":  "127.0.0.1",
		"[::1]:8080":      "[::1]:8080", // conservative: keep IPv6 with brackets untouched
		"localhost":       "localhost",
	}
	for in, want := range cases {
		got := stripPort(in)
		if got != want {
			t.Errorf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientIPXFF(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.5, 198.51.100.1")
	if got := clientIP(r); got != "203.0.113.5" {
		t.Errorf("XFF: got %q, want %q", got, "203.0.113.5")
	}

	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "10.0.0.2:5678"
	if got := clientIP(r2); got != "10.0.0.2:5678" {
		t.Errorf("no XFF: got %q, want %q", got, "10.0.0.2:5678")
	}
}
