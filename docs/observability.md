# Observability

statute exposes three observability channels: structured access logs, Prometheus metrics, and OpenTelemetry traces. They are independently optional but should all be enabled in production. This document covers what each emits, what to scrape and alert on, and how to size sample rates.

## Access log

```go
Observability: statute.Observability{
    AccessLog: statute.JSONLog(statute.Stdout),
}
```

One JSON line per selected request, written to the configured destination (`Stdout`, `Stderr`, or any `io.Writer` via the `LogWriter` type). Recording runs when the handler returns or unwinds, including streaming aborts.

### Fields

| Field           | Type   | Description                                                                                                                                  |
| --------------- | ------ | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `ts`            | string | Request start time, RFC 3339 with nanosecond precision, UTC.                                                                                 |
| `method`        | string | HTTP method.                                                                                                                                 |
| `host`          | string | Host header value as received.                                                                                                               |
| `path`          | string | URL path (no query).                                                                                                                         |
| `query`         | string | Raw query string (no leading `?`).                                                                                                           |
| `remote`        | string | Best-effort client IP. See "client IP attribution" below.                                                                                    |
| `user_agent`    | string | `User-Agent` header.                                                                                                                         |
| `referer`       | string | `Referer` header.                                                                                                                            |
| `status`        | int    | Committed response status, or zero if an aborted handler committed no final status.                                                          |
| `body_bytes`    | int64  | Body-byte counts returned by response-writer operations.                                                                                     |
| `aborted`       | bool   | Handler did not return normally, including proxy stream aborts.                                                                              |
| `body_error`    | bool   | A response-writer Write or ReadFrom operation returned an I/O error.                                                                         |
| `duration_us`   | int64  | Elapsed time from request start until handler return or panic unwinding. For a proxied upgrade, this spans the tunneled connection lifetime. |
| `proto`         | string | Protocol version, e.g. `HTTP/1.1`, `HTTP/2.0`.                                                                                               |
| `forwarded_for` | string | Raw `X-Forwarded-For` header (full chain, not parsed).                                                                                       |

### Client IP attribution

The `remote` field comes from `clientIP()`, which resolves in order:

1. If the listener declares a `TrustedProxy()` policy: the policy decides — a trusted direct peer speaks through the configured forwarded header, any other peer is its own client.
2. Otherwise, on a `BehindCloudflare()` listener: `CF-Connecting-IP`, then `True-Client-IP`.
3. Fallback: `r.RemoteAddr`.

`X-Forwarded-For` is never consulted without explicit trust configuration — it is client-controlled, and rate limiting, the IP lists, and client-IP route matching all key on this value. The raw header still lands in the `forwarded_for` log field, unparsed, for forensics.

### Sampling

```go
AccessLog: statute.JSONLog(statute.Stdout).Sample(0.1)
```

`Sample(rate)` records a fraction of successful (status < 400) requests. Within the status filter, 4xx/5xx responses, aborted handlers, and recorded body I/O errors always log. A stream aborted after a 200 header therefore remains visible at low sample rates.

Recommended rates by traffic volume:

- **<100 RPS**: `1.0` (no sampling). Log volume is manageable.
- **100–1000 RPS**: `0.1`. ~10x reduction.
- **>1000 RPS**: `0.01`. Combine with metrics for traffic shape; lean on tracing for individual-request visibility.

Sampling uses `math/rand/v2.Float64()`. The decision is independent per request, so the actual recorded fraction varies but converges to the rate over time.

### Status filtering

```go
AccessLog: statute.JSONLog(statute.Stdout).Statuses("400-499", "500-599")
```

`Statuses(...)` restricts the log to requests whose final status falls in one of the given inclusive ranges — `"400-499"`, or a single status like `"404"`. Sampling controls volume but cannot express error-only logging or exclude expected successful traffic deterministically; the status filter can.

The filter is a hard gate ahead of every other logging rule, including "errors are always logged":

```text
status range filter
↓ if allowed:
    >=400, aborted, or body I/O error → always log
    otherwise                       → sampling applies
```

