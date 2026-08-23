# statute vs nginx, Caddy, Traefik

A comparison of statute against the three reverse proxies it most often shares a deployment role with. The matrix is honest about gaps — statute is small and deliberately scoped; it will not replace nginx for everyone.

## Headline differences

|                          | statute                 | nginx                         | Caddy                      | Traefik                                |
| ------------------------ | ----------------------- | ----------------------------- | -------------------------- | -------------------------------------- |
| Configuration language   | Go code                 | nginx.conf                    | Caddyfile / JSON           | YAML / TOML / file or label discovery  |
| Configuration loaded at  | Compile time            | Process start / SIGHUP        | Process start / API        | Continuous (file or service discovery) |
| Hot reload               | No (recompile + deploy) | Yes (SIGHUP / -s reload)      | Yes (built-in)             | Yes (continuous)                       |
| Binary                   | Single static           | Single binary + modules       | Single static              | Single static                          |
| Plugin / module loading  | No                      | Compile-time modules          | Custom modules (recompile) | Yes (provider plugins)                 |
| Admin API                | No                      | nginx-plus / stub_status only | Yes (REST)                 | Yes (REST)                             |
| Distributed coordination | No                      | No                            | No                         | Yes (with KV store)                    |

## Protocols and transport

|                        | statute                             | nginx              | Caddy          | Traefik |
| ---------------------- | ----------------------------------- | ------------------ | -------------- | ------- |
| HTTP/1.1               | ✅                                  | ✅                 | ✅             | ✅      |
| HTTP/2                 | ✅                                  | ✅                 | ✅             | ✅      |
| HTTP/3                 | ✅ (quic-go)                        | ✅ (since 1.25)    | ✅             | ✅      |
| WebSocket pass-through | ✅                                  | ✅                 | ✅             | ✅      |
| gRPC                   | ✅ (pass-through, gRPC-aware retry) | ✅                 | ✅             | ✅      |
| L4 (TCP/UDP)           | ❌                                  | ✅ (stream module) | ❌ (HTTP only) | ✅      |
| SNI routing            | ✅ (via host matching)              | ✅                 | ✅             | ✅      |

## TLS

|                  | statute                        | nginx                   | Caddy               | Traefik             |
| ---------------- | ------------------------------ | ----------------------- | ------------------- | ------------------- |
| Static certs     | ✅                             | ✅                      | ✅                  | ✅                  |
| ACME HTTP-01     | ✅ (autocert)                  | ❌ (external — certbot) | ✅                  | ✅                  |
| ACME TLS-ALPN-01 | ✅                             | ❌                      | ✅                  | ✅                  |
| ACME DNS-01      | ✅ (Cloudflare only, built-in) | ❌                      | ✅ (many providers) | ✅ (many providers) |
| Wildcard certs   | ✅ (via DNS-01)                | ❌ built-in             | ✅                  | ✅                  |
| OCSP stapling    | (autocert default)             | ✅                      | ✅                  | ✅                  |

## Routing and balancing

|                            | statute                   | nginx                   | Caddy       | Traefik                 |
| -------------------------- | ------------------------- | ----------------------- | ----------- | ----------------------- |
| Path / host / method match | ✅                        | ✅                      | ✅          | ✅                      |
| Regex path patterns        | ❌ (prefix wildcard only) | ✅                      | ✅          | ✅                      |
| Round-robin                | ✅                        | ✅                      | ✅          | ✅                      |
| Least-connections          | ✅                        | ✅                      | ✅          | ✅                      |
| IP-hash                    | ✅                        | ✅                      | ✅          | ✅                      |
| Smooth weighted RR         | ✅                        | ✅                      | ✅          | ✅                      |
| Backup tier failover       | ✅                        | ✅ (`backup`)           | ❌ (manual) | ❌ (sticky / mirroring) |
| Active health checks       | ✅                        | nginx-plus (commercial) | ✅          | ✅                      |
| Passive health checks      | ❌                        | ✅                      | ✅          | ✅                      |

## Middleware

