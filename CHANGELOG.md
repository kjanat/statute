# Changelog

All notable changes to statute are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). statute is pre-1.0; minor releases may include breaking surface-API changes.

## [Unreleased]

## [v0.2.0] — 2026-05-17

### Added

#### Polish foundation

- GitHub Actions CI (`vet`, `golangci-lint`, `go test -race -coverprofile`, fuzz with 30s budget, examples build).
- GitHub Actions release workflow that fires on `v*` tags, extracts the matching `CHANGELOG.md` section, calls `gh release create --notes-file`, and warms `proxy.golang.org` for pkg.go.dev.
- Explicit `.golangci.yml` with an opinionated set: `errcheck`, `govet`, `ineffassign`, `staticcheck`, `unused`, `gocritic`, `revive`, `gofumpt`, `misspell`, `unparam`, `prealloc`, `bodyclose`, `nilerr`, `errorlint`, `unconvert`.
- Dependabot config for `gomod` (with grouped OTel + `x/crypto` updates) and `github-actions`.
- Repo files: `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CODEOWNERS`, `FUNDING.yml`, `.editorconfig`, issue templates (bug + feature, plus a blank-issue-disabled redirect), PR template with new-middleware checklist.
- README badge row: build status, godoc, Go report card, license, latest release.

#### Quality push

- Coverage target raised from 24% to >60%: new tests for `server.go`, `cloudflare_api.go`, `autotls.go`, `observability.go`.
- Fuzz tests for `parseDuration`, `parseRate`, `parseSize`.
- Benchmark suite for router matching, response-buffer replay, gzip compression, and rate-limit contention.
- `t.Parallel()` across all CPU-bound tests.

#### Feature expansion

- `statute.CORS(...)` middleware with `Origins`, `Methods`, `Headers`, `Credentials`, `MaxAge`, `ExposeHeaders` builders. Spec-compliant preflight. Wildcard origin with `Credentials()` is rejected at resolve time.
- `statute.SecurityHeaders()` middleware with `HSTS`, `CSP`, `FrameOptions`, `ContentTypeOptions`, `ReferrerPolicy`, `PermissionsPolicy` builders. Sensible defaults; HSTS strictly opt-in.
- `statute.RequestID()` middleware with `Header()` and `From()` builders. Generates a 16-byte hex ID, propagates to upstream, surfaces in the JSON access log as a `request_id` field.
- `statute.BodyLimit("1MB")` middleware backed by `http.MaxBytesReader`. Adds a `parseSize` parser to the resolve stage.
- `statute.BasicAuth(realm, users)` middleware. Users map values must be bcrypt hashes; resolve-time validates the prefix and runtime uses `crypto/subtle.ConstantTimeCompare`.
- `statute.AllowIPs(...)` and `statute.DenyIPs(...)` middleware backed by `netip.Prefix`. Respects `BehindCloudflare()` because it uses the shared `clientIP` helper.

#### DX and wow moves

- `Makefile` with `test`, `test-race`, `lint`, `cover`, `bench`, `fuzz`, `build-examples`, and `all` targets.
- `examples/dev/docker-compose.yml` runnable demo: three echo backends behind a statute proxy.
- Godoc `Example*` functions for the major constructors; rendered inline on pkg.go.dev.
- `statute.Main` gains two new flags:
  - `-graph` emits a Graphviz `.dot` representation of the resolved topology.
  - `-lint` audits the resolved config against eight production-readiness rules (`RHT001` ReadHeaderTimeout, `HC001` health checks, `OBS001` metrics, `LB001` single-backend pool, `TLS001` storage on `/tmp`, `RL001` rate too low, `AUTH001` BasicAuth on HTTP, `SHUT001` short grace period).
- `docs/comparison.md` comparing statute with nginx, Caddy, and Traefik.

### Changed

- The `MiddlewareType` iota in `resolved/resolved.go` is now documented as an append-only public contract for the JSON export format.

### Internal

- New direct dependency: `golang.org/x/crypto/bcrypt` (already transitively present).

---

## [v0.1.0] — 2026-05-07

### Added

- Initial release.
- HTTP/1.1 and HTTP/2 listeners (`HTTP`, `HTTPS`).
- HTTP/3 listener via [quic-go](https://github.com/quic-go/quic-go); `Alt-Svc` header advertised on the HTTPS response.
- TLS modes:
  - `StaticTLS(certFile, keyFile)`.
  - `AutoTLS(domains...)` via [autocert](https://pkg.go.dev/golang.org/x/crypto/acme/autocert) with HTTP-01 + TLS-ALPN-01.
  - `AutoTLS(...).CloudflareDNS01(token).Zone(id)` — custom DNS-01 manager (account key + cert persistence, hourly renewal) without lego or certmagic.
- `Listener.BehindCloudflare()` option: drops `acme-tls/1` ALPN entry and trusts `CF-Connecting-IP` / `True-Client-IP` for client-IP attribution.
- Upstream pools with weighted backends, backup tier, active health checks (consecutive thresholds), and four strategies: `RoundRobin`, `LeastConnections`, `IPHash`, smooth-weighted-RR (`Weighted`).
- Route declaration with `Match(pattern).Host(...).ProxyTo(name)` or `Serve(dir)`. Patterns: exact or trailing `/*`. Routes matched in declaration order.
- Middleware: `Timeout`, `RateLimit(...).Per(...)`, `Retry(max, OnStatus(...))`, `Cache`, `Compress(Gzip, Brotli)`, `ETag`. Retry skips non-idempotent methods, gRPC, SSE, WebSocket upgrades, and bodies > 1 MiB.
- Observability:
  - `JSONLog(dest).Sample(rate)` access log; errors logged unconditionally.
  - `Prometheus(addr, path)` text exposition endpoint, with pprof under `/debug/pprof/*` on the same listener.
  - `OTLP(endpoint).ServiceName(...).Insecure().Sample(rate)` distributed tracing via gRPC, with W3C trace-context propagation to upstream backends.
- `Defaults` block: `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, `MaxHeaderBytes`. `ReadHeaderTimeout` defaults to `5s` (Slowloris mitigation).
- `Shutdown` block: `GracePeriod`, `DrainListeners`. Signal handlers for SIGINT/SIGTERM with parallel listener drain and OTel span flush.
- Two-layer architecture: surface API (`github.com/kjanat/statute`) and resolved schema (`github.com/kjanat/statute/resolved`).
- `statute.Resolve(cfg)` and `statute.Export(cfg, w)` for tooling. `statute.Main(cfg)` CLI wrapper with `-validate` and `-export` flags.

[Unreleased]: https://github.com/kjanat/statute/compare/v0.2.0...HEAD
[v0.2.0]: https://github.com/kjanat/statute/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/kjanat/statute/releases/tag/v0.1.0
