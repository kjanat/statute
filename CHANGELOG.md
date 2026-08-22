# Changelog

All notable changes to statute are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). statute is pre-1.0; minor releases may include breaking surface-API changes.

## [Unreleased]

### Added

- Request and response header middleware: `SetRequestHeader`,
  `AddRequestHeader`, `RemoveRequestHeader`, `SetResponseHeader`,
  `AddResponseHeader`, and `RemoveResponseHeader`. Operations run in
  declaration order, header names are canonicalised and values validated at
  resolve time (rejecting header injection and the unsettable request `Host`),
  and both appear in the resolved and exported schema as `HeaderName` /
  `HeaderValue`. Response mutations are applied when the response header is
  committed, through a wrapper that preserves flushing and connection
  hijacking.

### Fixed

- Exact-path static routes serve the file their pattern names instead of the
  served directory's root. `Match("/robots.txt").Serve("./public")` now serves
  `./public/robots.txt`; prefix stripping is applied only to trailing-wildcard
  directory routes, whose behavior is unchanged.
- DNS-01 wildcard certificates are reused for matching SNI hosts instead of
  issuing and caching a separate certificate for each concrete hostname.

## [0.5.1] — 2026-08-22

### Changed

- Go 1.27 or newer is now required.
- CI now takes its Go version from `go.mod`, analyzes both Go and GitHub
  Actions with CodeQL, validates workflows with Actionlint, and discovers
  and runs every fuzz target independently.
- Commit signing is provided by a reusable local action and rejects a
  missing default branch instead of silently assuming `master`.

### Fixed

- Middleware resolution now distinguishes fallible and infallible
  middleware instead of manufacturing unused `nil` errors. This has no
  public API or behavior change.
- Formatting checks use the mutually compatible `gci`, `gofmt`, and
  `goimports` set under Go 1.27, without the deprecated `gofumpt`
  configuration.

## [0.5.0] — 2026-08-18

### Added

- Docker label discovery provider (`statute.Docker()`, new `Config.Docker`
  field): containers register routes and upstream pools via `statute.*`
  labels, discovered over the Docker Engine API (unix socket or TCP) with
  event-driven reconciliation, debounced bursts, atomic route-table
  generations, and pool-handler reuse that preserves health-check state
  across reconciles. Label-derived routes are matched only after all
  static routes. Includes a `TraefikLabels()` compat mode honoring the
  common subset of Traefik's docker labels — router rules with
  `Host`/`Path`/`PathPrefix` (`&&`, `||`, parentheses),
  `loadbalancer.server.port`/`.scheme`, loadbalancer health checks,
  `traefik.enable`, and `traefik.docker.network` — so fleets already
  labeled for Traefik migrate without editing compose files. Implemented
  in-tree with a minimal Docker API client (`internal/docker`); no new
  module dependencies. See `docs/docker.md` and `examples/docker`.

### Fixed

- Docker-discovered services with unsupported backend schemes — including
  `h2c`, which statute does not proxy — are now skipped with a warning
  instead of being silently registered as plain HTTP. This applies to both
  `statute.scheme` and Traefik's `loadbalancer.server.scheme` label.

## [0.4.0] — 2026-05-17

### Changed

- Extracted the pure config-string parsers (`Duration`, `DurationOr`,
  `Rate`, `Size`) out of `resolve.go` into a new internal package
  `internal/parse`; `resolve.go` calls them via `parse.*` and no longer
  imports `strconv`/`math`. `sizeMultiplier` reworked to a prefix-index
  form, extending accepted byte-size units to t/p/e/z/y/r/q. No public
  API change.
- Extracted the Cloudflare DNS-01 API client out of `cloudflare_api.go`
  into a new internal package `internal/cloudflare` (`cloudflare.Client`,
  `New`, `FindZoneID`, `AddTXTRecord`, `DeleteRecord`); `dns01.go`
  rewired accordingly. No public API change.
- Vanity host (`statute.kjanat.dev`) now also publishes a `404.html`
  carrying the same `go-import`/`go-source` meta, so every subpackage
  resolves directly for `go get` instead of relying on prefix fallback;
  browsers get a path-aware redirect to the exact pkg.go.dev page.
- CI `lint`, `fuzz`, and `examples` jobs moved to the lighter
  `ubuntu-slim` runner. `test` stays on `ubuntu-latest` because
  `go test -race` needs a C toolchain (cgo) that slim does not ship.

### Fixed

- Added package doc comments to the `basic` and `http-only` examples;
  pkg.go.dev rendered "There is no documentation for this package" for
  them. Enforced going forward by `revive`'s `package-comments` rule.
- `Rate` (rate-limit parser) now rejects non-finite counts:
  `strconv.ParseFloat` accepts `"NaN"`/`"Inf"`, which slipped past the
  positivity check and produced non-finite rates.
- Cloudflare `FindZoneID` no longer swallows API/auth/network errors
  during the zone-label walk and misreport them as "no zone found"; the
  underlying error now propagates.
