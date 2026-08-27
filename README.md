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
- Per-route middleware: timeout, rate limit (token bucket), retry (idempotent-method only, gRPC-aware), cache, gzip + brotli compression, ETag, header rewriting, and path rewriting (strip prefix, add prefix, replace path, regex rewrite)
- Static file serving
- In-process handler routes: mount any `http.Handler` from your own binary as a route action
- WebSocket pass-through (default httputil.ReverseProxy behaviour)
- Structured JSON access logging with sampling
- Prometheus-format metrics
- OpenTelemetry tracing via OTLP/gRPC
- pprof endpoints on the metrics server
- Dedicated process health endpoint with liveness and readiness paths
- Graceful shutdown with listener draining
- Cloudflare-aware mode: `BehindCloudflare()` flips ALPN to suppress TLS-ALPN-01 and trusts `CF-Connecting-IP`
- Docker label discovery: containers register routes and pools via `statute.*` labels, with a `traefik.*` label compat mode for drop-in migration
- `statute.Main` CLI wrapper with `-validate`, `-export`, `-graph`, and `-lint` flags

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

`TrustedProxy` scopes forwarded-header trust to the direct peer instead of the whole listener:

```go
statute.HTTPS(":443",
    statute.TrustedProxy("203.0.113.0/24").ClientIPHeader("CF-Connecting-IP"),
)
```

When a connection's direct peer falls inside one of the ranges, the client IP — as seen by the access log, rate limiting, `AllowIPs`/`DenyIPs`, and `IPHash` — comes from the configured header (`X-Forwarded-For` by default; of a multi-valued header the last value counts, the one the trusted proxy itself observed). Any other peer is its own client and its forwarded headers are ignored, so proxy-fronted and direct-origin hostnames can share a listener without the headers becoming spoofable. It applies identically over HTTP/1.1, HTTP/2, and HTTP/3 — every transport shares the listener's middleware chain. Behind Cloudflare, use it _alongside_ `BehindCloudflare()` rather than instead of it: the trust policy takes precedence for client IPs, while `BehindCloudflare()` keeps suppressing the TLS-ALPN-01 challenge that Cloudflare's edge cannot forward.

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
            Host:     "probe.internal.example", // optional probe Host override
            Statuses: []int{200, 204},          // optional; default accepts 200-399
        },
        PassiveHealthCheck: statute.PassiveHealthCheck{
            FailureWindow: "30s", MaxFailures: 5,
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

`UpstreamHost` selects the `Host` header backends receive. The default forwards the client's own `Host` unchanged; `statute.TargetHost` sends each backend its own host (what a plain client dialing it directly would send — hostname-sensitive upstreams that reject the client's `Host` usually want this); `statute.HostValue("api.internal.example")` sends a fixed name. The policy also covers active health-check probes: an explicit value is carried on every probe, and the other policies leave probes on each backend's own host, since a probe has no client `Host` to preserve. `HealthCheck.Host`, when set, overrides the probe `Host` alone — it takes precedence over the derivation above, is validated exactly like `HostValue`, and never touches proxied requests, which keep following `UpstreamHost`.

`HealthCheck.Statuses` narrows (or widens) what a probe accepts as healthy: leave it empty for the default 200–399 range, or list the exact statuses (`[]int{200, 204}` demotes a backend that answers 301). Each entry must be within 100–599. Setting `Host` or `Statuses` stops probes from following redirects, so the health endpoint's own status is what is judged; default probes follow redirects and judge the final response, as before.

The picker filters to healthy primary backends; when no primary is healthy, it falls through to the backup tier; when none of those are healthy either, it goes degraded and tries primaries anyway. Active health checks demote and promote backends in the background based on consecutive success/failure thresholds.

