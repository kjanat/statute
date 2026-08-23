# Production deployment

This document covers the operational concerns of running statute in production: how to bind low ports without root, signal handling and graceful shutdown ordering, persistent storage requirements, build flags, and the deployment patterns we recommend (and the ones we don't).

## Building the binary

```sh
go build -o statute ./examples/basic
```

The binary is a normal Go executable. There are no runtime dependencies on the build machine — no shared libraries to ship, no config files to colocate, no template directories. Your `main.go` and the resulting binary are the entire deployment artefact.

For smallest binaries:

```sh
go build -trimpath -ldflags="-s -w" -o statute ./examples/basic
```

`-trimpath` removes filesystem paths from the binary; `-s -w` strips DWARF debug info. Typical savings: ~25%. Skip these if you want stack traces with line numbers and absolute paths.

For static linking:

```sh
CGO_ENABLED=0 go build -o statute ./examples/basic
```

statute does not use cgo itself, but Go's default DNS resolver does in some configurations. `CGO_ENABLED=0` forces Go's pure-Go resolver, which produces a fully-static binary you can run from `scratch` containers.

## Running on low ports as a non-root user

Binding to ports below 1024 (`:80`, `:443`) traditionally requires root on Linux. Three options, in order of preference:

### Option 1: setcap

Grant the binary the `cap_net_bind_service` capability:

```sh
sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/statute
```

The binary can now bind low ports as any user. This is the standard mechanism on modern Linux. Capability persists across the binary file but is lost on file overwrite — re-run after every deploy.

### Option 2: systemd

systemd's `AmbientCapabilities` injects the capability without modifying the binary:

```ini
[Service]
ExecStart=/usr/local/bin/statute
User=statute
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
```

Combine with `ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, `ReadWritePaths=/var/lib/statute` for a hardened service. See `man systemd.exec` for the full list.

### Option 3: lower the unprivileged port floor

```sh
echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/99-statute.conf
sudo sysctl --system
```

This makes ports 80–1023 unprivileged for all processes. Convenient but global; affects every binary on the host.

### Don't run as root

Don't. There is no good reason. The proxy handles untrusted input from the network on every connection.

## Persistent storage

AutoTLS (both HTTP-01 and DNS-01) requires persistent storage for the ACME account key, issued certs, and renewal state. The directory layout:

```
<storage>/
├── acme_account+key            # autocert account state (HTTP-01 mode)
├── example.com                 # autocert cert files (HTTP-01 mode)
├── api.example.com
└── dns01/                      # DNS-01 mode subdirectory
    ├── account.key
    ├── example.com.crt
    └── example.com.key
```

The standard mistakes that get deployments rate-limited:

- Mounting the storage path as `tmpfs` (cleared on reboot).
- Wiping the storage path during container builds (clean slate on every deploy).
- Forgetting to mount the storage path at all when running in containers.
- Using `/tmp` (cleared periodically by systemd-tmpfiles).

Let's Encrypt rate limits new orders to 50 per registered domain per week. A misconfigured deployment that re-issues on every restart hits the limit in about 30 minutes if you deploy continuously. The fix is always the same: persistent storage, mounted at the same path on every restart, owned by the user statute runs as.

For containers, mount a named volume:

```yaml
volumes:
  - statute-certs:/var/lib/statute/certs
```

Or a host bind mount in development:

```yaml
volumes:
  - ./certs:/var/lib/statute/certs
```

For Kubernetes, use a `PersistentVolumeClaim`. Don't use an `emptyDir` — it disappears when the pod restarts.

## Signal handling and graceful shutdown

statute installs handlers for `SIGINT` and `SIGTERM`. On either signal:

1. The signal context cancels.
2. Each `http.Server` is given a context with `Shutdown.GracePeriod` deadline and asked to `Shutdown(ctx)`.
3. HTTP/3 servers are asked to `Shutdown(ctx)` in parallel.
4. Once listeners drain, health checkers stop and idle upstream connections close.
5. The OTel TracerProvider flushes pending spans.
6. The process exits.

Any outstanding request gets up to `GracePeriod` to complete. Requests that don't complete in time have their connections force-closed. The default is `30s`, which is fine for typical web traffic; tune up for long-running endpoints (file uploads, server-sent events, video streaming) or down for fast-deploy environments where you want pods to terminate quickly.

`Shutdown.DrainListeners: true` (recommended) causes listeners to stop accepting new connections immediately, while existing connections finish their requests. Without it, the listener closes hard and in-flight requests get TCP RST.

If statute is in a Kubernetes pod, set `terminationGracePeriodSeconds` >= `Shutdown.GracePeriod` + 5 seconds. Kubernetes sends SIGTERM, waits `terminationGracePeriodSeconds`, then sends SIGKILL. If the pod's grace period is shorter than statute's, you'll see KILLed processes losing in-flight requests.

## Containers

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /statute ./examples/basic

FROM gcr.io/distroless/static-debian12
COPY --from=builder /statute /statute
USER nonroot:nonroot
EXPOSE 80 443
ENTRYPOINT ["/statute"]
```

`distroless/static` is the smallest sensible base for a Go binary: no shell, no package manager, no /tmp clutter. The `nonroot` user is UID 65532. For low-port binding, use the setcap or sysctl approach on the host running the container, not inside the image.

If you're on Kubernetes, the container needs `securityContext.capabilities.add: [NET_BIND_SERVICE]` and `runAsUser: 65532`. Or use ports >= 1024 inside the container and remap externally with a Service.

## Reverse proxy chains

statute is fine as the front door, but it also works as a middle layer. Common patterns:

**Cloudflare → statute → backends**: see [docs/cloudflare.md](cloudflare.md). Use `BehindCloudflare()` for client IP attribution.

**ELB/ALB → statute → backends**: similar to the Cloudflare case, but trust `X-Forwarded-For` instead of `CF-Connecting-IP`. Declare the load balancer's address range as a trusted proxy on the listener — `TrustedProxy("10.0.0.0/8")` (the default header is `X-Forwarded-For`) — and the access log, rate limiter, IP lists, and IP-hash strategy key on the real client IP whenever the direct peer is the LB, while any other peer is attributed by its own address. Use static cert mode in this case (the load balancer terminates TLS).

**Service mesh sidecar (Envoy/Linkerd) → statute → backends**: don't. The sidecar already proxies. Either remove statute or remove the sidecar — running both is purely overhead.

**statute → another statute**: works fine, but suggests your routing topology is more complex than it needs to be. Reach for a single statute with named upstream pools instead of chaining instances.

## Observability checklist

Before considering a deployment "production":

- [ ] Access log destination is not stdout in production (parse, ship, retain). Use a structured log aggregator that can index `status`, `path`, `remote`.
- [ ] Metrics endpoint scraped by Prometheus or equivalent. Alerting on error rate (>1% 5xx over 5 minutes) and latency (p95 over SLO).
- [ ] Tracing exported to an OTel collector with at least 7 days of retention.
- [ ] Dashboards for: request rate, error rate, latency by route, upstream pool health.
- [ ] Runbook for common alerts: how to identify the bad backend, how to read the access log, how to inspect a trace.

## Security checklist

- [ ] `ReadHeaderTimeout` set in `Defaults`. Not zero. Slowloris mitigation is the most-skipped, most-impactful production setting in Go HTTP servers.
- [ ] Listener bound to a specific interface in single-tenant deployments (`192.0.2.10:443` rather than `:443`) so an unintended interface doesn't expose the proxy.
- [ ] Metrics listener private. Bind to loopback or a private VLAN. The `pprof` endpoints are debugging gold for an attacker.
- [ ] AutoTLS storage mode `0700`, owned by the statute user. The account key in there can issue arbitrary certs.
- [ ] Cloudflare API token (DNS-01 mode) in an env var or secrets manager, never in the binary or config.
- [ ] No secrets in the source. The Go config-as-code model makes this tempting; resist. Use env vars (`os.Getenv`) for tokens and credentials.
- [ ] CSP / HSTS / X-Frame-Options on responses where appropriate. statute doesn't set these by default; add a small middleware if your backends don't.
- [ ] Rate limiting on every public route. The token bucket is per-key; pick `ClientIP` (or `HostHeader` for tenant-isolation patterns) deliberately.

## Capacity planning

Some rough numbers from the standard library underneath:

- A single `http.Server` handles tens of thousands of concurrent idle connections without breaking a sweat.
- HTTP/2 connection pooling means the upstream transport can saturate a few hundred backend connections from a single statute instance.
- Active health checks add `n_backends * (1 / interval)` background requests. With 10 backends at 10s interval, that's 1 RPS — negligible.
- The token bucket rate limiter holds one struct per key in a `sync.Map`. At 1 million unique client IPs, expect roughly 50 MB of memory just for the buckets. Replace with a real LRU for high-cardinality deployments.

For real numbers, run a load test against your specific config. The Go runtime has a lot of variance based on GC pressure, GOMAXPROCS, and cgo invocation patterns; benchmarks from someone else's deployment rarely transfer.

## What we don't recommend

**Running statute under another reverse proxy that also terminates TLS**: pointless. Pick one TLS terminator. If it's Cloudflare, terminate at Cloudflare and use `BehindCloudflare()` here. If it's statute, point DNS at statute directly.

**Hot-swapping the binary**: Go's binary is immutable on disk while running. Replace + restart, with the new binary providing the same listener config so connections drain into the old process while the new one accepts. Use systemd, Kubernetes rolling updates, or a process manager that does the dance.

**Running multiple statute instances on one host without coordination**: each instance owns its own ACME account and storage. Two instances trying to issue for overlapping domains will race on the storage and confuse Let's Encrypt. Run one statute per host, or shard storage and domains carefully.

**Burying secrets in the binary**: the Go compiler does not encrypt strings. `strings yourbinary | grep -i token` finds anything you embed. Read secrets from env or a secrets manager at startup.
