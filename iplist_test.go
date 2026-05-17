package statute

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllowIPs_Allow(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := allowIPsHandler(mustResolveMW(t, AllowIPs("10.0.0.0/8", "203.0.113.0/24")), inner)

	cases := []struct {
		remote string
		want   int
	}{
		{"10.5.5.5:1234", http.StatusOK},
		{"203.0.113.7:1234", http.StatusOK},
		{"198.51.100.1:1234", http.StatusForbidden},
		{"172.16.0.1:1234", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.remote, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = c.remote
			rec := runRequest(t, h, req)
			if rec.Code != c.want {
				t.Errorf("got %d, want %d", rec.Code, c.want)
			}
		})
	}
}

func TestDenyIPs_Deny(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := denyIPsHandler(mustResolveMW(t, DenyIPs("10.0.0.0/8")), inner)

	cases := []struct {
		remote string
		want   int
	}{
		{"10.5.5.5:1234", http.StatusForbidden},
		{"198.51.100.1:1234", http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		rec := runRequest(t, h, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.remote, rec.Code, c.want)
		}
	}
}

func TestIPList_IPv6(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := allowIPsHandler(mustResolveMW(t, AllowIPs("2001:db8::/32")), inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "[2001:db8::1]:1234"
	rec := runRequest(t, h, req)
	if rec.Code != http.StatusOK {
		t.Errorf("IPv6 in range got %d", rec.Code)
	}

	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "[2001:db9::1]:1234"
	rec2 := runRequest(t, h, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("IPv6 outside range got %d, want 403", rec2.Code)
	}
}

func TestIPList_InvalidCIDRRejectedAtResolve(t *testing.T) {
	t.Parallel()
	_, err := resolveMiddleware(AllowIPs("not-a-cidr"))
	if err == nil {
		t.Fatal("want error for bogus CIDR")
	}
}

func TestIPList_CanonicalisedAtResolve(t *testing.T) {
	t.Parallel()
	// "10.0.0.1/24" should resolve to "10.0.0.0/24" (host bits masked).
	r, err := resolveMiddleware(AllowIPs("10.0.0.1/24"))
	if err != nil {
		t.Fatal(err)
	}
	if r.IPCIDRs[0] != "10.0.0.0/24" {
		t.Errorf("not canonicalised: %q", r.IPCIDRs[0])
	}
}
