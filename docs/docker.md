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
| `Storage(string)`          | Existing persistent directory for outstanding workload mutations. Required with `Workload`; must survive process/container restarts.  |
| `Middleware(name, mw...)`  | Register a named, code-owned middleware chain that container labels may reference (see below). Re-registering a name replaces it.     |
| `DefaultMiddleware(mw...)` | Middleware applied to every Docker-discovered route, outermost — before label-referenced chains and label hints.                      |
| `PoolPolicy(name, policy)` | Attach code-owned transport, Host, and health policy to one exact discovered-service identity. Re-registering a name replaces it.     |
| `Workload(name, policy)`   | Grant on-demand activation for one exact discovered-service identity: start its container on routed demand, stop it when idle.        |

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
  reconciles once. Activation demand arriving during an in-flight listing
  joins that publication edge; readiness waits one probe interval after
  publication before checking again.
- Route tables swap atomically per generation. Requests in flight finish
  against the generation they started with. Pool handlers whose resolved
  config is unchanged are carried over, keeping health-check state and
  keep-alive connections.
- **Static routes always win.** Label-derived routes are matched only after
  every compiled `Routes` entry, so a container label can never shadow
  configuration you shipped in the binary.
- Label-derived routes are ordered by specificity, not container order:
  host-scoped before host-agnostic, longer path prefixes first, and an exact
  native host before the broader Traefik host matcher when both overlap.
- Label values go through the same resolver as static config (same
  duration/rate parsing, same validation). An invalid label skips that
  service — with a warning logged once — rather than poisoning the rest.

## TLS

Certificates stay static: AutoTLS domains are declared in code, not
discovered from labels. Cover labeled hosts with a wildcard certificate
(`AutoTLS("example.com", "*.example.com")` via DNS-01) or list them
explicitly. A label naming a host outside your certificate coverage will
route, but TLS for it will not be issued.

## On-demand workloads (`Workload`)

A service that is needed rarely can scale to zero. Register its identity
with `Workload` and statute starts the container when a routed request
needs it, holds the request until readiness is established, proxies it, and
stops the container again once it has been idle:

```go
Docker: statute.Docker().
    Storage("/var/lib/statute/docker").
    Workload("tools", statute.WorkloadPolicy{
        IdleAfter:    "15m",
        ReadyTimeout: "2m",
        Readiness:    statute.HTTPReadiness("/healthz"),
    }),
```

Registration is the only source of the start/stop authority; a container
label can never grant it. The policy requires a one-to-one service and
container pair. A merged multi-container service has no single activation
owner, and a container contributing several services has no single
controllable lifecycle, since a stop acts on all of them at once; either
shape leaves the policy unapplied and the provider reports it. Candidate
service claims count toward this topology even when backend validation fails.
`Storage` is required, must already exist, and must persist across process and container restarts.
Do not share it between concurrent Statute processes. One process must remain
the sole lifecycle authority for its governed containers; rolling overlap is
unsupported. Statute validates the registry and its Docker endpoint binding
before publishing Docker routes; corruption or an endpoint mismatch fails
startup closed.

How it behaves:

- **A dormant route still matches.** The stopped container keeps its
  identity, labels, and routes; only the backend address is absent until it
  runs. The route never falls through to `Fallback` because the workload is
  stopped.
- **Running is not ready.** An activated container serves nothing until the
  readiness policy proves it: the container's `HEALTHCHECK` when it defines
  one, else a TCP connect (the default), or `HTTPReadiness(path)` /
  `TCPReadiness` / `DockerHealthReadiness` explicitly.
- **Activation is single-flight.** Concurrent requests for one dormant
  workload produce one start call and one readiness wait, and every waiter
  gets the same outcome. One client disconnecting cancels nothing the
  others still need.
- **Failure is terminal for the request.** A start error or readiness
  timeout answers `503` and never continues into `Fallback`. The container
  statute started is stopped again, and an exponential backoff
  (`BackoffBase` to `BackoffCap`) spaces further attempts; requests inside
  the window get `503` with `Retry-After`.
- **Idle is measured from request completion.** In-flight requests, open
  WebSockets, and open streaming responses each hold the workload active;
  the `IdleAfter` timer starts when the last one finishes. A request
  arriving while the stop is pending revokes it; one arriving after the
  stop call was issued waits and triggers a fresh activation.
- **External changes reconcile.** A container stopped outside statute
  becomes dormant and reactivates on the next request. A container started
  outside statute after startup enters the same readiness gate and idle policy.
  Replacement beneath the same service supersedes
  an in-flight operation: its waiters fail closed, stale cleanup is ignored,
  a running successor proves readiness afresh, and old request completions
  remain bound to the predecessor's idle state. A label change during activation
  invalidates queued requests when route or middleware policy changes.
