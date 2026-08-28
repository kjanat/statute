# Docker label discovery

The Docker provider is statute's one deliberately dynamic corner. The fact
that discovery happens — the socket, the network, the opt-in policy — is
declared in code and validated at startup like everything else, but the
routes and upstream pools it produces come from container labels and follow
containers as they start and stop. If you run Traefik today purely to get
"label a container, it gets routed", this is the migration path.

```go
statute.Main(statute.Config{
    Listeners: statute.Listeners{
        statute.HTTP(":80").RedirectTo("https"),
        statute.HTTPS(":443",
            statute.AutoTLS("example.com", "*.example.com").
                Email("ops@example.com").
                Storage("/var/lib/statute/certs").
                CloudflareDNS01(os.Getenv("CLOUDFLARE_API_TOKEN")),
            statute.HTTP2(),
        ),
    },
    Docker: statute.Docker().
        Network("proxy").
        TraefikLabels(),
})
```

## Provider options

| Option                     | Meaning                                                                                                                               |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `Endpoint(string)`         | Docker Engine API endpoint. Default `unix:///var/run/docker.sock`; `tcp://host:port` also works — but see the warning below.          |
| `Network(string)`          | Docker network whose IP is used to reach containers. Overridable per container with `statute.network` / `traefik.docker.network`.     |
| `ExposedByDefault()`       | Register every running container without requiring an enable label (Traefik's `exposedByDefault=true`). statute's default is opt-in.  |
| `TraefikLabels()`          | Additionally honor `traefik.*` labels (see below).                                                                                    |
| `Refresh(string)`          | Periodic full resync interval, e.g. `"30s"`. Default: events only; the provider already resyncs whenever the event stream reconnects. |
| `Middleware(name, mw...)`  | Register a named, code-owned middleware chain that container labels may reference (see below). Re-registering a name replaces it.     |
| `DefaultMiddleware(mw...)` | Middleware applied to every Docker-discovered route, outermost — before label-referenced chains and label hints.                      |

> **Warning — TCP endpoints are unauthenticated.** The client speaks plain
> HTTP with no TLS or client certificates, and whoever can answer on that
> socket controls your routing. Use `tcp://` only for loopback addresses or
> endpoints reached through an authenticated tunnel (SSH forward, TLS
> sidecar such as `docker-socket-proxy` on a private network). Never expose
> a raw Docker TCP socket to a network you don't fully trust — that is
> remote root on the host, as the Docker docs themselves warn. The unix
> socket default is the safe choice.

## How discovery behaves

- The initial container sync runs **before listeners open**, so the first
  request already sees label-derived routes. An unreachable daemon at
  startup is a fatal misconfiguration, statute-style. After startup, a
  failed reconcile logs and keeps the previous routing generation serving.
- The provider follows the Docker event stream (with reconnect + backoff)
  and coalesces bursts — a `docker compose up` starting ten containers
  reconciles once.
- Route tables swap atomically per generation. Requests in flight finish
  against the generation they started with. Pool handlers whose resolved
  config is unchanged are carried over, keeping health-check state and
  keep-alive connections.
- **Static routes always win.** Label-derived routes are matched only after
  every compiled `Routes` entry, so a container label can never shadow
  configuration you shipped in the binary.
- Label-derived routes are ordered by specificity, not container order:
  host-scoped before host-agnostic, longer path prefixes first.
- Label values go through the same resolver as static config (same
  duration/rate parsing, same validation). An invalid label skips that
  service — with a warning logged once — rather than poisoning the rest.

## TLS

Certificates stay static: AutoTLS domains are declared in code, not
discovered from labels. Cover labeled hosts with a wildcard certificate
(`AutoTLS("example.com", "*.example.com")` via DNS-01) or list them
explicitly. A label naming a host outside your certificate coverage will
route, but TLS for it will not be issued.

## Native label schema (`statute.*`)

Minimal case — one label:

```yaml
services:
  whoami:
    image: traefik/whoami
    labels:
      statute.enable: "true"
```

With one exposed port, the container is routed on every host at `/*` under
a pool named after the compose service. The full schema:

| Label                                                 | Meaning                                                                                                                                                                                                                                               |
| ----------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `statute.enable`                                      | Boolean (parsed like Traefik: `1`/`t`/`true`/`True`, …). Required unless the container carries other `statute.*` labels or `ExposedByDefault()` is set. An explicit false always wins; an unparseable value is not an opt-out: it leaves a tombstone. |
| `statute.service`                                     | Pool name. Containers sharing it pool together (replicas). Default: compose service name, else container name.                                                                                                                                        |
| `statute.host`                                        | Comma-separated hosts; each becomes a route (matched case-insensitively). Empty fragments are skipped. Empty or unset: any host.                                                                                                                      |
| `statute.path`                                        | statute pattern: `/exact` or `/prefix/*`. Default `/*`.                                                                                                                                                                                               |
| `statute.routes.<name>.host` / `.path`                | Additional named routes for the same service.                                                                                                                                                                                                         |
| `statute.port`                                        | Container-side port. Default: the lowest exposed TCP port (Traefik's rule), with a warning when several were exposed; a container exposing no port is dropped with a warning and a tombstone.                                                         |
| `statute.scheme`                                      | `http` (default) or `https` for the backend connection (case-insensitive). Anything else, including `h2c` which statute's proxy does not speak, drops the service with a warning and a tombstone.                                                     |
| `statute.network`                                     | Docker network to take the IP from, overriding `Network()`.                                                                                                                                                                                           |
| `statute.weight`                                      | Backend weight for the `weighted` strategy. Default 1.                                                                                                                                                                                                |
| `statute.backup`                                      | `"true"` marks a failover-only backend.                                                                                                                                                                                                               |
| `statute.strategy`                                    | `round_robin`, `least_connections`, `ip_hash`, `weighted`. First container in the pool (by name) wins.                                                                                                                                                |
| `statute.healthcheck.path` / `.interval` / `.timeout` | Active health checks, same semantics as `statute.HealthCheck`.                                                                                                                                                                                        |
| `statute.timeout`                                     | Per-route timeout middleware, e.g. `30s`.                                                                                                                                                                                                             |
| `statute.ratelimit`                                   | Per-route rate limit, e.g. `100/min`.                                                                                                                                                                                                                 |
| `statute.compress`                                    | `gzip`, `br`, a comma list, or `true` for both.                                                                                                                                                                                                       |

A container these labels select but statute cannot route (no exposed port,
an unsupported scheme, no reachable address, an `enable` value that is
neither true nor false) leaves a **tombstone**
covering the routes its labels declared, as a dropped Traefik router
does; see [Tombstones](#tombstones-what-a-dropped-registration-leaves-behind)
below, which applies to both label schemas. A container carrying
`statute.enable` but naming neither host nor path is routed on every host at
`/*`, so its tombstone is the global one. The single exception is a
container `ExposedByDefault()` registered that carries no `statute.*` label
at all: its catch-all is statute's own inference, and dropping it leaves
the fallback alone.

The health-check labels stop at path/interval/timeout — deliberately. The
probe `Host` override (`HealthCheck.Host`), accepted probe statuses
(`HealthCheck.Statuses`), and passive health (`PassiveHealthCheck`) have no
label form, in either the native or the Traefik schema: they exist only in
compiled configuration. A Docker-discovered pool therefore always runs with
the default 200–399 probe acceptance, the derived probe host, and no
passive demotion. Transport tuning (`Transport`, including
`FlushInterval`) likewise has no label form — Traefik's
`loadbalancer.responseforwarding.flushinterval` is not supported — so a
Docker-discovered pool runs with default transport settings.

## Traefik compatibility (`TraefikLabels()`)

The goal is that a fleet labeled for Traefik's docker provider migrates
without editing compose files. Supported:

| Label                                                                                | Notes                                                                                                                                                                                                                                                                                                                       |
| ------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `traefik.enable`                                                                     | Exactly Traefik's semantics: without `ExposedByDefault()`, a container is exposed only with an explicit `traefik.enable=true`; router labels alone do not expose it.                                                                                                                                                        |
| `traefik.http.routers.<r>.rule`                                                      | `Host()`, `Path()`, `PathPrefix()` combined with `&&`, `\|\|`, parentheses, and multi-argument `Host()`. `PathPrefix` is Traefik's byte prefix (`/api` matches `/api-secret`); statute `Match("/api/*")` stays segment-aware. `Path`/`PathPrefix` arguments with placeholders or regexp syntax are rejected and tombstoned. |
| `traefik.http.routers.<r>.service`                                                   | Router→service binding, with Traefik's defaulting: the sole service defined on the container, else an implicit service named after the container, so several label-less routers share one backend pool.                                                                                                                     |
| `traefik.http.services.<s>.loadbalancer.server.port`                                 | Container-side port. Default: the lowest exposed port, as in Traefik.                                                                                                                                                                                                                                                       |
| `traefik.http.services.<s>.loadbalancer.server.scheme`                               | `https` for TLS backends. `h2c` is not supported: such services are skipped with a warning, never proxied over the wrong protocol.                                                                                                                                                                                          |
| `traefik.http.services.<s>.loadbalancer.healthcheck.path` / `.interval` / `.timeout` | Mapped to statute active health checks.                                                                                                                                                                                                                                                                                     |
| `traefik.docker.network`                                                             | Same as `statute.network`.                                                                                                                                                                                                                                                                                                  |
| `traefik.http.routers.<r>.middlewares`                                               | Comma-separated names resolved against the code-owned registry declared with `Middleware(name, ...)`, scoped to that router's routes; see below.                                                                                                                                                                            |

Deliberately ignored (harmless, handled at the listener level in statute):
`entrypoints`, `tls`, `tls.certresolver`, `priority`.

Skipped **with a logged warning** rather than silently mis-routed:

- routers whose rule uses unsupported matchers (`HostRegexp`, `Header`,
  `Query`, `ClientIP`, negation, …),
- routers whose `middlewares` label names a chain no
  `Middleware(name, ...)` registration covers — the router's routes are
  omitted (fail closed) while sibling routers and services keep routing,
- `traefik.tcp.*` / `traefik.udp.*` routers.

### Tombstones: what a dropped registration leaves behind

This section describes both label schemas. A **registration** is a Traefik
router or a container's native `statute.*` labels; both declare routes, and
dropping either has the same consequence.

A dropped registration does not simply vanish. Its traffic used to end in
the terminal 404, and with a `Config.Fallback` configured it would instead
fall through into operator code that does not know the registration asked
for a policy statute could not supply. So the generation keeps a
**tombstone** for it: a matcher with no upstream and no middleware that
answers the same 404, consulted after the discovered routes and before the
fallback. Without a fallback configured nothing changes.

A tombstone covers everything the dropped registration could have matched,
never less. Constraints statute cannot represent are dropped:

| Rule                                                         | Refuses                                                     |
| ------------------------------------------------------------ | ----------------------------------------------------------- |
| ``Host(`admin.example.com`) && ClientIP(`10.0.0.0/8`)``      | `admin.example.com`, all paths                              |
| ``Host(`a.example.com`) && PathPrefix(`/api`) && Header(…)`` | `a.example.com` PathPrefix `/api` (including `/api-secret`) |
| ``PathPrefix(`/private`) && ClientIP(…)``                    | PathPrefix `/private` on every host                         |
| ``Host(`a.example.com`) && Path()``                          | `a.example.com`, all paths                                  |
| ``Host(`a.example.com`) \|\| ClientIP(…)``                   | everything                                                  |
| `ClientIP(…)`, `HostRegexp(…)`, `Path()`, a typo             | everything                                                  |

Dropping a conjunct only adds requests. An unreadable matcher, such as
zero-argument `Path()`, standing beside a readable conjunct, costs no more
than one statute reads but cannot represent, because a rule matches every one
of its conjuncts, so any single conjunct already covers it. A disjunction is the opposite: it is a union of its branches, so
one unreadable branch widens the whole rule. The last row is the **global
tombstone**, reached when nothing in the rule bounds it because it does not
parse or every matcher in it widened away. It refuses every unmatched
request in that generation, which disables the fallback until the labels
are fixed.

Every tombstone is logged with the traffic it now refuses, and the global
one says so in as many words. What else the line names is whatever the stage
that dropped the routes knew: a drop at the label stage names the container
and the router, while a drop at the pool stage (an unresolvable pool, a
pool handler that cannot be built) names the service, since one service's
pool can be assembled from several containers. On top of the per-drop lines,
the generation's standing refusal is announced whenever it changes, so a
rule that is repaired and later regresses does not disable the fallback in
silence.

`traefik.tcp.*` / `traefik.udp.*` routers leave no tombstone. The tier
expresses HTTP refusals only. Neither does a router with no rule (it
matches nothing in Traefik either) or a container that opted out with
`traefik.enable=false`. An `enable` value that is neither true nor false is
not an opt-out: it cannot be read, so the routes are dropped and the
envelope stands.

### Label-referenced middleware

Traefik middleware _definitions_ don't exist here — labels never define
middleware. Instead, code registers named chains and labels select them:

```go
Docker: statute.Docker().TraefikLabels().
    Middleware("edge-security@file", statute.SecurityHeaders()).
    DefaultMiddleware(statute.RequestID()),
```

A `traefik.http.routers.<r>.middlewares` label naming `edge-security@file`
— the label value must match the registered name **verbatim**, `@provider`
suffix included — attaches that chain to the routes derived from that
router, and only those. References are **router-scoped**, as in Traefik:
routers sharing one service keep their own chains while pooling into the
same backends. This keeps the config-as-code trust boundary: a label can
only pick policies compiled into the binary, and existing container labels
migrate without edits.

Per route, the chain order is: `DefaultMiddleware` outermost, then the
router's referenced chains in label order, then the `statute.timeout` /
`statute.ratelimit` / `statute.compress` hints.

A reference to an unregistered name **fails closed**: that router's routes
are omitted from the generation and replaced by a tombstone refusing the
traffic they matched, with a warning naming the missing middleware. A route
that asked for an auth policy must never be served without it, so a broken
middleware dependency means a broken router, as in Traefik: the traffic
does not reach `Config.Fallback`.
Other routers on the same service, and every other service, are unaffected.

Traefik-derived pools are namespaced (`api` becomes `api@traefik`), so they
can't collide with pools from native labels or static `Upstreams`.

## Migration from Traefik, in three steps

1. Replace the Traefik container with your statute binary; declare the
   listeners/TLS you had as entrypoints and certificate resolvers, and add
   `Docker: statute.Docker().TraefikLabels().Network("<your proxy network>")`.
2. Start it next to your existing stack. Watch the log for
   `statute: docker:` warnings — each names a container and label that
   needs attention (unsupported matcher, ambiguous port, unregistered
   middleware name).
3. Migrate labels to `statute.*` at your own pace, or don't — the compat
   subset keeps working.
