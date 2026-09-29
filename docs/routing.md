# Multi-host route declarations

Use `Hosts` when several hosts need the same route:

```go
statute.Match("/api/*").
    Hosts("app.example.com", "www.example.com").
    ProxyTo("api").
    With(statute.Timeout("30s"))
```

`Resolve` expands the declaration into consecutive single-host routes in its
table, in the supplied order and before the next declaration. `ProxyTo`, `Serve`,
`RedirectTo`, and `Handle` all support this shorthand. Every expansion keeps the
same path, client-IP matcher, action, and middleware ordering.

Each expanded route has independent runtime middleware state, including cache
and rate-limit state. Upstream pools and application-provided handlers remain
shared, just as when declaring the routes separately. Static routes retain
precedence over Docker routes. Requests that miss the host, path, or client-IP
matcher continue through ordinary routing and fallback rules.

## Validation and matching

- Repeated `Hosts` calls append; supplied slices are copied. A zero-argument call
  appends nothing. The final list must contain at least one entry.
- Empty entries are rejected. Use an omitted host or `Host("")` on a separate declaration
  when any-host matching is intentional.
- Mixing `Host` and `Hosts` fails resolution in either order, including `Host("")`.
- Duplicate entries are rejected using the matcher's case-insensitive comparison.
  Errors identify the offending host entry and original route declaration.
- Other literal values use existing `Host` semantics: matching is case-insensitive
  and preserves trailing-dot spelling. There is no new DNS validation, IDNA
  conversion, wildcard expansion, or normalization. `example.com` and
  `example.com.` are distinct. Supply the same host values you would use with
  `Host`; this shorthand adds no new support for port-qualified host matching.

Existing `Host` declarations are unchanged, including its last-call-wins behavior.
Route hosts do not infer TLS certificate domains; configure those independently.
Export and graph show the concrete host-specific routes. Lint paths refer to their
expanded route indexes; resolution errors refer to the original declaration.

## Ordinary Go alternative

Explicit expansion remains supported and works with earlier Statute versions:

```go
var routes statute.Routes
for _, host := range []string{"app.example.com", "www.example.com"} {
    routes = append(routes,
        statute.Match("/api/*").Host(host).ProxyTo("api").
            With(statute.Timeout("30s")),
    )
}
```

## Terminal native routes

`Config.FallbackRoutes` handles routing misses with the same native route actions
and middleware as `Routes`, without putting a catch-all ahead of Docker discovery:

```text
ordinary Routes
    -> existing Docker dispatch, including quarantines and tombstones
    -> FallbackRoutes, in declaration order
    -> Config.Fallback handler, or the built-in 404
```

Only a **no-match** advances to the next stage. A matched route's 404, auth denial,
backend failure, or exhausted Retry is final; none invokes another fallback.
Upstream response statuses are preserved: fetching a file named `404.html` does
not turn a successful response into 404.

For a CDN-backed default, keep the pool and its transport/health policy native,
with separate path rewrites and cache headers per terminal route:

```go
cfg := statute.Config{
    Listeners: statute.Listeners{statute.HTTP(":8080")},
    Upstreams: statute.Upstreams{
        "default-site": statute.Pool{
            Backends: []statute.Backend{{Address: "https://assets.example.net"}},
            UpstreamHost: statute.TargetHost,
            Transport: statute.Transport{
                DialTimeout: "5s", ResponseHeaderTimeout: "5s",
            },
            HealthCheck: statute.HealthCheck{
                Path: "/ready", Interval: "30s", Timeout: "2s",
            },
            PassiveHealthCheck: statute.PassiveHealthCheck{
                FailureWindow: "30s", MaxFailures: 3,
            },
        },
        "legacy": statute.Pool{
            Backends: []statute.Backend{{Address: "http://legacy.internal:8080"}},
        },
    },
    Docker: statute.Docker().TraefikLabels(),
    FallbackRoutes: statute.Routes{
        statute.Match("/*").Host("legacy.example.com").ProxyTo("legacy"),
        statute.Match("/favicon.ico").ProxyTo("default-site").With(
            statute.ReplacePath("/site/favicon.ico"),
            statute.SetResponseHeader("Cache-Control", "public, max-age=604800"),
        ),
        statute.Match("/*").ProxyTo("default-site").With(
            statute.ReplacePath("/site/index.html"),
        ),
    },
}
```

The legacy route supports incremental migration: any ordinary or discovered route
wins first, and only unmatched requests for `legacy.example.com` continue to that
pool. The later hostless routes deliberately provide a default for every other
host. Add `Host` or `Hosts` constraints instead if unknown hosts should remain 404;
omit `Fallback` to keep the final built-in 404 after all terminal routes miss.

`ProxyTo`, `Serve`, `RedirectTo`, and `Handle` all work in this table. Existing
path, `Host`/`Hosts`, and `ClientIPs` matchers retain their semantics. A `ClientIPs`
miss can select a later route; a matched `AllowIPs` denial cannot. Matching sees
the original path, and hoisted path/header transformations run once even under
Retry. Listener trusted-proxy policy still supplies verified client attribution.

An ordinary route and a terminal route may name the same pool: they share backend
health, balancing, transport/TLS policy, connections, and startup/shutdown ownership,
but not route-local middleware state. Only declared `Config.Upstreams` names are
accepted; terminal routes cannot refer to ephemeral Docker pools. Missing pools
and invalid route declarations fail resolution, not a later request.

Export includes the normalized `FallbackRoutes` collection; `HasFallback` still
marks only an application-owned `Fallback` handler. Graph renders a separate
ordered terminal stage with edges to the same pool nodes. `AUTH001` and `RL001`
apply to both tables, using `fallback_routes[i].middleware[j]` for terminal routes.
`FB001` warns when an ordinary hostless, client-unconstrained catch-all shadows
Docker or either fallback stage; a final catch-all in `FallbackRoutes` is valid.
