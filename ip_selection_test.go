package statute

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIPSelectionUsesDeclaredAddressSource(t *testing.T) {
	subnet := 64
	for _, tc := range []struct {
		policy                IPSelection
		peer, forwarded, want string
	}{
		{IPSelection{Mode: "peer"}, "192.0.2.1:80", "198.51.100.1", "192.0.2.1"},
		{IPSelection{Mode: "forwarded", Depth: 2}, "127.0.0.1:80", "198.51.100.1, 192.0.2.1", "198.51.100.1"},
		{IPSelection{Mode: "forwarded", Depth: 3}, "127.0.0.1:80", "192.0.2.1", ""},
		{IPSelection{Mode: "forwarded", ExcludedIPs: []string{"192.0.2.0/24"}}, "127.0.0.1:80", "198.51.100.1, ,192.0.2.1", "198.51.100.1"},
		{IPSelection{Mode: "peer", IPv6Subnet: &subnet}, "[2001:db8::1234]:80", "", "2001:db8::"},
	} {
		p, err := resolveIPSelection(&tc.policy)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", tc.forwarded)
		if got := selectedRequestIP(p, r); got != tc.want {
			t.Fatalf("policy=%+v got=%q want=%q", tc.policy, got, tc.want)
		}
	}
}

func TestIPSelectionRejectsInvalidPolicy(t *testing.T) {
	negative, oversized := -1, 129
	for _, policy := range []IPSelection{{Mode: "unknown"}, {Mode: "peer", Depth: -1}, {Mode: "forwarded", ExcludedIPs: []string{"invalid"}}, {Mode: "peer", IPv6Subnet: &negative}, {Mode: "peer", IPv6Subnet: &oversized}} {
		if _, err := resolveMiddleware(AllowIPs("192.0.2.0/24").SelectIP(policy)); err == nil {
			t.Fatal("invalid address policy accepted")
		}
	}
}

func TestRateLimitKeysRemainIndependent(t *testing.T) {
	for _, mw := range []Middleware{RateLimit("1/h").Burst(1).Reserve("0s").FromHeader("X-Client"), RateLimit("1/h").Burst(1).Reserve("0s").Per(PeerIP)} {
		h := chain(t, statusHandler(http.StatusNoContent), mw)
		for _, tc := range []struct {
			client string
			status int
		}{{"192.0.2.1", 204}, {"192.0.2.1", 429}, {"192.0.2.2", 204}} {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.client + ":1234"
			r.Header.Set("X-Client", tc.client)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("client=%s status=%d", tc.client, w.Code)
			}
		}
	}
}
