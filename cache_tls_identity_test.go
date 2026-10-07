package statute

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCacheTLSIdentityIsolation(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"GET", "HEAD"} {
		for _, innerCache := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/inner=%v", method, innerCache), func(t *testing.T) {
				t.Parallel()
				calls := 0
				mws := []Middleware{Cache("1h"), ETag(), Retry(2)}
				if innerCache {
					mws = []Middleware{ETag(), Retry(2), Cache("1h")}
				}
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.Header().Set("Cache-Control", "public, s-maxage=3600")
					w.Header().Set("X-Call", fmt.Sprint(calls))
				}), mws...)
				cert := &x509.Certificate{}
				for _, tc := range []struct {
					state *tls.ConnectionState
					call  string
				}{
					{&tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}, "1"},
					{&tls.ConnectionState{}, "2"},
					{&tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}, "3"},
					{&tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}, "4"},
					{&tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}, "5"},
					{&tls.ConnectionState{}, "2"},
				} {
					r := httptest.NewRequest(method, "https://example.com/", nil)
					r.TLS = tc.state
					rec := runRequest(t, h, r)
					if rec.Code != 200 || rec.Header().Get("X-Call") != tc.call {
						t.Fatalf("TLS=%+v: status=%d headers=%v want call=%s", tc.state, rec.Code, rec.Header(), tc.call)
					}
				}
			})
		}
	}
}
