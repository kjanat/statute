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

## Audit scope

This is the C03 request-policy outcome in [audit #152](https://github.com/kjanat/statute/issues/152).
It does not settle custom representation identity (C04), resource bounds (C06),
origin freshness (C09), or GET/HEAD request-body selection (C10). See
[production cache guidance](production.md) for storage and representation rules.
