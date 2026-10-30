package statute

import (
	"testing"
)

func TestRateLimitRejectsInvalidReservationAndIdentityPolicy(t *testing.T) {
	for _, mw := range []Middleware{
		RateLimit("invalid"), RateLimit("1/s").Per(RateLimitKey(100)), RateLimit("1/s").Reserve("invalid"), RateLimit("1/s").Reserve("-1s"),
		RateLimit("1/s").ExpireIdle("0s"), RateLimit("1/s").ExpireIdle("invalid"), RateLimit("1/s").EvictOldest(), RateLimit("1/s").FromHeader("Bad\nHeader"), RateLimit("1/s").SelectIP(IPSelection{Mode: "unknown"}),
	} {
		if _, err := resolveMiddleware(mw); err == nil {
			t.Fatal("invalid limiter policy accepted")
		}
	}
}

func TestProbePolicyRejectsInvalidWireValues(t *testing.T) {
	for _, policy := range []HealthCheck{
		{Path: "/", Redirects: "unknown"}, {Path: "/", Method: "BAD METHOD"}, {Path: "%", ResolvePath: true}, {Path: "/", Headers: map[string]string{"Authorization": "bad\r\nvalue"}}, {Path: "/", Headers: map[string]string{"Bad Header": "value"}},
	} {
		if _, err := resolvePool("api", Pool{Backends: []Backend{{Address: "127.0.0.1:8080"}}, HealthCheck: policy}); err == nil {
			t.Fatal("invalid probe policy accepted")
		}
	}
}

func TestPassiveHealthRejectsInvalidHoldDown(t *testing.T) {
	for _, hold := range []string{"invalid", "0s", "-1s"} {
		if _, err := resolvePool("api", Pool{Backends: []Backend{{Address: "127.0.0.1:8080"}}, PassiveHealthCheck: PassiveHealthCheck{FailureWindow: "1s", MaxFailures: 1, HoldDown: hold}}); err == nil {
			t.Fatal("invalid passive hold-down accepted")
		}
	}
}

func TestAccessLogRejectsNegativeQueueSize(t *testing.T) {
	if _, err := resolveObservability(Observability{AccessLog: CommonLog(Stdout).Buffer(-1)}); err == nil {
		t.Fatal("negative log queue size accepted")
	}
}

func TestACMERejectsUnsupportedLeafKeyPolicy(t *testing.T) {
	for _, kind := range []string{"unknown", "RSA2048"} {
		if _, err := resolveAutoTLS(AutoTLS("api.example").Email("ops@example.test").Storage(t.TempDir()).KeyType(kind)); err == nil {
			t.Fatal("unsupported automatic key policy accepted")
		}
	}
}

func TestMiddlewareGroupsRejectInvalidChildPolicies(t *testing.T) {
	for _, mw := range []Middleware{InOrder(Timeout("invalid")), OnListeners(nil, Timeout("1s")), OnListeners([]string{"web"}, Timeout("invalid"))} {
		if _, err := resolveMiddleware(mw); err == nil {
			t.Fatal("invalid grouped policy accepted")
		}
	}
}