|                           | statute                                      | nginx                           | Caddy                         | Traefik                                |
| ------------------------- | -------------------------------------------- | ------------------------------- | ----------------------------- | -------------------------------------- |
| Timeout (per route)       | ✅                                           | ✅                              | ✅                            | ✅                                     |
| Rate limit (per key)      | ✅ (token bucket; ClientIP/Host)             | ✅                              | ✅                            | ✅                                     |
| Retry on status           | ✅ (idempotent + gRPC-aware)                 | ✅                              | ✅                            | ✅                                     |
| Cache                     | ✅ (in-process TTL)                          | ✅                              | ✅                            | ❌ (delegates to upstream)             |
| Gzip / Brotli compression | ✅                                           | ✅ / ✅ (with module)           | ✅                            | ✅                                     |
| ETag                      | ✅                                           | ✅                              | ✅                            | ❌                                     |
| Body size limit           | ✅                                           | ✅ (`client_max_body_size`)     | ✅                            | ✅                                     |
| Basic auth (bcrypt)       | ✅                                           | ✅                              | ✅                            | ✅                                     |
| IP allow / deny           | ✅ (CIDR)                                    | ✅                              | ✅                            | ✅                                     |
| CORS                      | ✅ (preflight, credentialed-wildcard reject) | ✅                              | ✅ (handler)                  | ✅                                     |
| Security headers          | ✅                                           | ✅ (manual)                     | ✅                            | ✅                                     |
| Request ID propagation    | ✅                                           | ✅ (manual)                     | ✅                            | ✅                                     |
| Path rewrite              | ✅ (strip / add prefix, replace, regex)      | ✅ (`rewrite`)                  | ✅ (`rewrite`, `handle_path`) | ✅ (`StripPrefix`, `ReplacePathRegex`) |
| JWT validation            | ❌                                           | ✅ (nginx-plus or auth_request) | ✅ (jwt module)               | ✅ (middleware)                        |

## Observability

|                              | statute                  | nginx                  | Caddy | Traefik      |
| ---------------------------- | ------------------------ | ---------------------- | ----- | ------------ |
| Structured (JSON) access log | ✅                       | ✅ (custom log_format) | ✅    | ✅           |
| Sampled access log           | ✅                       | ❌ (manual)            | ❌    | ✅ (filters) |
| Prometheus metrics           | ✅                       | nginx-plus / exporter  | ✅    | ✅           |
| OpenTelemetry tracing        | ✅ (OTLP/gRPC)           | ✅ (1.27+)             | ✅    | ✅           |
| pprof                        | ✅ (on metrics listener) | ❌                     | ❌    | ❌           |

## When to choose statute

You should choose statute when **all** of these are true:

- Your team already builds and ships Go binaries.
- You want your reverse-proxy configuration to be type-checked, refactor-friendly Go code rather than nginx.conf / Caddyfile / YAML.
- The non-goals listed in the README do not bite you (no hot reload, no admin API, no plugins, no L4).
- You value a small dependency tree and a small binary.

## When to choose something else

- **nginx** is the right answer when you need maximum throughput, L4 streaming, the mature ecosystem of third-party modules (auth, geo, image processing), or when you already have nginx expertise on staff.
- **Caddy** is the right answer when you want automatic HTTPS for everything, a JSON config API, or when you appreciate the Caddyfile's terseness. CertMagic alone is worth the dependency for many users.
- **Traefik** is the right answer when you need dynamic service discovery (Docker, Kubernetes, Consul) without writing a deploy pipeline that re-renders config. Its labels-on-containers UX is unmatched.

## Performance

Honest answer: we do not yet have publishable numbers comparing statute to the alternatives under realistic load. The reverse-proxy hot path is dominated by Go's `net/http` and `httputil.ReverseProxy`, both of which are well-optimised; in microbenchmarks statute's per-request overhead is dominated by the access-log JSON encode and the middleware chain. The middleware framework is a slice walk, not a reflection-based dispatcher.

For deployments with strict latency budgets (sub-millisecond p99), benchmark on your own workload. For typical edge-proxy workloads, statute is fast enough that the bottleneck will be the upstream backend, not the proxy.