So `Statuses("200-299")` really does suppress 500s — errors **within the selected ranges** are never sampled out, and everything outside the ranges is never logged at all. Filtering applies to the final status as committed to the client: the recorder ignores 1xx interim responses, so a 103 → 404 exchange filters as 404. Two commit edge cases are honoured — a `Flush` before any `WriteHeader` commits an implicit 200 (a later `WriteHeader(500)` cannot change what the client saw, so the filter sees 200), and 101 Switching Protocols is final, not interim, so a handler-written 101 filters as 101. A proxied upgrade takes the same path from the other side: the reverse proxy hijacks the connection and writes the 101 handshake directly to it, bypassing the response writer — but the recorder implements `Hijack` itself, so a successful hijack before any committed response latches 101. Proxied WebSocket upgrades therefore log and count as 101, and `Statuses("101")` matches them alongside handler-written 101s. The recorded duration still spans the whole tunneled connection lifetime, since the proxy's handler only returns when the tunnel closes.

`Resolve` rejects malformed or out-of-range (`[100, 599]`) inputs and normalizes the rest — sorted ascending, overlapping and adjacent ranges merged — and the canonical ranges appear in the exported schema.

A pre-header abort records status zero. Because status filters select HTTP status
codes, they exclude that outcome; omit the filter to retain pre-header failures.
Aborting after a committed 200 retains 200 and sets `aborted: true`. The abort
still reaches the HTTP server and terminates the response. Panic and I/O error
text are excluded from these fields.

Body-byte counts cover completed writer operations, including optimized body
copies. They measure bytes accepted by the writer, without claiming delivery or
client acknowledgement. They exclude headers and data written through hijacked
connections. Compression is counted at the listener's outer writer. An upstream
read failure can produce an abort without a writer I/O error; the two outcome
signals overlap and must not be added as disjoint error counts.

## Metrics

```go
Observability: statute.Observability{
    Metrics: statute.Prometheus(":9090", "/metrics"),
}
```

Prometheus exposition format on a separate listener. The metrics listener is intended to be **private** — bind it to a loopback address or a private interface and scrape it from your monitoring system. Do not expose it publicly: the same unauthenticated listener exposes `pprof` and workload diagnostics (see below). Statute does not enforce a private bind address.

