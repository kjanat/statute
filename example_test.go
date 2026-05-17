package statute_test

import (
	"github.com/kjanat/statute"
)

// ExampleHTTP shows the minimal config: one HTTP listener proxying to an
// upstream pool. Compile and run as a standalone binary.
func ExampleHTTP() {
	statute.Main(statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":8080"),
		},
		Upstreams: statute.Upstreams{
			"api": statute.Pool{
				Backends: []statute.Backend{{Address: "127.0.0.1:9001"}},
			},
		},
		Routes: statute.Routes{
			statute.Match("/*").ProxyTo("api"),
		},
	})
}

// ExampleHTTPS_autoTLS shows AutoTLS with Let's Encrypt HTTP-01. The
// :80 listener serves the ACME challenge automatically because AutoTLS
// is configured elsewhere in the config; in production it should also
// redirect non-challenge traffic to HTTPS.
func ExampleHTTPS_autoTLS() {
	statute.Main(statute.Config{
		Listeners: statute.Listeners{
			statute.HTTP(":80").RedirectTo("https"),
			statute.HTTPS(":443",
				statute.AutoTLS("example.com").
					Email("ops@example.com").
					Storage("/var/lib/statute/certs"),
				statute.HTTP2(),
			),
		},
		Upstreams: statute.Upstreams{
			"api": statute.Pool{
				Backends: []statute.Backend{{Address: "10.0.0.1:8080"}},
			},
		},
		Routes: statute.Routes{
			statute.Match("/*").ProxyTo("api"),
		},
		Defaults: statute.Defaults{ReadHeaderTimeout: "5s"},
	})
}

// ExampleMatch_proxyTo shows a host-scoped proxy route.
func ExampleMatch_proxyTo() {
	_ = statute.Routes{
		statute.Match("/api/v1/*").Host("api.example.com").ProxyTo("api").
			With(
				statute.Timeout("30s"),
				statute.RateLimit("100/min").Per(statute.ClientIP),
			),
	}
}

// ExampleRateLimit shows the rate limiter keyed on client IP.
func ExampleRateLimit() {
	_ = statute.RateLimit("100/s").Per(statute.ClientIP)
}

// ExampleCORS shows a credentialed CORS policy bound to a specific origin.
func ExampleCORS() {
	_ = statute.CORS().
		Origins("https://app.example.com").
		Methods("GET", "POST", "PUT", "DELETE").
		Headers("Authorization", "Content-Type").
		Credentials().
		MaxAge("1h")
}

// ExampleSecurityHeaders shows the recommended baseline for a public-facing
// origin: HSTS, CSP, and the default conservative headers.
func ExampleSecurityHeaders() {
	_ = statute.SecurityHeaders().
		HSTS("365d").
		CSP("default-src 'self'; img-src 'self' data:")
}

// ExampleBasicAuth shows BasicAuth with bcrypt password hashes.
// Generate hashes with bcrypt.GenerateFromPassword (cost >= 10).
func ExampleBasicAuth() {
	users := map[string]string{
		"alice": "$2a$10$HwrzUQtDrRX0/09su3BahezCIqD.f4HjCkYD5b9w8gl4eUkPJzCyu", // password: "hunter2"
	}
	_ = statute.BasicAuth("Admin", users)
}

// ExampleAllowIPs shows the IP allow-list middleware. CIDRs are parsed
// once at startup; the runtime match is O(prefixes).
func ExampleAllowIPs() {
	_ = statute.AllowIPs("10.0.0.0/8", "192.168.0.0/16")
}
