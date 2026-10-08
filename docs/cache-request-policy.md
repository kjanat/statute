# Cache and per-request policy

A Cache hit returns a stored response without invoking its inner handlers. The
first declared middleware is outermost. This matters for authorization, retry
attempts, rate limits, and request observation.

## Supported contracts

| Component                            | Cache-hit behavior                                                                                                                                                   | Regression evidence                                                            |
| ------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------ |
| BasicAuth                            | Authorization-bearing requests bypass lookup and storage in either order. Missing credentials produce a non-cacheable 401.                                           | `TestCacheCannotBypassBasicAuth`                                               |
| AllowIPs / DenyIPs                   | Must precede every enabled Cache. Invalid static/fallback configuration is rejected; Docker refuses the affected router.                                             | `TestResolveCacheIPPolicyOrder`, `cache_policy_order_test.go`                  |
| RequestID                            | Must precede every enabled Cache; each external request gets its current ID, including cached responses and listener logs.                                           | `TestCacheRequestIDFreshOnHits`, `TestCacheRetryListenerObservation`           |
| RateLimit                            | Before Cache limits every request; after Cache limits only misses. No automatic reordering.                                                                          | `TestCacheRateLimitPlacement`                                                  |
| Retry                                | Non-2xx responses, including 503, are not cached. A hit avoids new producer calls. Custom retry statuses do not change cache admission; see below.                   | `TestCacheRetryListenerObservation`, `TestCacheRetrySuccessfulStatusSelection` |
| Listener access logs and metrics     | Observe external requests, including hits, once each. Intermediate retry attempts do not become external requests. Existing sampling and status filters still apply. | `TestCacheRetryListenerObservation`                                            |
| Custom authorization inside `Handle` | Skipped on a hit. Statute cannot infer a handler's authorization or revocation requirements.                                                                         | `TestCacheCustomAuthorizationRevocation`                                       |

## Retrying successful statuses

Retry statuses can include otherwise cacheable successes. With
`With(Retry(2, OnStatus(200)), Cache("1m"))`, the second attempt can consume the
first attempt's cached response. With `With(Cache("1m"), Retry(2, OnStatus(200)))`,
a miss runs both producer attempts before Cache stores the final response.
Neither ordering silently bypasses or invalidates existing entries for Retry.

## Custom authorization

If a handler checks a lease, account status, an API key in a custom header, or
another mutable policy on every request, **omit Cache from that route**. Passing
that handler to `Handle` puts it inside the route middleware, including Cache.
Wrapping the application handler before passing it to `Handle` does not move
authorization outside Cache.

For example, keep a revocation-sensitive handler uncached:

```go
Match("/private/*").Handle(authorizeThenServe)
```

Adding `.With(Cache("1m"))` would permit an earlier successful response to bypass
that handler until the entry expires. A `Vary` key can select a representation;
it does not recheck whether the selected identity is still authorized.

Using the standard Authorization or Cookie request fields bypasses shared Cache
lookup and storage. A response that is private or no-store from its first
successful delivery also cannot populate the cache. Sending no-store only after
revocation cannot protect an already-stored response: the cache hit never reaches
the code that would send it. Route-wide no-store prevents new storage, but is not
a general substitute for per-request enforcement.

The revocation regression includes a deliberately unsafe cached-handler control.
Its successful cached response after revocation documents the unsupported
configuration; it is not evidence that arbitrary application authorization is
safe to cache.

## Representation identity and native proxies

Cache keys include the method, request Host, effective URL target (path and query),
original `RequestURI`, URL scheme, and presence of downstream TLS. Hoisted path
rewrites preserve the original target as a separate key dimension. Two requests
rewritten to the same URL therefore remain distinct when a `Handle` inspects
their original targets.

Native `ProxyTo` and Docker routes also account for request-header changes made
after Cache. Their rules apply to static routes, fallback routes, and each Docker
router independently; sharing a backend pool does not share route cache policy.

- Any `Connection` field bypasses native lookup and storage, including an empty
  field or `keep-alive`. The proxy can remove headers nominated by that field.
- Responses varying on `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, or
  `X-Forwarded-Proto` are not stored. Native proxying derives these values after
  cache selection; explicit route Set/Add/Remove operations retain their normal
  effect. Both origin Vary and projected response-header Vary participate.
- The route captures the selected OpenTelemetry propagator and its declared
  output fields once per external request. Responses varying on those fields
  are not stored. A propagator declaring credentials, cache controls,
  conditions/ranges, connection controls, or body/framing fields bypasses caching
  entirely, even without Vary. Invalid declared field names also bypass caching.
- Different propagation field sets have separate cache namespaces. If the
  captured object's declared fields change during a request, a shared request
  marker disables subsequent lookups and storage, including Retry re-entry and
  nested caches. This includes OpenTelemetry's first-registration delegation.
- Injection still happens only at the actual proxy attempt. A cache hit does not
  perform a synthetic injection; retries inject once per real attempt. Routes
  without an enabled Cache keep ordinary propagation behavior.

These restrictions preserve response delivery: uncertainty makes a request
uncached, without producing a new HTTP error. Ordinary anonymous native responses
and application-header Vary variants remain cacheable. Custom `Handle` routes
can also cache Vary on forwarded headers because Statute does not apply the
native proxy's late header transformations to them.

Custom propagators must accurately implement `Fields()` and keep their declared
output set stable during an injection. Undeclared outputs and changes that
disappear before either observation are outside that interface contract.
Likewise, Cache cannot infer custom personalization based on arbitrary context
values, peer IP, or application state. Omit Cache for such responses or mark
them private/no-store from the first delivery. Vary can describe request-header
selection; it cannot describe arbitrary context state or recheck authorization.

Regression evidence: `cache_native_identity_test.go`,
`cache_native_propagation_test.go`, and `cache_native_boundary_test.go` cover
forwarded identity, Connection nomination, path/scheme/TLS separation, initial
delegation, concurrent propagation replacement, Retry/ETag, shared pools, and
Docker generation replacement. `TestCacheCustomContextIdentity` and
`TestCacheCustomAuthorizationRevocation` document the application-owned
personalization and authorization boundaries.

## Audit scope

This documents C03 request policy and C04 representation identity in
[audit #152](https://github.com/kjanat/statute/issues/152). C10 body/trailer
selection is covered by the conservative protocol-specific bypass policy.
Cache's entry, body and allocation bounds are described under
[Cache capacity](production.md#cache-capacity); oversized responses stream without
storage. [Origin freshness](production.md#cache-freshness) (C09) uses corrected Age
and a TTL ceiling. Retry request-body retention (C22) has a separate allocation
budget; [Timeout buffering](production.md#timeout-bounds) (C19) has independent
body and producer limits. See
[production cache guidance](production.md) for storage and representation rules.