`PassiveHealthCheck` demotes backends from real traffic instead of probes: a backend that accumulates `MaxFailures` failed attempts — a transport error or a 5xx response — inside the sliding `FailureWindow` is excluded from selection. A request canceled by its own client does not count — a client abort is not a backend fault, and counting it would let any client demote backends pool-wide — while a deadline that expires waiting on the backend does. Failures are windowed, not consecutive: a success in between neither clears nor extends the window, and the backend is re-admitted only as failures age out. Counting is per backend attempt, so under a `Retry` middleware every attempt counts against the backend that served it even when the client-visible request succeeds elsewhere. Passive and active health are independent — either works alone, and an active probe success never clears a passive window. Degraded mode is unchanged: when every backend in a pool is demoted — in particular, a single-backend pool whose only backend is passively demoted — the pool keeps sending traffic to its primaries rather than manufacturing 503s, so passive demotion never stops traffic on its own. Both fields must be set together; the zero value disables passive health.

`Transport` carries pool-wide upstream transport and proxy-response policy, reused across all backends in the pool. The default `MaxIdleConnsPerHost` (32) is a much better default for a proxy than Go's stdlib value (2); leave it alone unless you know why you're changing it.

`Transport.FlushInterval` (e.g. `"100ms"`) makes the reverse proxy flush buffered response bytes to the client at that interval. It is pool policy: every route proxying to the pool shares the one interval. The default `0` keeps Go's `httputil.ReverseProxy` default — no periodic flushing — and responses the proxy detects as streaming (unknown Content-Length, `text/event-stream`) are flushed immediately regardless of this setting, so SSE works out of the box either way. Only non-negative durations are accepted; there is no equivalent of Traefik's `-1` immediate-flush sentinel, so a response the proxy does not detect as streaming (for example long polling with a known Content-Length) needs a small positive interval instead.

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

Each route declares exactly one action: a proxy (`ProxyTo("pool")`), a static-file serve (`Serve("./dir")`), a redirect (`RedirectTo(target, status)`), or an in-process handler (`Handle(h)`).

`ClientIPs("10.0.0.0/8", ...)` makes client CIDRs part of route _selection_: a request from outside the ranges falls through to the next route, where `AllowIPs` middleware would answer 403 and stop. That enables conditional policies — a trusted-network route first, an authenticated fallback beneath it:

```go
statute.Match("/*").Host("admin.example.com").ClientIPs("10.0.0.0/8").ProxyTo("admin"),
statute.Match("/*").Host("admin.example.com").ProxyTo("admin").With(statute.BasicAuth("admin", users)),
```

The matcher keys on the same verified client-IP resolution as rate limiting and the IP lists, so the listener's `TrustedProxy` policy decides whether forwarded headers count; a client whose address cannot be parsed never matches a constrained route.

A wildcard static route strips its own prefix before looking in the directory, so `Match("/static/*").Serve("./public")` maps `/static/css/app.css` to `./public/css/app.css`. An exact static route keeps the whole path, so `Match("/robots.txt").Serve("./public")` serves `./public/robots.txt` — a single declared file rather than the directory root.

A redirect route answers without an upstream:

```go
statute.Match("/*").Host("old.example.com").RedirectTo(
    "https://new.example.com{request_uri}",
    http.StatusPermanentRedirect,
)
```

The status must be 301, 302, 303, 307, or 308. The target may be fixed, or preserve parts of the request through placeholders substituted at request time: `{request_uri}` (path and query as sent), `{path}`, `{query}` (raw, without the `?`), and `{host}` (port stripped). The target is validated when the config resolves — unknown placeholders, non-redirect statuses, and header-breaking bytes are startup errors — and substituted values come straight from net/http's request parsing, so placeholder-shaped text arriving in a request stays literal in the `Location` header. A `Location` that would come out protocol-relative (a leading `//` or `/\`, whether from a `//evil.com` request path or a `StripPrefix` that exposes one) is collapsed to a single leading slash, so a client cannot steer a relative redirect off-site. This is the route-level counterpart of the listener-level `RedirectTo("https")`, which redirects a whole listener to another scheme.

A handler route answers with an `http.Handler` from the same binary — a health endpoint, a debug page, a small API living beside the proxy:

```go
statute.Match("/healthz").Host("foo.example.com").Handle(http.HandlerFunc(
    func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
))
```

