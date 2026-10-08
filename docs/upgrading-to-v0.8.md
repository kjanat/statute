# Upgrading to Statute v0.8

v0.8.0 contains security fixes for shared caching, request identity and Docker
route policy. Review existing middleware configurations before upgrading from
v0.7.x. HTML rewriting remains private research; there is no public rewriting
API in this release.

## Resource limits

Limits belong to each compiled middleware instance. Routes using the same
upstream pool have independent budgets; no process-wide quota is imposed.

| Middleware             | Default limits                                                  | When capacity is exceeded                                                                                    |
| ---------------------- | --------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| Cache                  | 1,024 entries; 8 MiB response body; 64 MiB allocation budget    | Deliver without caching; response overflow switches to streaming.                                            |
| ETag / Retry responses | 8 MiB body; 64 MiB body-allocation budget each                  | 502 for body overflow; 503 for allocation exhaustion.                                                        |
| Retry requests         | 64 MiB request-allocation budget; existing 1 MiB replay ceiling | Deliver the request once without retrying when replay capacity is unavailable.                               |
| Timeout                | 8 MiB body; 64 MiB body-allocation budget; 128 active producers | 502 for body overflow; 503 for allocation or producer exhaustion.                                            |
| RateLimit              | 65,536 retained client/host keys                                | Existing keys retain normal enforcement; new keys receive 503 at capacity. Ordinary rate excess remains 429. |

Use `MaxResponseBody` and `BufferBudget` to tune response limits; Cache also has
`MaxEntries`, Retry has a separate `RequestBufferBudget`, Timeout has `MaxInFlight`,
and RateLimit has `MaxBuckets`. For example:

```go
ETag().MaxResponseBody("16MiB").BufferBudget("128MiB")
```

Budget for final body length, concurrent allocations and growth overlap.
These budgets do not cap total process RSS. Timeout producers that ignore
cancellation retain their slots until they actually exit. Timeout no longer
supports HTTP/2 Push; it returns `http.ErrNotSupported`.

See [cache capacity](production.md#cache-capacity),
[buffered response limits](production.md#buffered-response-limits),
[Timeout bounds](production.md#timeout-bounds), and
[Retry request buffers](production.md#retry-request-buffer-limits).

## Configuration validation

- Put AllowIPs, DenyIPs and RequestID before every enabled Cache. Statute rejects
  unsafe ordering. RateLimit keeps declaration-order
  semantics: before Cache counts all requests; after Cache counts misses.
- Configure at most one RequestID per assembled route. Its output header must
  not be an HTTP control field such as Content-Length, Content-Type or Set-Cookie.
  Authorization and Cookie remain supported credential outputs and disable cache
  reuse for that route. See the [complete reserved set](request-id.md#reserved-output-headers).
- Remove raw response Content-Encoding Set/Add/Remove operations from routes
  using Compress. Compression owns that metadata.

Invalid static/fallback configuration fails resolution. Invalid assembled Docker
chains refuse affected routes while healthy siblings remain available. Run your
application's `-validate` command if it uses `statute.Main`, or call `Resolve`
in your configuration tests; also inspect Docker discovery diagnostics.

## Cache and HTTP behavior

Shared Cache bypasses requests with Authorization/Cookie fields, client
certificates, request bodies or trailers. Private, Set-Cookie and no-cache
responses are not stored. Origin freshness caps configured TTL, and Vary
separates response variants. Existing policies can therefore produce fewer hits.

All HTTP/3 requests currently bypass Cache, including empty requests. Safe
body-and-trailer absence detection is tracked in
[#165](https://github.com/kjanat/statute/issues/165). Empty HTTP/1 and HTTP/2
requests remain eligible. Custom authorization inside a cached handler is still
skipped on hits; see [per-request policy](cache-request-policy.md).

Explicit ETag middleware now renders the inner GET representation for HEAD,
evaluates the original conditions and suppresses the body. Account for that
rendering cost. Compression returns 400 for malformed Accept-Encoding and 406
when a successful response has no acceptable representation. See
[ETag and compression ordering](production.md#body-derived-etags).

## Docker routing

A rejected route with a parsed matcher defeats broader or equally specific
dynamic serving routes; rejection wins ties. More-specific healthy routes and
static-route priority remain intact. Unparseable rules still produce fallback-only
refusals and may be shadowed by valid routes.

Native timeout/rate-limit/compression hints remain attached to their originating
routes when containers share a service. Invalid hints refuse the affected
registration; conflicting middleware on identical predicates refuses that
predicate group. Repair labels or middleware references to restore those routes.
See [Docker discovery](docker.md).

## Toolchain and research status

Go 1.27 remains the module language requirement; `go1.27.1` is the preferred
toolchain. Upgrade the dependency with `go get statute.kjanat.dev@v0.8.0`, then
run your application tests and configuration validation before deployment.

The separate `research/htmlrewrite` module contains the LOL HTML/Wasm prototype
behind `statute_htmlrewrite`. Ordinary Statute consumers need neither Rust nor
the research artifact. Configurable programs, Go callbacks and public production
integration remain under [#114](https://github.com/kjanat/statute/issues/114).