- **Issued mutations stay owned.** Idle stops and failed-activation cleanup
  stops are written to the persistent registry by immutable container ID before
  Docker receives the mutation, and remain non-serving until their outcome settles.
  If a stop response is lost or Docker returns a server error, an immediate running
  inspect does not reopen traffic: Docker may still finish the stop. That uncertainty
  remains attached to the mutation and cannot be erased by a later rejected retry.
  Statute coalesces discovery and retries the bounded call until positive stopped or
  missing-container evidence resolves the operation, including when periodic refresh
  is disabled. If the service-to-container topology stops being one-to-one during an
  issued stop, lifecycle authority is revoked immediately, but every route contributed
  by that immutable container stays quarantined with `503` until the stop settles and a
  later reconcile removes quarantine. Ordinary pool rules then apply. Quarantine routes
  compile from a fail-closed registration envelope before service contributions merge:
  they remain present when the stopped container has no backend address, and invalid
  rules, middleware, health, or transport configuration cannot replace their `503` with
  a route miss. Only the mutation-owned container contribution is excluded from ordinary
  routing. Another immutable container contributing the same service remains routable
  through its own backend when the same logical service has the identical host kind,
  host, path kind, and path. Ordinary routes and quarantines otherwise share normal
  route specificity: a broad healthy route cannot bypass a narrower quarantine, and a
  narrower healthy route still beats a broad quarantine. Quarantines remain ahead of
  tombstones and fallback.
  Settlement itself schedules the reconcile, so removing
  quarantine does not depend on a Docker event or periodic refresh. Ordinary
  serving-validation results determine the published route outcome only after quarantine
  ends. A recreated container starts with fresh pool health and connections, even when its
  name and backend address are unchanged. Statute's own shutdown leaves workloads as they
  are.

Defaults: `IdleAfter` 15m, `StartTimeout` 30s, `ReadyTimeout` 2m,
`BackoffBase` 5s, `BackoffCap` 5m.

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
(`HealthCheck.Statuses`), passive health (`PassiveHealthCheck`), transport
tuning, and upstream Host policy have no label form. Attach those code-owned
settings to one exact service identity with `PoolPolicy`:

```go
Docker: statute.Docker().TraefikLabels().
    PoolPolicy("api@traefik", statute.PoolPolicy{
        HealthCheck: statute.HealthCheck{
            Path: "/ready", Host: "api.internal", Statuses: []int{200, 204},
        },
        PassiveHealthCheck: statute.PassiveHealthCheck{
            FailureWindow: "30s", MaxFailures: 3,
        },
        Transport: statute.Transport{
            ServerName: "api.internal",
            RootCAFiles: []string{"/run/certs/api-root.pem"},
            ClientCertificate: statute.ClientCertificate{
                CertFile: "/run/certs/api-client.crt",
                KeyFile:  "/run/certs/api-client.key",
            },
        },
        UpstreamHost: statute.HostValue("api.internal"),
    }),
```

Native service keys are their resolved `statute.service` names; Traefik keys
carry the `@traefik` suffix. The policy is authoritative for its four fields,
including zero values, while Docker continues to own backends, strategy, and
routes. It cannot leak to another service or into router middleware. A key that
matches no discovered service produces a deduplicated provider warning. Traefik's
`loadbalancer.responseforwarding.flushinterval` remains unsupported; use
`PoolPolicy.Transport.FlushInterval` instead.

The transport's upstream client certificate is pool-owned too: proxy requests
and active health probes present the same identity. An unreadable, malformed, or
mismatched certificate/key pair rejects only the matched discovered service and
keeps its routes fail-closed; sibling services continue serving.

## Traefik compatibility (`TraefikLabels()`)

The goal is that a fleet labeled for Traefik's docker provider migrates
without editing compose files. Supported:

| Label                                                                                | Notes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `traefik.enable`                                                                     | Exactly Traefik's semantics: without `ExposedByDefault()`, a container is exposed only with an explicit `traefik.enable=true`; router labels alone do not expose it.                                                                                                                                                                                                                                                                                                                   |
| `traefik.http.routers.<r>.rule`                                                      | `Host()`, `Path()`, `PathPrefix()` combined with `&&`, `\|\|`, parentheses, and multi-argument `Host()`. `PathPrefix` is Traefik's byte prefix (`/api` matches `/api-secret`); statute `Match("/api/*")` stays segment-aware. Traefik `Host()` keeps the configured spelling and folds one trailing FQDN dot on the rule or the request. `Host("*")` / `Host("*.example.com")` and `Path`/`PathPrefix` arguments with `%`, placeholders, or regexp syntax are rejected and tombstoned. |
| `traefik.http.routers.<r>.service`                                                   | Router→service binding, with Traefik's defaulting: the sole service defined on the container, else an implicit service named after the container, so several label-less routers share one backend pool.                                                                                                                                                                                                                                                                                |
| `traefik.http.services.<s>.loadbalancer.server.port`                                 | Container-side port. Default: the lowest exposed port, as in Traefik.                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `traefik.http.services.<s>.loadbalancer.server.scheme`                               | `https` for TLS backends. `h2c` is not supported: such services are skipped with a warning, never proxied over the wrong protocol.                                                                                                                                                                                                                                                                                                                                                     |
| `traefik.http.services.<s>.loadbalancer.healthcheck.path` / `.interval` / `.timeout` | Mapped to statute active health checks.                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `traefik.docker.network`                                                             | Same as `statute.network`.                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `traefik.http.routers.<r>.middlewares`                                               | Comma-separated names resolved against the code-owned registry declared with `Middleware(name, ...)`, scoped to that router's routes; see below.                                                                                                                                                                                                                                                                                                                                       |

Deliberately ignored (harmless, handled at the listener level in statute):
`entrypoints`, `tls`, `tls.certresolver`, `priority`.

Skipped **with a logged warning** rather than silently mis-routed:

- routers whose rule uses unsupported matchers (`HostRegexp`, `Header`,
  `Query`, `ClientIP`, `Host("*")`, percent-encoded `Path`/`PathPrefix`,
  negation, …),
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