The handler composes with route middleware like any other action and drains through graceful shutdown like proxied requests. It receives the request path unstripped — the wildcard prefix stripping above is `Serve`-specific — though the hoisted header operations and path rewrites apply to it as usual. Under a `Retry` the handler may run once per attempt (idempotent methods only, as `Retry` enforces), and it is invoked concurrently, so it must be safe for concurrent use. Because a handler is opaque code, the JSON export carries only a `HandlerRoute` marker for it, the DOT graph renders the route as an edge-less route node, and Docker labels cannot reference or construct one — handlers exist solely in your compiled configuration.

A `Fallback` handler answers the requests no route matched at all:

```go
statute.Config{
    Docker: statute.Docker().TraefikLabels(),
    Fallback: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        http.Error(w, "not found", http.StatusNotFound)
    }),
}
```

It is the router's terminal stage, so the precedence chain is static routes (declaration order) → the current Docker generation → that generation's tombstones → the fallback. A hostless `Match("/*")` route cannot express that: static routes are consulted before Docker's dynamic routes, so it would shadow every discovered one — declare one anyway and the fallback, along with every Docker-discovered route, becomes permanently unreachable, which `-lint` reports as `FB001`. Nil leaves the terminal 404 in place, and a non-nil interface wrapping a nil handler is a startup error rather than a silent 404.

The fallback is not a route: it has no matcher, and it carries no route middleware, which is route-scoped. Listener wrapping still covers it — access log, metrics, tracing, and the `TrustedProxy` policy wrap the whole router, so a fallback response is logged and counted like any other — and everything wrapping the router answers ahead of it: a pending ACME HTTP-01 challenge response always does, and an automatic (autocert) source absorbs the whole `/.well-known/acme-challenge/` namespace, answering unknown tokens with its own 404. A pinned `HTTP01()` source only answers its pending tokens, so unknown paths under that prefix pass through to the content router and are routed normally: a static route or a Docker-discovered one may still match them, and they reach the fallback only if those tables and the tombstones miss too. The whole prefix passes through the same way when no ACME source is configured. A redirect-only listener never reaches the router at all. Like a handler route it is opaque code, so the JSON export carries only a `HasFallback` marker, the DOT graph renders it as one node reached from the content listeners, and Docker labels cannot name one.

A Docker registration whose routes were dropped does not reach the fallback — a Traefik router or a container's native `statute.*` labels alike. An unreadable rule, a reference to a middleware chain that was never registered, a pool that cannot be built, a container with no reachable address: each discards a routing decision the labels declared, and that traffic used to end in the terminal 404. With a fallback configured it would instead fall through into operator code that does not know the router asked for a policy statute could not supply — typically a catch-all proxy to the very same container, serving unauthenticated exactly what the dropped auth middleware was protecting. So the generation keeps a **tombstone** for it: a matcher with no upstream and no middleware that answers the same 404, consulted after the discovered routes and before the fallback. A deployment with no fallback configured sees no change at all.

The tombstone covers everything the dropped router could have matched, never less. Constraints statute cannot represent are dropped, which only widens the refusal: ``Host(`admin.example.com`) && ClientIP(`10.0.0.0/8`)`` refuses all of `admin.example.com`, and ``PathPrefix(`/private`) && ClientIP(…)`` refuses `/private/*` on every host. A matcher statute cannot read at all is dropped the same way, since a rule matches every one of its conjuncts: ``Host(`a.example.com`) && Path()`` still refuses only `a.example.com`. A disjunction is the opposite — a union of its branches — so ``Host(`a.example.com`) || ClientIP(…)`` cannot be narrowed at all. A rule with nothing left bounding it — a bare `ClientIP`, a `HostRegexp`, a lone `Path()`, a typo — leaves the global tombstone, which refuses every unmatched request in that generation and disables the fallback until the labels are fixed. Every one of these is logged with the envelope now refusing, alongside whatever the stage that dropped the routes knew — the container and router for a label-stage drop, the service for a pool-stage one — so a 404 traces back to the labels that caused it, and the generation's standing refusal is announced again whenever it changes. A router with no rule matches nothing in Traefik either, so it leaves no tombstone, and neither does a container that opted out with `traefik.enable=false`, nor one `ExposedByDefault()` registered that carries no `statute.*` label of its own. An `enable` value that is neither true nor false is not an opt-out: it cannot be read, so the routes are dropped and the envelope stands.

Middleware:

