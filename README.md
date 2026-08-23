# statute

> Config-as-code reverse proxy in Go. The binary _is_ the configuration.

[![ci](https://github.com/kjanat/statute/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/kjanat/statute/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/statute.kjanat.dev.svg)](https://pkg.go.dev/statute.kjanat.dev)
[![Release](https://img.shields.io/github/v/release/kjanat/statute?sort=semver)](https://github.com/kjanat/statute/releases)
[![License](https://img.shields.io/github/license/kjanat/statute)](LICENSE)

statute is a reverse proxy framework where your routing topology, TLS material, upstream pools, and middleware stack are expressed as Go values — type-checked, IDE-completed, and validated at startup. There is no runtime config file, no hot reload, no module loader. You write a `main.go`, you `go build`, you ship a single binary that boots, validates, and serves.

```go
package main

import "statute.kjanat.dev"

func main() {
    statute.Main(statute.Config{
        Listeners: statute.Listeners{
            statute.HTTP(":80").RedirectTo("https"),
            statute.HTTPS(":443",
                statute.AutoTLS("example.com").Email("ops@example.com").Storage("/var/lib/statute/certs"),
                statute.HTTP2(),
            ),
        },
        Upstreams: statute.Upstreams{
            "api": statute.Pool{
                Backends: []statute.Backend{{Address: "10.0.0.1:8080"}, {Address: "10.0.0.2:8080"}},
                Strategy: statute.LeastConnections,
                HealthCheck: statute.HealthCheck{Path: "/healthz", Interval: "10s"},
            },
        },
        Routes: statute.Routes{
            statute.Match("/*").ProxyTo("api").With(statute.Timeout("30s")),
        },
        Defaults:      statute.Defaults{ReadHeaderTimeout: "5s", WriteTimeout: "30s", IdleTimeout: "120s"},
        Observability: statute.Observability{AccessLog: statute.JSONLog(statute.Stdout), Metrics: statute.Prometheus(":9090", "/metrics")},
        Shutdown:      statute.Shutdown{GracePeriod: "30s", DrainListeners: true},
    })
}
```

## Why this and not nginx, Caddy, or Traefik

Choose statute when you want your reverse proxy configuration to be Go code that compiles. You get: type checking on every field, IDE completion as you write it, refactoring tools that work, the ability to put helper functions and conditional logic in your config without learning a templating language, and a single static binary that can't drift between "the config file on disk" and "what the daemon loaded".

You give up: hot reload, runtime configuration changes, plugin loading, web admin UIs, and a community ecosystem of off-the-shelf middleware. If those are deal-breakers, run Caddy or Traefik instead — they're better at being them.

One deliberate exception: the opt-in [Docker label discovery provider](docs/docker.md) routes containers by their labels, Traefik-style — including a compat mode for existing `traefik.*` labels. The topology (listeners, TLS, static routes, that discovery happens at all) stays compiled; only the label-derived routes follow containers as they come and go.

statute is designed for teams that already build and ship Go binaries, where adding "edit the config file" as an extra deployment path costs more than recompiling and re-rolling.

## Status

The framework is implemented and works end-to-end for the documented features. The HTTP-only, AutoTLS+HTTP-01, AutoTLS+DNS-01, and HTTP/3 paths all pass smoke tests. The API is design-stage and may shift in incompatible ways before a 1.0 release.

What's implemented:

- HTTP/1.1 and HTTP/2 listeners
- HTTP/3 (QUIC) via [quic-go](https://github.com/quic-go/quic-go), with `Alt-Svc` advertisement
- TLS termination: static certs, AutoTLS via [autocert](https://pkg.go.dev/golang.org/x/crypto/acme/autocert) (HTTP-01 + TLS-ALPN-01), AutoTLS via custom DNS-01 manager with Cloudflare API
- Upstream pools with round-robin, least-connections, IP-hash, and smooth weighted round-robin strategies
- Backup tier failover when all primary backends are unhealthy
- Active health checks with configurable thresholds
- Per-route middleware: timeout, rate limit (token bucket), retry (idempotent-method only, gRPC-aware), cache, gzip + brotli compression, ETag
- Static file serving
- WebSocket pass-through (default httputil.ReverseProxy behaviour)
- Structured JSON access logging with sampling
- Prometheus-format metrics
- OpenTelemetry tracing via OTLP/gRPC
- pprof endpoints on the metrics server
- Graceful shutdown with listener draining
- Cloudflare-aware mode: `BehindCloudflare()` flips ALPN to suppress TLS-ALPN-01 and trusts `CF-Connecting-IP`
- Docker label discovery: containers register routes and pools via `statute.*` labels, with a `traefik.*` label compat mode for drop-in migration
- `statute.Main` CLI wrapper with `-validate` and `-export` flags

## Install

```sh
go get statute.kjanat.dev
```

Requires Go 1.27 or newer.

## Concepts

### Config-as-code

The configuration is Go. Every field is a typed Go value. The `Config` struct is the entire surface of the framework.

```go
statute.Run(statute.Config{
    Listeners:     ...,
    Upstreams:     ...,
    Routes:        ...,
    Defaults:      ...,
    Observability: ...,
    Shutdown:      ...,
})
```

Helper functions (`HTTP`, `HTTPS`, `Match`, `RateLimit`, …) construct the values; struct literals fill in named fields. Durations are strings (`"10s"`, `"90s"`, `"1h"`) so the configuration reads like a config file rather than a Go program. Rate limits are strings (`"100/min"`). Sizes are strings (when added). Type checking still applies — invalid fields are caught at build time.

### Two layers: surface and resolved

statute has two type packages:

- `statute.kjanat.dev` is the **surface** API. It is what you write. It optimises for readability and ergonomic chaining.
- `statute.kjanat.dev/resolved` is the **resolved** schema. It is what the runtime executes against. It optimises for invariants: durations are `time.Duration`, upstream references are `*Pool` pointers, optional fields are filled with their canonical defaults, no string-encoded values remain.

Tooling (validators, exporters, dashboards) targets the resolved schema. End users target the surface API. They are connected by a single `Resolve(cfg) (*resolved.Config, error)` function.

### The pipeline: validate → resolve → run

Every config flows through three stages on startup:

- **Validate** rejects structural and semantic errors with path-style locations: `route[2] "/api/v1/*": unknown upstream "users"`.
- **Resolve** parses durations, dereferences upstream names, fills defaults, normalises addresses, and emits a `*resolved.Config`.
- **Run** opens listeners, builds per-backend reverse proxies, starts health checks, registers signal handlers, and serves.

You can stop after stage 2 with `statute.Resolve()` (for tooling) or `statute.Export()` (for snapshot and diff in CI).

## Feature reference

### Listeners

```go
statute.HTTP(":80").RedirectTo("https")
statute.HTTPS(":443",
    statute.AutoTLS("example.com").Email("ops@example.com").Storage("/var/lib/statute/certs"),
    statute.HTTP2(),
    statute.HTTP3(":443/udp"),
    statute.BehindCloudflare(),
)
statute.HTTPS(":443",
    statute.StaticTLS("/etc/ssl/cert.pem", "/etc/ssl/key.pem"),
    statute.HTTP2(),
)
```

`HTTP` and `HTTPS` declare a listener; `RedirectTo` turns a listener into a permanent redirect. The HTTPS variant takes options as variadic arguments — TLS material, HTTP/2, HTTP/3, Cloudflare-awareness — composed flat rather than nested.

When AutoTLS is configured anywhere in the config, the plain-HTTP listener automatically serves `/.well-known/acme-challenge/*` so HTTP-01 validation works without separate plumbing.

### Upstreams

```go
Upstreams: statute.Upstreams{
    "api": statute.Pool{
        Backends: []statute.Backend{
            {Address: "10.0.0.1:8080", Weight: 2},
            {Address: "10.0.0.2:8080", Weight: 1},
            {Address: "10.0.0.3:8080", Backup: true},
        },
        Strategy: statute.LeastConnections,
        HealthCheck: statute.HealthCheck{
            Path: "/healthz", Interval: "10s", Timeout: "2s",
            Healthy: 2, Unhealthy: 3,
        },
        Transport: statute.Transport{
            MaxIdleConnsPerHost: 32,
            IdleConnTimeout:     "90s",
        },
    },
}
```

Upstreams are a named map. Routes refer to pools by string key. A single pool can be reused across many routes.

Strategies:

- `RoundRobin` — even distribution.
- `LeastConnections` — pick the backend with fewest in-flight requests. Best when request durations vary.
- `IPHash` — consistent per-client routing for session affinity.
- `Weighted` — smooth weighted round-robin (Nginx-style).

The picker filters to healthy primary backends; when no primary is healthy, it falls through to the backup tier; when none of those are healthy either, it goes degraded and tries primaries anyway. Active health checks demote and promote backends in the background based on consecutive success/failure thresholds.

`Transport` tunes the HTTP transport reused across all backends in the pool. The default `MaxIdleConnsPerHost` (32) is a much better default for a proxy than Go's stdlib value (2); leave it alone unless you know why you're changing it.

A backend with an `https://` address gets its certificate verified against the system roots by default. `Transport` carries the pool's verification policy when that is not enough:

```go
"internal": statute.Pool{
    Backends: []statute.Backend{{Address: "https://10.0.0.10:8443"}},
    Transport: statute.Transport{
        ServerName:  "foo.internal.example",
        RootCAFiles: []string{"/etc/webserver/internal-ca.pem"},
    },
}
```

`ServerName` overrides the hostname verified (and sent as SNI) when backends are dialed by IP but present a certificate for a DNS name; `RootCAFiles` replaces the system roots with your internal CA. Reverse-proxy requests and active health-check probes share the same transport, so one policy covers both. `InsecureSkipVerify: true` is the explicit escape hatch that disables verification entirely — the lint rule `TLS002` warns whenever it is set.

### Routes and middleware

```go
Routes: statute.Routes{
    statute.Match("/api/v1/*").Host("api.example.com").ProxyTo("api").
        With(
            statute.RateLimit("100/min").Per(statute.ClientIP),
            statute.Retry(3, statute.OnStatus(502, 503, 504)),
            statute.Timeout("30s"),
        ),
    statute.Match("/static/*").Serve("./public").
        With(statute.Cache("1h"), statute.Compress(statute.Gzip, statute.Brotli), statute.ETag()),
}
```

Routes are matched in declaration order; the first match wins. Patterns support exact match (`/api`) and a trailing wildcard (`/api/*`). `Host` scopes a route to a specific Host header value. Catch-all `/*` should be last.

Each route declares exactly one action: a proxy (`ProxyTo("pool")`), a static-file serve (`Serve("./dir")`), or a redirect (`RedirectTo(target, status)`).

A wildcard static route strips its own prefix before looking in the directory, so `Match("/static/*").Serve("./public")` maps `/static/css/app.css` to `./public/css/app.css`. An exact static route keeps the whole path, so `Match("/robots.txt").Serve("./public")` serves `./public/robots.txt` — a single declared file rather than the directory root.

A redirect route answers without an upstream:

```go
statute.Match("/*").Host("old.example.com").RedirectTo(
    "https://new.example.com{request_uri}",
    http.StatusPermanentRedirect,
)
```

The status must be 301, 302, 303, 307, or 308. The target may be fixed, or preserve parts of the request through placeholders substituted at request time: `{request_uri}` (path and query as sent), `{path}`, `{query}` (raw, without the `?`), and `{host}` (port stripped). The target is validated when the config resolves — unknown placeholders, non-redirect statuses, and header-breaking bytes are startup errors — and substituted values come straight from net/http's request parsing, so placeholder-shaped text arriving in a request stays literal in the `Location` header. This is the route-level counterpart of the listener-level `RedirectTo("https")`, which redirects a whole listener to another scheme.

Middleware:

- **`Timeout(dur)`** — wraps the handler in `http.TimeoutHandler`. Returns 503 when exceeded.
- **`RateLimit(rate).Per(key)`** — token bucket per key. Rate format is `"N/unit"` where unit is `s`, `min`, `h`. Keys: `ClientIP` (default), `HostHeader`.
- **`Retry(max, OnStatus(...))`** — retries upstream calls up to `max` attempts when the response status matches one of the listed codes. Skips for non-idempotent methods (POST, PATCH), gRPC, SSE, WebSocket upgrades, and bodies > 1 MiB. Buffers smaller bodies to replay on retry.
- **`Cache(ttl)`** — in-process cache for 2xx GET/HEAD responses. Replace with a real LRU for high-cardinality deployments.
- **`Compress(Gzip, Brotli)`** — negotiates content encoding via `Accept-Encoding`. Brotli preferred when the client advertises both.
- **`ETag()`** — adds an SHA-256-based ETag to 200 responses; answers 304 on `If-None-Match` match.
- **`SetRequestHeader(name, value)`, `AddRequestHeader(name, value)`, `RemoveRequestHeader(name)`** — rewrite the request before it reaches the proxy or file handler. Names are canonicalised and values validated when the config resolves, so a bad header is a startup error rather than a malformed request.
- **`SetResponseHeader(name, value)`, `AddResponseHeader(name, value)`, `RemoveResponseHeader(name)`** — rewrite the response on the way out: `Set` overrides whatever the upstream sent, `Add` appends a value while keeping the upstream's, and `Remove` drops every value of the name. Applied when the response header is committed, so streaming and protocol upgrades are unaffected.

Header operations run in declaration order, request and response alike: the last `Set` of a name wins, and a `Remove` after a `Set` clears it. They apply at the route's edges rather than interleaved with the other middleware — request mutations before the chain runs, response mutations when the header commits — so a `Retry` underneath cannot apply them a second time per attempt.

On a proxy route, an explicit `X-Forwarded-For`, `-Host`, or `-Proto` declaration is reapplied after the proxy derives its own, so your value wins while the fields you leave alone keep the derived, unspoofable ones. Four names are rejected on requests because Go carries them outside the header map, where the mutation would do nothing: `Host`, `Content-Length`, `Transfer-Encoding`, and `Trailer`. Hop-by-hop headers can be set but the proxy strips them, as RFC 9110 requires, and a protocol-upgrade handshake is written straight to the hijacked connection, so response operations do not reach it.

### Docker discovery

```go
Docker: statute.Docker().
    Network("proxy").    // docker network to take container IPs from
    TraefikLabels().     // also honor existing traefik.* labels
    Refresh("30s"),      // optional periodic resync on top of the event stream
```

Containers opt in with labels and are routed as they start and stop:

```yaml
services:
  api:
    image: example/api
    labels:
      statute.enable: "true"
      statute.host: api.example.com
      statute.port: "8080"
      statute.healthcheck.path: /healthz
```

Replicas sharing a `statute.service` name pool together. With `TraefikLabels()`, containers already labeled for Traefik (`traefik.http.routers.<r>.rule` with `Host`/`Path`/`PathPrefix`, `loadbalancer.server.port`, …) work unmodified — the intended migration path for fleets moving off Traefik. Label-derived routes are matched only after every static route, and the discovery settings themselves are validated at startup like all other config. Full label reference and semantics in [docs/docker.md](docs/docker.md).

### Observability

```go
Observability: statute.Observability{
    AccessLog: statute.JSONLog(statute.Stdout).Sample(0.1),
    Metrics:   statute.Prometheus(":9090", "/metrics"),
    Tracing:   statute.OTLP("otel-collector:4317").ServiceName("edge").Insecure().Sample(0.05),
}
```

**Access log** — one JSON line per request. Fields: `ts`, `method`, `host`, `path`, `query`, `remote`, `user_agent`, `referer`, `status`, `duration_us`, `proto`, `forwarded_for`. `Sample(rate)` records a fraction of successful requests; errors (status ≥ 400) are always logged regardless.

**Metrics** — Prometheus exposition format on a separate listener. Counters for total requests, requests by status, and request duration. `pprof` is mounted under `/debug/pprof/*` on the same listener.

**Tracing** — OTLP/gRPC export to an OpenTelemetry collector. Spans use HTTP semantic conventions. W3C trace context is automatically propagated to upstream backends (the reverse proxy injects `traceparent` and `tracestate` headers). `Sample(rate)` is `TraceIDRatioBased` with parent-based sampling, so trace continuity is preserved across hops.

### TLS

```go
// Static cert from disk
statute.StaticTLS("/etc/ssl/cert.pem", "/etc/ssl/key.pem")

// Auto-provisioned via Let's Encrypt with HTTP-01 (default)
statute.AutoTLS("example.com", "api.example.com").
    Email("ops@example.com").
    Storage("/var/lib/statute/certs")

// Auto-provisioned via Let's Encrypt with DNS-01 + Cloudflare
// (required for wildcards and when port 80 is not reachable)
statute.AutoTLS("*.example.com", "example.com").
    Email("ops@example.com").
    Storage("/var/lib/statute/certs").
    CloudflareDNS01(token).Zone(zoneID)
```

AutoTLS persistence is **mandatory**. The `Storage` directory holds the ACME account key, issued certs, and renewal state; without it, every restart re-registers and re-issues, blowing through Let's Encrypt rate limits in days.

The DNS-01 path is implemented in-tree using `golang.org/x/crypto/acme` directly + a tiny Cloudflare DNS API client. It does not pull in lego or certmagic. It supports wildcards and works without a publicly-reachable port 80. See [docs/cloudflare.md](docs/cloudflare.md) for setup details.

### HTTP/3

```go
statute.HTTPS(":443",
    statute.AutoTLS(...),
    statute.HTTP2(),
    statute.HTTP3(":443/udp"),
)
```

When `HTTP3()` is on a listener, statute runs a quic-go HTTP/3 server alongside the HTTPS listener and adds an `Alt-Svc: h3=":443"; ma=86400` header on every HTTPS response so browsers upgrade subsequent requests. The same TLS material is shared between the HTTPS listener and the HTTP/3 server.

### CLI

The `statute.Main(cfg)` wrapper provides standard flags:

```
$ ./myproxy -validate          # parse and resolve, exit 0/1
$ ./myproxy -export             # write resolved config as JSON to stdout
$ ./myproxy                     # run the server
```

Use `Run(cfg)` directly if you want to handle flags yourself.

## Production checklist

The following are framework-enforced or strongly recommended:

- **`ReadHeaderTimeout`** is required. The default scaffold sets `5s`. Without it, statute is vulnerable to Slowloris.
- **Graceful shutdown** with `Shutdown.GracePeriod` and `DrainListeners: true`. Without it, every deploy drops in-flight requests.
- **Observability** — at minimum, access log + metrics. A proxy without observability is operationally blind.
- **Persistent AutoTLS storage**. Re-issuing on every restart will get the account rate-limited.
- **Health checks** on every pool. Without them, statute keeps sending traffic to dead backends.
- **At least two backends per pool**. A single-backend pool has no failover.
- **Tracing in production**. Set a sample rate (e.g. `0.05`) to control collector cost. Errors are still captured because parent-based sampling preserves traced error paths.
- **`BehindCloudflare()`** when fronted by Cloudflare. Without it, client IPs collapse to the CF edge node and rate limiting becomes useless.

## Examples

Examples are runnable Go programs in `examples/`:

- `examples/http-only` — HTTP-only proxy on `:8080`. Smallest runnable config.
- `examples/basic` — canonical AutoTLS + HTTP/2 + HTTP/3 setup.
- `examples/cloudflare` — fronted by Cloudflare with HTTP-01 (no API key).
- `examples/cloudflare-wildcard` — wildcard cert via Cloudflare DNS-01 + OTLP tracing.
- `examples/docker` — Docker label discovery with Traefik label compatibility.

Run any of them:

```sh
go run ./examples/http-only
go run ./examples/cloudflare-wildcard       # needs CLOUDFLARE_API_TOKEN
```

## Deeper docs

- [docs/cloudflare.md](docs/cloudflare.md) — running behind Cloudflare, HTTP-01 vs DNS-01, settings to enable on the Cloudflare side, failure modes.
- [docs/observability.md](docs/observability.md) — access log fields, metric names, span structure, sampling guidance.
- [docs/production.md](docs/production.md) — deployment patterns, ports, capabilities, the `setcap` trick for binding low ports as a non-root user.
- [docs/docker.md](docs/docker.md) — Docker label discovery: the `statute.*` label schema, the supported `traefik.*` compat subset, and the migration path from Traefik.

## Testing

```sh
go test ./...           # all unit tests
go vet ./...            # vet
make lint               # built-in + statute-specific linters
```

The race detector (`go test -race`) does not work on Raspberry Pi / older 64-bit Arm kernels with VMA range < 48; this is a TSAN limitation, not a code issue.

## License

See [LICENSE](LICENSE).

## Contributing

The API is design-stage. If you want to use statute in production, pin a specific commit, expect breakage on updates, and read the source — the tree is small (~4.3 kLOC across ~30 files) and self-contained.