For containers, distinguish the listener inside the container from the published
host port. The development Compose example keeps the container listener on
`:9090` and publishes `127.0.0.1:9090:9090`, reachable on the host at
`http://127.0.0.1:9090/metrics`. This limits host publication to IPv4 loopback;
containers sharing its network can still reach the listener. Use a current Docker
Engine: releases before 28.0.0 have a documented same-network exception to
[localhost port isolation](https://docs.docker.com/engine/network/port-publishing/).

### Metric names

| Name                                                | Type    | Description                                                                                                               |
| --------------------------------------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------- |
| `statute_requests_total`                            | counter | Total HTTP requests handled across all listeners.                                                                         |
| `statute_requests_by_status_total{status="..."}`    | counter | Requests broken down by response status code.                                                                             |
| `statute_request_duration_microseconds_sum`         | counter | Sum of request durations in microseconds.                                                                                 |
| `statute_request_duration_microseconds_count`       | counter | Count of observed requests.                                                                                               |
| `statute_response_body_bytes_total`                 | counter | Sum of response-writer body-byte counts.                                                                                  |
| `statute_requests_aborted_total`                    | counter | Handlers that did not return normally.                                                                                    |
| `statute_response_body_errors_total`                | counter | Responses with a recorded writer/copy I/O error.                                                                          |
| `statute_docker_workload_activations_total`         | counter | Accepted activation/readiness attempts, including observe-only adoption.                                                  |
| `statute_docker_workload_activation_failures_total` | counter | Failed start/readiness attempts; excludes cancellation, supersession, and stale observe-only stopped observations.        |
| `statute_docker_workload_idle_stops_total`          | counter | Successfully settled idle shutdowns; not retries, rejections, or cleanup stops.                                           |
| `statute_docker_workload_external_starts_total`     | counter | Running observations initiating adoption/repair readiness, including initial discovery.                                   |
| `statute_docker_workload_external_stops_total`      | counter | Observed stops of ready or stop-pending workloads, not repeated listings or owned-stop settlement.                        |
| `statute_docker_workload_phase`                     | gauge   | Current owner phase: dormant=0, starting=1, ready=2, stop-pending=3, stop-issued=4, stop-unknown=5, failed=6.             |
| `statute_docker_workload_waiters`                   | gauge   | Requests waiting for the current activation; excludes issued-stop waiters.                                                |
| `statute_docker_workload_last_activation_seconds`   | gauge   | Current incarnation's last completed activation/readiness duration, including cancelled attempts; zero before completion. |
| `statute_docker_workload_retired`                   | gauge   | Whether the current registered owner has lost lifecycle authority (0 or 1).                                               |
| `statute_docker_workload_retired_owners`            | gauge   | Retained retired owners, including unresolved predecessor mutations.                                                      |
| `statute_docker_workload_orphaned_mutations`        | gauge   | Recovered mutation owners without a currently configured service; no labels.                                              |

Average request duration is `sum / count`. Histogram buckets are not exported — for percentile queries use OpenTelemetry tracing or a richer metrics backend. The current metrics surface is intentionally minimal; deployments that need more should swap the in-process `stats` for the [prometheus/client_golang](https://github.com/prometheus/client_golang) library and define their own histograms.

Workload series have only a `service` label drawn from the compiled `Docker().Workload(...)` map, except the unlabelled orphan count. There are no container-ID, incarnation, phase-text, or error-text labels. Service counters start at zero and accumulate for the provider object's lifetime, including provider stop/start within one process. They reset when that object/process is replaced and are not persisted. Container replacement and removal of settled retired owners never decrease them. A recovered idle stop that successfully settles counts in this process if its service remains configured; recovery does not invent historical activation counts.

The phase, waiter, duration, and retired gauges describe only the current registered owner. They are absent before an owner exists; a retained retired predecessor never emits duplicate current-owner samples. `retired_owners` counts all retained retired owners, including a retired current entry. External transition counters describe observations, not which actor caused them. Existing request metrics retain their request-level semantics and do not count auxiliary metrics/diagnostic reads.

### Workload snapshots

`GET /debug/workloads` (also `HEAD`) returns JSON on the metrics listener whenever `Prometheus(...)` is enabled. It is not mounted on content or health listeners and has `Cache-Control: no-store`. The exact path is reserved: a conflicting metrics pattern is a resolve/construction error, not a startup panic. Other custom metrics paths, including `/` or a subtree, continue to work; the exact diagnostic handler takes precedence.

The response has `services` and `orphaned_mutations` arrays, both empty when Docker is absent. Each configured service has `service`, cumulative `activation_attempts`, `activation_failures`, `idle_stops`, `external_starts`, `external_stops`, and an `owners` array. An owner contains:

- `incarnation`: opaque provider-local binding identity, not a Docker container ID; do not correlate it across processes.
- `current`: whether this is the service's registered owner rather than a detached predecessor.
- `phase`, `retired`, and `activation_waiters`: a coherent copy of current lifecycle state.
- `last_activation_seconds`: the last completed current-incarnation attempt's duration, not an in-progress timer.
- `last_failure_reason` and optional `last_failure_at` (UTC RFC 3339): the last actual failure; success preserves that history until the binding changes.

Failure reasons are a closed set: `start-failed`, `start-timeout`, `readiness-timeout`, `stopped-before-ready`, or `activation-failed`; an empty string means no recorded failure. Raw Docker errors, endpoints, container names/IDs, labels, backend addresses, credentials, and policy objects are never included. The configured service name is the only service identity exposed. Restored owners with no current code-owned service grant appear only in `orphaned_mutations`, without their historical service name.

Generation replacement and retirement/regrant of the same binding preserve incarnation details. A different container gets fresh duration/failure details even when its service name is reused. An unresolved predecessor remains separately visible until canonical settlement and subsequent reconciliation remove it; counters survive that removal. Snapshots copy registry membership, owner state, and service totals under their owning locks, then encode without holding lifecycle locks. Reads never trigger Docker calls, readiness, reconciliation, or lifecycle transitions. Snapshots are live diagnostics, not a durable history; the metrics listener drains before provider shutdown completes.

### What to alert on

The minimum-useful alert set:

- **Workload failures**: increasing `statute_docker_workload_activation_failures_total`, sustained activation waiters, or phase `5` (unresolved stop). Inspect `/debug/workloads` for safe reasons and retired owners. A nonzero `statute_docker_workload_orphaned_mutations` means recovered ownership still needs convergence despite the removed grant.
- **Error rate**: `rate(statute_requests_by_status_total{status=~"5.."}[5m]) / rate(statute_requests_total[5m]) > 0.01`. Page when 5xx exceeds 1% over a 5-minute window.
- **Availability of upstreams**: best detected from access log (502 spike) since active health checks demote silently. Pair with backend-side metrics if you have them.
- **Latency**: `rate(statute_request_duration_microseconds_sum[5m]) / rate(statute_request_duration_microseconds_count[5m])` against a per-route SLO. Average latency is a weak signal — prefer p95/p99 from traces.

### pprof

The metrics listener also serves Go's standard pprof endpoints:

- `/debug/pprof/` — index
- `/debug/pprof/profile` — CPU profile (?seconds=30 for length)
- `/debug/pprof/heap` — heap snapshot
- `/debug/pprof/goroutine` — goroutine stacks
- `/debug/pprof/trace` — execution trace
- `/debug/pprof/cmdline`, `/debug/pprof/symbol`

Use `go tool pprof http://localhost:9090/debug/pprof/profile` for live profiling. Because pprof and metrics share a listener, the same "do not expose publicly" warning applies.

## Health endpoint

Health and metrics listeners inherit `Defaults.ReadHeaderTimeout`, `ReadTimeout`,
`WriteTimeout`, `IdleTimeout`, and `MaxHeaderBytes`, just like content listeners.
The defaults include a five-second header timeout, 30-second write timeout,
120-second idle timeout and 1 MiB header limit. `ReadTimeout` defaults to zero;
configure a nonzero value when incomplete request bodies must time out. Explicit
zero durations retain their normal Go HTTP server semantics. The standard Go
CPU, trace and delta-profile handlers extend a positive write deadline by the
requested collection duration, so longer profiles do not require increasing
`WriteTimeout` just to cover collection time.

```go
Observability: statute.Observability{
    Health: statute.Health(":8081", "/healthz"),
}
```

A dedicated process health listener for supervisors (Kubernetes probes, systemd watchdogs, load balancers). It serves exactly two paths and nothing else, with no metrics and no pprof:

- **Liveness** at the configured path (default `/healthz` when the path is empty): `200 "ok"` for the whole time the process runs.
- **Readiness** at the configured path plus `/ready` (e.g. `/healthz/ready`): `200 "ok"` once startup has committed, `503 "not ready"` otherwise.

Path matching is exact: only those two paths answer, and any other path — including the configured path with a trailing slash or anything below it — returns 404. The configured path must start with a single `/` (a `//` or `/\` opening is rejected too), must not be `/` itself, and must not end with `/`; `Resolve` rejects other shapes.

The health listener **brackets** the application's availability. `Start` binds it and begins answering first, before certificate managers start, before the initial Docker sync, and before any other socket binds — so during the entire startup phase probes read liveness `200` and readiness `503 "not ready"`. Readiness flips to `200` only when startup commits: every listener socket bound, certificate managers started, and the initial Docker sync (when configured) complete. It does **not** wait for asynchronous HTTP-01 certificate warm-up: that runs in the background after startup, and a slow or unreachable CA must not keep an otherwise-serving process out of rotation. A failed `Start` fully tears the health listener down again — socket released, serve goroutine stopped — and a retried `Start` serves health afresh.

On `Shutdown`, readiness flips to `503` as the very first action, and the health listener closes **last** — only after the content and metrics listeners have finished draining. Probes therefore keep receiving answers (liveness `200`, readiness `503`) for the whole grace period rather than refused connections; the health port refuses only once the process is done.

Like the metrics listener, the health listener is intended to be **private**: bind it to a loopback address or a private interface. It deliberately serves plain-text `ok` / `not ready` bodies with no version or subsystem detail, but a health port is still an internal surface: do not expose it publicly.

## Tracing

```go
Observability: statute.Observability{
    Tracing: statute.OTLP("otel-collector:4317").
        ServiceName("edge-proxy").
        Insecure().
        Sample(0.05),
}
```

OTLP/gRPC export to an OpenTelemetry collector. Spans use HTTP semantic conventions via [otelhttp](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp). W3C trace context is automatically extracted from incoming requests and injected into outgoing reverse-proxy requests, so traces continue across hops if your backends are also instrumented.

### Span structure

For a single proxied request you'll see:

```text
statute.request                          [server span, root or child of incoming traceparent]
└── HTTP GET                             [client span, the proxy call to the backend]
    └── (backend's spans, if instrumented)
```

The server span's name is `<method> <path>` (e.g. `GET /api/v1/users`). HTTP semantic-convention attributes are populated automatically: `http.method`, `http.target`, `http.status_code`, `http.user_agent`, `http.host`, `net.peer.ip`, `net.peer.port`. The HTTP version (`http.flavor`) reflects the listener: 1.1, 2, or 3.

### Sampling

`Sample(rate)` uses `TraceIDRatioBased` with `ParentBased` outer sampling. This means:

- **Roots**: a fresh trace is sampled at `rate`. With `0.05`, 5% of root requests get traced.
- **Continuations**: a request that arrives with an existing `traceparent` honours the parent's `sampled` flag. If the upstream sampled, this hop samples too; if not, it doesn't.

This preserves trace continuity. If you trace 5% at the edge and 100% at a backend service, the backend honours the edge's decision — you never see a half-traced request where the edge span is missing. Conversely, an internal client that always samples its requests will produce fully-sampled traces through your statute proxy regardless of statute's own rate.

Recommended rates:

- **Development**: `1.0`. Trace everything.
- **Production, high-traffic**: `0.01–0.05`. Combined with parent-based sampling, errors and slow paths stay traceable when upstream services raise their sample rate for affected requests.
- **Production, low-traffic**: `0.1–1.0`. The collector cost is the limiting factor; a few thousand traces per second is comfortable for most setups.

### Resource attributes

Spans are tagged with:

- `service.name` — from `ServiceName()`. Defaults to `"statute"`.
- `statute.version` — currently `"0.1.0"`, baked into the binary.
- Process attributes (PID, runtime.name, runtime.version) via `resource.WithProcess()`.
- Telemetry SDK attributes (otel.library.name, otel.library.version) automatically.
- Anything in `OTEL_RESOURCE_ATTRIBUTES` env var (read by `resource.WithFromEnv()`).

The `OTEL_RESOURCE_ATTRIBUTES` channel is the canonical way to inject deployment-specific attributes (`environment=prod`, `region=us-east-1`, `host=edge-01`) without recompiling.

### Endpoint format

The `OTLP("...")` argument is a host:port for the gRPC OTLP collector. No scheme: gRPC clients don't take URLs. Examples:

- `OTLP("otel-collector:4317")` — typical Kubernetes sidecar/sibling
- `OTLP("localhost:4317")` — agent on the same host
- `OTLP("api.honeycomb.io:443")` — direct to a managed backend

Use `Insecure()` only when the collector is on a trusted network (sidecar, in-cluster). Managed backends always require TLS.

### Graceful shutdown

OTel's batch span processor flushes pending spans at process exit. statute calls `tp.Shutdown(ctx)` during graceful shutdown, with the same `Shutdown.GracePeriod` as the listeners. Spans queued during the last few seconds before SIGTERM are exported reliably.

If the collector is unreachable at shutdown time, the flush blocks until the grace period expires, then the process exits with the spans dropped. Alarming on collector availability matters: a chronic collector outage causes shutdowns to hang, which delays deployments.

## Combining the channels

A typical production deployment has:

- **Access log** at sample rate `0.1`, sent to a log aggregator (Loki, Cloud Logging, Logstash). Used for ad-hoc investigation and request-level forensics.
- **Metrics** scraped at 15-second intervals by Prometheus. Used for dashboards and alerts.
- **Tracing** at sample rate `0.05`, exported to an OTel collector that fans out to Honeycomb / Tempo / Jaeger. Used for latency analysis and dependency mapping.

Errors are visible in all three: access log captures every 4xx/5xx unconditionally, metrics counts them by status, and traces capture them via parent-based sampling. The redundancy is intentional — losing one channel still leaves error visibility intact.