- **`Timeout(dur)`** — wraps the handler in `http.TimeoutHandler`. Returns 503 when exceeded.
- **`RateLimit(rate).Per(key)`** — token bucket per key. Rate format is `"N/unit"` where unit is `s`, `min`, `h`. Keys: `ClientIP` (default), `HostHeader`.
- **`Retry(max, OnStatus(...))`** — retries upstream calls up to `max` attempts when the response status matches one of the listed codes. Skips for non-idempotent methods (POST, PATCH), gRPC, SSE, WebSocket upgrades, and bodies > 1 MiB. Buffers smaller bodies to replay on retry.
- **`Cache(ttl)`** — in-process cache for 2xx GET/HEAD responses. Replace with a real LRU for high-cardinality deployments.
- **`Compress(Gzip, Brotli)`** — negotiates content encoding via `Accept-Encoding`. Brotli preferred when the client advertises both.
- **`ETag()`** — adds an SHA-256-based ETag to 200 responses; answers 304 on `If-None-Match` match.
- **`SetRequestHeader(name, value)`, `AddRequestHeader(name, value)`, `RemoveRequestHeader(name)`** — rewrite the request before it reaches the proxy or file handler. Names are canonicalised and values validated when the config resolves, so a bad header is a startup error rather than a malformed request.
- **`SetResponseHeader(name, value)`, `AddResponseHeader(name, value)`, `RemoveResponseHeader(name)`** — rewrite the response on the way out: `Set` overrides whatever the upstream sent, `Add` appends a value while keeping the upstream's, and `Remove` drops every value of the name. Applied when the response header is committed, so streaming and protocol upgrades are unaffected.
- **`StripPrefix(prefix)`** — removes `prefix` from the request path before it is proxied or served, so `/api/users` reaches the upstream as `/users`. Stripping the whole path leaves `/`; a path the prefix does not cover passes through untouched rather than 404ing.
- **`AddPrefix(prefix)`** — prepends `prefix` to the request path, the inverse: `/users` reaches an upstream mounted under `/v2` as `/v2/users`.
- **`ReplacePath(path)`** — discards the request path and substitutes a fixed one. An optional `?query` suffix explicitly replaces the query string (`ReplacePath("/healthz?")` clears it) — the one place in the set where the query changes.
- **`RewritePath(pattern, replacement)`** — rewrites the path through an RE2 regexp with `$1`-style capture references, following `regexp.ReplaceAllString`: `RewritePath("^/api/v(\\d+)/(.*)$", "/v$1/$2")` turns `/api/v1/users` into `/v1/users`. A result that loses its leading slash gets one back; one that empties the path becomes `/`.

Header operations run in declaration order, request and response alike: the last `Set` of a name wins, and a `Remove` after a `Set` clears it. They apply at the route's edges rather than interleaved with the other middleware — request mutations before the chain runs, response mutations when the header commits — so a `Retry` underneath cannot apply them a second time per attempt.

On a proxy route, an explicit `X-Forwarded-For`, `-Host`, or `-Proto` declaration is reapplied after the proxy derives its own, so your value wins while the fields you leave alone keep the derived, unspoofable ones. Four names are rejected on requests because Go carries them outside the header map, where the mutation would do nothing: `Host`, `Content-Length`, `Transfer-Encoding`, and `Trailer`. Hop-by-hop headers can be set but the proxy strips them, as RFC 9110 requires, and a protocol-upgrade handshake is written straight to the hijacked connection, so response operations do not reach it.

