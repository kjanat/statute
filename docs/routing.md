# Multi-host route declarations

Use `Hosts` when several hosts need the same route:

```go
statute.Match("/api/*").
    Hosts("app.example.com", "www.example.com").
    ProxyTo("api").
    With(statute.Timeout("30s"))
```

`Resolve` expands the declaration into consecutive ordinary single-host routes,
in the supplied order and before the next declaration. `ProxyTo`, `Serve`,
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