- OpenTelemetry traces reported a hardcoded `statute.version` of
  `0.1.0`. It is now derived from `debug.ReadBuildInfo()` — the pinned
  module version when imported, the VCS revision for a local statute
  checkout, else `unknown` — with no hand-maintained constant. A
  `(devel)` statute dependency no longer borrows the host app's VCS
  stamp. The module path is derived via `reflect.TypeFor`, so it tracks
  module renames automatically.

## [0.3.0] — 2026-05-17

### Added

- `FuzzParseSize` fuzz target; loosened the `parseDuration` / `parseRate`
  / `parseSize` fuzz invariants to cut false failures.

### Changed

#### Breaking

- Module path renamed to `statute.kjanat.dev`. Importers must update
  their import paths. `.editorconfig` removed.

#### Repository & CI

- Moved `CODEOWNERS`, `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`, and
  `SECURITY.md` under `.github/`; `SECURITY.md` rewritten and
  `CODE_OF_CONDUCT.md` condensed; `CONTRIBUTING.md` reframed (dropped the
  clustering non-goal, allow L4 SNI / PROXY-protocol passthrough).
- Pages workflow heredoc fix; CI fuzz step fixed (`-fuzz` cannot target
  multiple packages in one invocation); formatter/linter config tidied.

#### Complexity refactor (no behaviour change)

- Brought every function to cyclomatic complexity ≤ 10 (Go Report Card
  `gocyclo > 15`, then the repo's stricter `min-complexity: 10`). Large
  functions split into focused helpers across `server.go` (`newServer`,
  `Start`, `Shutdown`, `buildHTTPServer`), `resolve.go` (`Resolve`,
  `resolveListener`, `resolveRoute`, `resolveMiddleware`, `parseSize`,
  `expandDayWeekUnits`), `cors.go` (`corsHandler`), `graph.go`
  (`graphResolved`), `dns01.go` (`issue`), `export.go` (`Main`), and
  `retry.go` (`retryHandler`).
- `resolveMiddleware`'s type switch replaced by a `resolvableMiddleware`
  interface with per-type `resolve()` methods; `applyMiddleware`'s value
  switch replaced by a `middlewareBuilders` dispatch map.
- `graphResolved` now uses an error-accumulating `dotWriter`, removing
  the repeated `if err != nil` ladder.

#### Modernization

- Adopted stdlib helpers via the `modernize` analyzer: `maps.Copy`,
  `slices.Backward`, `strings.CutSuffix`, `sync.WaitGroup.Go`, removed
  redundant Go 1.22+ loop-variable copies.

#### Tooling

- Expanded the `golangci-lint` set: added `modernize`, `gosec`, `noctx`,
  `contextcheck`, `fatcontext`, `errchkjson`, `exhaustive`,
  `predeclared`, `usestdlibvars`, `usetesting`, `copyloopvar`,
  `reassign`, `wastedassign`, `gocheckcompilerdirectives`. Set
  `exhaustive.default-signifies-exhaustive: true`; extended the
  documented `_test.go` relaxation to `noctx`/`errchkjson`/
  `usestdlibvars` (httptest noise).
- Hardening surfaced by the new linters: clamp negative durations before
  the `uint64` Prometheus conversion (gosec G115); switch `net.Listen`
  to `net.ListenConfig.Listen(ctx)` (noctx); rename the `max`-shadowing
  identifiers to `maxAttempts` (predeclared). Verified-intentional
  findings (best-effort cleanup context, log sampling RNG,
  allowlist-gated cert paths, same-host HTTP→HTTPS redirect) are
  suppressed with rationale.
- CI uploads coverage to Codecov from the existing `cover.out` profile.

### Fixed

- `parseSize` no longer overflows `int64` on very large inputs.
- Retry middleware: the large-body single-shot pass-through built its
  fallback `io.MultiReader` from `r.Body` _after_ closing it, so reads
  past the buffered prefix failed with `ErrBodyReadAfterClose`. The body
  is now closed only once the stream is drained or has errored.
- Latent `staticcheck` SA5011 (possible nil dereference) in
  `TestBuildAutocertManager_SingleListener`.
- `unparam` finding: `resolveCompressMW` no longer returns an
  always-nil error.

## [0.2.0] — 2026-05-17

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

## [0.1.0] — 2026-05-07

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
- Two-layer architecture: surface API (`statute.kjanat.dev`) and resolved schema (`statute.kjanat.dev/resolved`).
- `statute.Resolve(cfg)` and `statute.Export(cfg, w)` for tooling. `statute.Main(cfg)` CLI wrapper with `-validate` and `-export` flags.

[Unreleased]: https://github.com/kjanat/statute/compare/v0.5.1...HEAD
[0.5.1]: https://github.com/kjanat/statute/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/kjanat/statute/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/kjanat/statute/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/kjanat/statute/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/kjanat/statute/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/kjanat/statute/releases/tag/v0.1.0