Path rewrites are normalised when the config resolves — trailing slashes are trimmed, so `StripPrefix("/api/")` and `StripPrefix("/api")` are one declaration and the resolved export shows `/api` — and an empty, slash-only, `?`/`#`/`%`-carrying, or doubled-leading-slash (`//`, `/\`) prefix, an unrooted, protocol-relative, or badly escaped `ReplacePath` target (query included: a space or control byte there is a startup error, not a per-request 400), and an uncompilable `RewritePath` pattern are all startup errors. A `StripPrefix`/`AddPrefix` prefix is a decoded literal; `ReplacePath` takes an escaped target, so it is the primitive for a path that must carry an escaped `%2F`. The query string is carried through untouched by every one of them except `ReplacePath`'s explicit `?query` form.

Like the header operations, path rewrites apply at the route's edge — before every other middleware, in declaration order among themselves — rather than interleaved with the chain, so where a rewrite sits in the `.With(...)` list does not change the result: the cache key, the remaining middleware, and the upstream all see the same rewritten path, and a `Retry` beneath never observes a half-rewritten one. Route matching and the access log see the original path. (A rewrite works on a clone of the request rather than mutating it, which is what keeps it exactly-once when a retry re-serves the same request.)

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
    Health:    statute.Health(":8081", "/healthz"),
}
```

**Access log** — one JSON line per request. Fields: `ts`, `method`, `host`, `path`, `query`, `remote`, `user_agent`, `referer`, `status`, `duration_us`, `proto`, `forwarded_for`. `Sample(rate)` records a fraction of successful requests; errors (status ≥ 400) are always logged regardless.

**Metrics** — Prometheus exposition format on a separate listener. Counters for total requests, requests by status, and request duration. `pprof` is mounted under `/debug/pprof/*` on the same listener.

**Tracing** — OTLP/gRPC export to an OpenTelemetry collector. Spans use HTTP semantic conventions. W3C trace context is automatically propagated to upstream backends (the reverse proxy injects `traceparent` and `tracestate` headers). `Sample(rate)` is `TraceIDRatioBased` with parent-based sampling, so trace continuity is preserved across hops.

**Health** — a dedicated process health listener for supervisor probes that brackets the application's availability: it answers first in `Start` and closes last in `Shutdown`. Liveness at the configured path (default `/healthz`) answers `200 ok` for the whole time the process runs; readiness at the path plus `/ready` answers `503 not ready` throughout startup (cert managers, initial Docker sync), `200 ok` once startup commits, and `503 not ready` again for the entire shutdown grace period while content drains. Matching is exact — only those two paths answer, everything else 404s, and the path must start with `/`, not be `/`, and not end with `/`. Nothing else is mounted, neither metrics nor pprof. Details in [docs/observability.md](docs/observability.md).

### TLS

```go
// Static cert from disk
statute.StaticTLS("/etc/ssl/cert.pem", "/etc/ssl/key.pem")

// Auto-provisioned via Let's Encrypt, automatic challenge policy (default):
// TLS-ALPN-01 is attempted first where the listener advertises acme-tls/1,
// with HTTP-01 as the fallback.
statute.AutoTLS("example.com", "api.example.com").
    Email("ops@example.com").
    Storage("/var/lib/statute/certs")

// Pinned to HTTP-01: issues through the in-tree ACME manager, which never
// attempts TLS-ALPN-01. Requires a plain HTTP listener in the config.
statute.AutoTLS("example.com", "api.example.com").
    Email("ops@example.com").
    Storage("/var/lib/statute/certs").
    HTTP01()

// Against a different ACME directory: Let's Encrypt staging while testing
// rate-limit-sensitive rollouts, or a private CA (step-ca, Pebble). Empty
// (the default) is Let's Encrypt production. It must be an absolute HTTPS
// URL — plain HTTP is a resolve error, since ACME account and order
// material must never travel unencrypted. Sources sharing an ACME account
// must agree on the directory.
statute.AutoTLS("example.com").
    Email("ops@example.com").
    Storage("/var/lib/statute/certs").
    Directory("https://acme-staging-v02.api.letsencrypt.org/directory")

// Auto-provisioned via Let's Encrypt with DNS-01 + Cloudflare
// (required for wildcards and when port 80 is not reachable)
statute.AutoTLS("*.example.com", "example.com").
    Email("ops@example.com").
    Storage("/var/lib/statute/certs").
    CloudflareDNS01(token).Zone(zoneID).
    Propagation(statute.DNSPropagation{
        Delay:     "30s",
        Resolvers: []string{"192.0.2.53:53", "198.51.100.53:53"},
    })
```

AutoTLS persistence is **mandatory**. The `Storage` directory holds the ACME account key, issued certs, and renewal state; without it, every restart re-registers and re-issues, blowing through Let's Encrypt rate limits in days.

**Directory overrides are linted, not policed.** Resolve validates the shape of `Directory` (absolute HTTPS, and agreement between sources sharing one ACME account) but has no way to judge whether the CA you named is the one you meant to ship. Two lint warnings close that gap. `TLS005` fires when the directory is Let's Encrypt staging: it issues certificates that are not publicly trusted, so a config that reaches production with the staging URL still in it serves certificates the default trust stores of browsers and API clients reject. `TLS006` fires on any other non-Let's-Encrypt directory and names it in the message, so a step-ca or Pebble URL surfaces on a pre-deploy checklist instead of at the first handshake. Both are warnings rather than errors, because a non-default directory is a legitimate operator decision that resolve has already accepted; treat one in a production config as something to confirm deliberately, not as noise to silence.

The DNS-01 path is implemented in-tree using `golang.org/x/crypto/acme` directly + a tiny Cloudflare DNS API client. It does not pull in lego or certmagic. It supports wildcards and works without a publicly-reachable port 80. See [docs/cloudflare.md](docs/cloudflare.md) for setup details.

**Propagation control.** After publishing the challenge TXT record, a DNS-01 source waits a fixed 15 seconds before asking the CA to validate. `Propagation` replaces that with a policy of your own, in either or both of two modes: a `Delay` you choose (up to 10 minutes), and a list of `Resolvers` — `host:port` DNS servers that must **all** return the expected TXT value before validation is requested. Polling runs from the end of the delay on a `Timeout` (default `"2m"`, max 10 minutes) and `Interval` (default `"5s"`, min 100ms), with the first round immediate and a resolver dropped from the set once it has answered correctly. Lookup errors are not failures — an unpropagated record is indistinguishable from `NXDOMAIN` — but the deadline is: it fails issuance with an error naming the record and the resolvers still short, without asking the CA to validate, so a slow zone costs a retry instead of one of the five validation failures Let's Encrypt allows per hostname per hour. The propagation budget is added to the per-order timeout so a long policy is not cancelled mid-wait. Resolve rejects the dead and contradictory shapes: `Timeout` or `Interval` without `Resolvers`, a policy that waits for nothing (`Delay: "0s"` alone counts), a delay or timeout over 10 minutes, an explicit interval below 100ms or above the timeout, a resolver without a valid `host:port`, a duplicate resolver in any spelling, and `Propagation` on a source with no `CloudflareDNS01`. The normalised policy is part of the resolved schema and the `-export` output.

**SNI-scoped sources.** One listener takes any number of TLS sources, mixed freely, and picks one per handshake by SNI hostname — an exact name wins over a wildcard pattern (which covers exactly one extra label), and a hostless `StaticTLS` is the fallback for unmatched names and clients that send no SNI. Mixed public/direct names, DNS-01 wildcards, and externally provisioned certificates can therefore share port 443:

```go
statute.HTTPS(":443",
    statute.AutoTLS("foo.example.com").HTTP01().
        Email("ops@example.com").Storage("/var/lib/statute/certs"),
    statute.AutoTLS("*.bar.example").CloudflareDNS01(token).
        Email("ops@example.com").Storage("/var/lib/statute/certs"),
    statute.StaticTLSFor("baz.example.net", certFile, keyFile),
    statute.StaticTLS(defaultCert, defaultKey), // fallback for everything else
)
```

`HTTP01()` pins a source to the HTTP-01 challenge: instead of the shared autocert manager — whose challenge preference is hard-coded to attempt TLS-ALPN-01 first — the source issues through statute's in-tree ACME manager (the same machinery as DNS-01), which only ever attempts HTTP-01 and never advertises `acme-tls/1`. The default policy without it stays automatic: TLS-ALPN-01 where advertisable, HTTP-01 otherwise. Calling it together with `CloudflareDNS01` on one source is a resolve error, as is the same name claimed by two sources, a second hostless fallback, and a pinned source in a config with no plain HTTP listener to serve its challenge tokens. All names — AutoTLS domains, static hosts, and incoming SNI — are canonicalised the same way (case, trailing dots, IDNA A-label), so `foo.example.com.` and `FOO.example.com` are one name to both routing and duplicate detection; ACME domains must additionally survive the strict IDNA lookup, since a name autocert's host policy would drop can never be issued. Across listeners, resolve also rejects two pinned sources that would persist one domain to the same `<storage>/<challenge>/` path (their managers would race to rename over one stored key pair; the same domain on two automatic listeners stays legal — those feed one shared autocert manager) and pinned sources that share an ACME account directory but disagree on `Email`. The shapes it deliberately allows — one domain issued by two pinned sources with distinct storage roots or challenge kinds, or pinned on one source and automatic on another — are reported by the lint rule `TLS003`: each manager orders and renews that domain on its own, spending Let's Encrypt's duplicate-certificate limit once per manager.

**Downstream TLS policy.** `TLSPolicy` sets the protocol version window and the permitted TLS 1.2 cipher suites for one HTTPS listener:

```go
statute.HTTPS(":443",
    statute.AutoTLS("foo.example.test").
        Email("ops@example.test").Storage("/var/lib/statute/acme"),
    statute.TLSPolicy{
        MinVersion: statute.TLS12,
        MaxVersion: statute.TLS13,
        CipherSuites: []statute.CipherSuite{
            statute.TLSECDHEECDSAWithAES128GCM,
            statute.TLSECDHERSAWithAES128GCM,
        },
    },
)
```

Without a policy a listener keeps the defaults it has always had: minimum TLS 1.2, no upper bound, and Go's own TLS 1.2 suite selection. `MinVersion` and `MaxVersion` accept `statute.TLS12` or `statute.TLS13`, and resolve rejects every other value — there are deliberately no TLS 1.0/1.1 constants, so no option lowers the floor. `CipherSuites` governs TLS 1.2 handshakes only: TLS 1.3 suites are fixed by the protocol and `crypto/tls` accepts no override for them, so listing suites alongside `MinVersion: statute.TLS13` is a resolve error rather than a setting that quietly does nothing. Declaration order is preserved in the resolved schema, but `crypto/tls` ranks the listed suites itself when negotiating. All ten `TLSECDHE*` constants are ECDHE; six are AEAD (AES-GCM, ChaCha20-Poly1305) and four are CBC, which HTTP/2 never negotiates (RFC 9113).

Resolve also rejects the combinations that could never serve. A suite list must include `TLSECDHEECDSAWithAES128GCM` or `TLSECDHERSAWithAES128GCM`: `net/http` checks every TLS 1.2 suite override for one of those two before it will serve TLS at all — HTTP/2 enabled or not — so a listener without them would bind and then never answer a handshake. `HTTP3()` under `MaxVersion: statute.TLS12` is an error, since QUIC is defined over TLS 1.3 alone; so is an RSA-only suite list under a 1.2 cap on a listener with a pinned HTTP-01/DNS-01 source — the in-tree ACME manager always generates ECDSA P-256 keys, an `ECDHE_RSA` suite needs an RSA certificate to sign the key exchange, and the SNI router never falls back past the source that matched the name, so no other certificate on the listener (static fallback included) can rescue those domains. The same policy over automatic sources is lint rule `TLS004` (warning) rather than an error: autocert picks each leaf's key type from the ClientHello — ECDSA P-256 unless the client advertises no ECDSA support — so only legacy RSA-only clients connect, and where `acme-tls/1` is advertised the ECDSA challenge certificate makes TLS-ALPN-01 validation fail too. An unsupported version, an inverted window, an unknown suite, a suite listed twice, a second policy on one listener, and a policy on a redirect-only listener (which terminates no TLS) are errors too. One policy covers the whole listener — every TLS source on it and the HTTP/3 server sharing its certificates. The resolved schema and `-export` output carry it normalised: `"1.2"`/`"1.3"` for the versions, IANA names in declaration order for the suites.

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

```sh
./myproxy -validate  # parse and resolve, exit 0/1
./myproxy -export    # write resolved config as JSON to stdout
./myproxy -graph     # write topology as Graphviz DOT to stdout
./myproxy -lint      # audit the resolved config, exit 0/1
./myproxy            # run the server
```

The four operation flags are mutually exclusive; passing two exits 2.

Use `Run(cfg)` directly if you want to handle flags yourself.

**Lint rules.** `-lint` resolves the configuration and runs the production-readiness rule set over the resolved model, printing one line per finding as `[severity] CODE: message (at path)`. Severity decides the exit status: one `error` finding exits 1, warnings are reported and still exit 0. Warnings are judgement calls a deliberate operator may keep; errors are shapes that are unsafe or cannot serve.

The table is the index. The paragraph beside each feature above stays the explanation, and the config path column is the shape of the `Path` on the finding, with `i`/`j` standing for the offending element's index and `name` for a pool's key.

<!-- lint-rules:start -->

| Code      | Severity | Config path                                      | Fires when                                                                                                                       |
| --------- | -------- | ------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------- |
| `AUTH001` | error    | `routes[i].middleware[j]`                        | A route uses `BasicAuth` and no HTTPS listener serves content, so credentials would travel in clear-text.                        |
| `FB001`   | warning  | `routes[i]`                                      | A hostless catch-all route (`/*`, no host, no `ClientIPs`) is declared alongside Docker discovery or a fallback, shadowing both. |
| `HC001`   | warning  | `upstreams[name]`                                | A pool has neither active nor passive health checks, so dead backends keep receiving traffic.                                    |
| `LB001`   | warning  | `upstreams[name]`                                | A pool has exactly one primary (non-backup) backend, so it has no failover and no load distribution.                             |
| `OBS001`  | warning  | `observability.metrics`                          | The metrics endpoint is disabled, leaving no Prometheus visibility.                                                              |
| `OBS002`  | warning  | `observability.access_log`                       | The access log is disabled, leaving no per-request audit trail.                                                                  |
| `RHT001`  | error    | `defaults.read_header_timeout`                   | `ReadHeaderTimeout` resolves to zero, which is the Slowloris exposure.                                                           |
| `RL001`   | warning  | `routes[i].middleware[j]`                        | A `RateLimit` resolves below 1 request per second, low enough to block legitimate clients.                                       |
| `SHUT001` | warning  | `shutdown.grace_period`                          | `Shutdown.GracePeriod` is under 5s, so deploys may cut off in-flight requests.                                                   |
| `TLS001`  | error    | `listeners[i].auto_tls[j].storage`               | AutoTLS storage is under `/tmp`: wiped on reboot, then re-issued until the account hits the rate limit.                          |
| `TLS002`  | warning  | `upstreams[name].transport.insecure_skip_verify` | Backend certificate verification is off, so anyone on the path to the pool can impersonate it.                                   |
| `TLS003`  | warning  | `listeners[i].auto_tls[j]`                       | One domain is issued by more than one ACME manager, each spending the duplicate-certificate limit on its own renewals.           |
| `TLS004`  | warning  | `listeners[i].tls_policy`                        | A TLS 1.2 cap with RSA-only suites governs a listener with an automatic ACME source, whose leaves are ECDSA.                     |
| `TLS005`  | warning  | `listeners[i].auto_tls[j].directory`             | The ACME directory is Let's Encrypt staging, which issues certificates no public trust store accepts.                            |
| `TLS006`  | warning  | `listeners[i].auto_tls[j].directory`             | The ACME directory is some other non-Let's-Encrypt endpoint, named in the message so a private CA surfaces before deploy.        |

<!-- lint-rules:end -->

`TestLintRuleTableDocumentsEveryRule` holds this table to the rule set: a code that fires in `lint.go` without a row here, a row naming a code no rule emits, or a severity that disagrees with the code fails the test.

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
go test ./...  # all unit tests
go vet  ./...  # vet
make lint      # built-in + statute-specific linters
```

The race detector (`go test -race`) does not work on Raspberry Pi / older 64-bit Arm kernels with VMA range < 48; this is a TSAN limitation, not a code issue. `make test-race` probes for it and skips with a note instead of failing, so the target is safe to run anywhere; CI runs the detector on x86. A probe failure that is not ThreadSanitizer still fails the target.

## License

See [LICENSE](LICENSE).

## Contributing

The API is design-stage. If you want to use statute in production, pin a specific commit, expect breakage on updates, and read the source — the tree is small (~4.3 kLOC across ~30 files) and self-contained.
