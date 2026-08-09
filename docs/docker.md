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
                CloudflareDNS01(os.Getenv("CF_API_TOKEN")),
            statute.HTTP2(),
        ),
    },
    Docker: statute.Docker().
        Network("proxy").
        TraefikLabels(),
})
```

## Provider options

| Option | Meaning |
| --- | --- |
| `Endpoint(string)` | Docker Engine API endpoint. Default `unix:///var/run/docker.sock`; `tcp://host:port` also works. |
| `Network(string)` | Docker network whose IP is used to reach containers. Overridable per container with `statute.network` / `traefik.docker.network`. |
| `ExposedByDefault()` | Register every running container without requiring an enable label (Traefik's `exposedByDefault=true`). statute's default is opt-in. |
| `TraefikLabels()` | Additionally honor `traefik.*` labels (see below). |
| `Refresh(string)` | Periodic full resync interval, e.g. `"30s"`. Default: events only; the provider already resyncs whenever the event stream reconnects. |

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

| Label | Meaning |
| --- | --- |
| `statute.enable` | `"true"` / `"false"`. Required unless the container carries other `statute.*` labels or `ExposedByDefault()` is set. `"false"` always wins. |
| `statute.service` | Pool name. Containers sharing it pool together (replicas). Default: compose service name, else container name. |
| `statute.host` | Comma-separated hosts; each becomes a route. Empty: any host. |
| `statute.path` | statute pattern: `/exact` or `/prefix/*`. Default `/*`. |
| `statute.routes.<name>.host` / `.path` | Additional named routes for the same service. |
| `statute.port` | Container-side port. Default: the single exposed TCP port; ambiguous or missing ports skip the container with a warning. |
| `statute.scheme` | `http` (default) or `https` for the backend connection. |
| `statute.network` | Docker network to take the IP from, overriding `Network()`. |
| `statute.weight` | Backend weight for the `weighted` strategy. Default 1. |
| `statute.backup` | `"true"` marks a failover-only backend. |
| `statute.strategy` | `round_robin`, `least_connections`, `ip_hash`, `weighted`. First container in the pool (by name) wins. |
| `statute.healthcheck.path` / `.interval` / `.timeout` | Active health checks, same semantics as `statute.HealthCheck`. |
| `statute.timeout` | Per-route timeout middleware, e.g. `30s`. |
| `statute.ratelimit` | Per-route rate limit, e.g. `100/min`. |
| `statute.compress` | `gzip`, `br`, a comma list, or `true` for both. |

## Traefik compatibility (`TraefikLabels()`)

The goal is that a fleet labeled for Traefik's docker provider migrates
without editing compose files. Supported:

| Label | Notes |
| --- | --- |
| `traefik.enable` | Same opt-in semantics as `statute.enable`. |
| `traefik.http.routers.<r>.rule` | `Host()`, `Path()`, `PathPrefix()` combined with `&&`, `\|\|`, parentheses, and multi-argument `Host()`. |
| `traefik.http.routers.<r>.service` | Router→service binding, with Traefik's defaulting (sole defined service, else implicit service per router). |
| `traefik.http.services.<s>.loadbalancer.server.port` | Container-side port. |
| `traefik.http.services.<s>.loadbalancer.server.scheme` | `https` for TLS backends. |
| `traefik.http.services.<s>.loadbalancer.healthcheck.path` / `.interval` / `.timeout` | Mapped to statute active health checks. |
| `traefik.docker.network` | Same as `statute.network`. |

Deliberately ignored (harmless, handled at the listener level in statute):
`entrypoints`, `tls`, `tls.certresolver`, `priority`.

Skipped **with a logged warning** rather than silently mis-routed:

- routers whose rule uses unsupported matchers (`HostRegexp`, `Header`,
  `Query`, `ClientIP`, negation, …),
- `middlewares` references — Traefik middleware definitions don't exist
  here; attach `statute.timeout` / `statute.ratelimit` / `statute.compress`
  labels instead,
- `traefik.tcp.*` / `traefik.udp.*` routers.

Traefik-derived pools are namespaced (`api` becomes `api@traefik`), so they
can't collide with pools from native labels or static `Upstreams`.

## Migration from Traefik, in three steps

1. Replace the Traefik container with your statute binary; declare the
   listeners/TLS you had as entrypoints and certificate resolvers, and add
   `Docker: statute.Docker().TraefikLabels().Network("<your proxy network>")`.
2. Start it next to your existing stack. Watch the log for
   `statute: docker:` warnings — each names a container and label that
   needs attention (unsupported matcher, ambiguous port, middleware
   reference).
3. Migrate labels to `statute.*` at your own pace, or don't — the compat
   subset keeps working.
